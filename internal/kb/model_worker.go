package kb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const maximumModelWorkerLineBytes = 1 << 20

// PythonRetrievalModels adapts the pinned local SentenceTransformers models
// through one private JSON-lines child process. It never downloads models.
type PythonRetrievalModels struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	mu      sync.Mutex
	closed  bool
}

type modelWorkerRequest struct {
	Action string   `json:"action"`
	Query  string   `json:"query"`
	Texts  []string `json:"texts,omitempty"`
}

type modelWorkerResponse struct {
	OK        bool      `json:"ok"`
	ErrorCode string    `json:"error_code,omitempty"`
	Result    []float64 `json:"result,omitempty"`
}

// StartPythonRetrievalModels launches the model adapter with the workspace's
// src package on PYTHONPATH. The model files must already be cached locally.
func StartPythonRetrievalModels(ctx context.Context, python string) (*PythonRetrievalModels, error) {
	if strings.TrimSpace(python) == "" {
		return nil, errors.New("KB model Python executable is empty")
	}
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, errors.New("KB model package location could not be resolved")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	cmd := exec.CommandContext(ctx, python, "-m", "bb_assistant.kb.model_worker")
	cmd.Dir = root
	pythonPath := filepath.Join(root, "src")
	env := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "PYTHONPATH=") {
			if inherited := strings.TrimPrefix(item, "PYTHONPATH="); inherited != "" {
				pythonPath += string(os.PathListSeparator) + inherited
			}
			continue
		}
		env = append(env, item)
	}
	cmd.Env = append(env, "PYTHONPATH="+pythonPath)
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("KB model worker input could not be opened")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, errors.New("KB model worker output could not be opened")
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.New("KB model worker could not be started")
	}
	return &PythonRetrievalModels{command: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}, nil
}

func (m *PythonRetrievalModels) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	response, err := m.call(ctx, modelWorkerRequest{Action: "embed_query", Query: query})
	if err != nil {
		return nil, err
	}
	values := make([]float32, len(response.Result))
	for n, value := range response.Result {
		values[n] = float32(value)
	}
	return values, nil
}

func (m *PythonRetrievalModels) Rerank(ctx context.Context, query string, texts []string) ([]float64, error) {
	response, err := m.call(ctx, modelWorkerRequest{Action: "rerank", Query: query, Texts: texts})
	if err != nil {
		return nil, err
	}
	return response.Result, nil
}

func (m *PythonRetrievalModels) call(ctx context.Context, request modelWorkerRequest) (modelWorkerResponse, error) {
	if m == nil {
		return modelWorkerResponse{}, errors.New("KB model worker is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.stdin == nil || m.stdout == nil {
		return modelWorkerResponse{}, errors.New("KB model worker is closed")
	}
	if err := ctx.Err(); err != nil {
		return modelWorkerResponse{}, err
	}
	requestLine, err := json.Marshal(request)
	if err != nil || len(requestLine) >= maximumModelWorkerLineBytes {
		return modelWorkerResponse{}, errors.New("KB model request exceeds its size limit")
	}
	requestLine = append(requestLine, '\n')
	if _, err := m.stdin.Write(requestLine); err != nil {
		return modelWorkerResponse{}, errors.New("KB model worker request could not be written")
	}
	stopKill := context.AfterFunc(ctx, func() {
		if m.command.Process != nil {
			_ = m.command.Process.Kill()
		}
	})
	line, err := m.stdout.ReadBytes('\n')
	stopKill()
	if err != nil || len(line) > maximumModelWorkerLineBytes {
		return modelWorkerResponse{}, errors.New("KB model worker response could not be read")
	}
	if err := ctx.Err(); err != nil {
		return modelWorkerResponse{}, err
	}
	var response modelWorkerResponse
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || !response.OK {
		return modelWorkerResponse{}, errors.New("KB model inference is unavailable")
	}
	return response, nil
}

func (m *PythonRetrievalModels) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	if m.stdin != nil {
		_ = m.stdin.Close()
	}
	if m.command == nil {
		return nil
	}
	if err := m.command.Wait(); err != nil {
		// A model worker may have exited after a failed local model lookup.
		return nil
	}
	return nil
}
