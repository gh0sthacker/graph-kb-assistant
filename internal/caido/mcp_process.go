package caido

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const expectedServerVersion = "v4.3.0"

var readTools = map[string]struct{}{
	"caido_list_projects":  {},
	"caido_select_project": {},
	"caido_list_requests":  {},
	"caido_get_request":    {},
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type MCPProcessConfig struct {
	Context        context.Context
	Command        string
	ExpectedSHA256 string
	TokenFile      string
	StartupTimeout time.Duration
	CallTimeout    time.Duration
}

type MCPProcess struct {
	mu         sync.Mutex
	ctx        context.Context
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	lines      chan processLine
	readerDone chan struct{}
	closed     bool
	nextID     uint64
	timeout    time.Duration
}

type processLine struct {
	line []byte
	err  error
}

var errMCPLineTooLarge = errors.New("Caido MCP response line exceeds its size limit")

type jsonRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

func StartMCPProcess(cfg MCPProcessConfig) (*MCPProcess, error) {
	processContext := cfg.Context
	if processContext == nil {
		processContext = context.Background()
	}
	command := cfg.Command
	if command == "" {
		command = os.Getenv("BB_ASSISTANT_CAIDO_MCP_COMMAND")
	}
	if command == "" {
		command = "caido-mcp-server"
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 30 * time.Second
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = 180 * time.Second
	}
	resolved, err := verifyExecutable(command, cfg.ExpectedSHA256)
	if err != nil {
		return nil, err
	}
	env, err := childEnvironment(cfg.TokenFile)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(resolved, "serve")
	cmd.Env = env
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, sanitized("Could not open the Caido MCP input stream")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, sanitized("Could not open the Caido MCP output stream")
	}
	if err := cmd.Start(); err != nil {
		return nil, sanitized("Could not start the configured Caido MCP stdio server")
	}
	client := &MCPProcess{
		ctx: processContext, cmd: cmd, stdin: stdin, lines: make(chan processLine, 16),
		readerDone: make(chan struct{}), timeout: cfg.StartupTimeout,
	}
	go client.readLoop(stdout)
	initialized, err := client.rpc("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name": "bb-assistant-caido-reader", "version": "0.1.0",
		},
	})
	clear(initialized)
	if err != nil {
		client.Close()
		return nil, sanitized("Could not initialize the Caido MCP stdio server")
	}
	if err := client.notify("notifications/initialized", map[string]any{}); err != nil {
		client.Close()
		return nil, sanitized("Could not initialize the Caido MCP stdio server")
	}
	if err := client.discoverReadTools(); err != nil {
		client.Close()
		return nil, err
	}
	client.mu.Lock()
	client.timeout = cfg.CallTimeout
	client.mu.Unlock()
	return client, nil
}

func (c *MCPProcess) readLoop(stdout io.Reader) {
	defer close(c.readerDone)
	reader := bufio.NewReaderSize(stdout, 64*1024)
	const maxLine = 16 << 20
	for {
		line, err := readBoundedLine(reader, maxLine)
		if errors.Is(err, errMCPLineTooLarge) {
			sendProcessLine(c.lines, processLine{err: err})
			return
		}
		if len(line) != 0 {
			trimmed := bytes.TrimSpace(line)
			queuedLine := append([]byte(nil), trimmed...)
			select {
			case c.lines <- processLine{line: queuedLine}:
			case <-time.After(2 * time.Second):
				clear(queuedLine)
				clear(line)
				return
			}
			clear(line)
		}
		if err != nil {
			if err != io.EOF {
				sendProcessLine(c.lines, processLine{err: err})
			} else {
				sendProcessLine(c.lines, processLine{err: io.EOF})
			}
			return
		}
	}
}

func sendProcessLine(lines chan<- processLine, item processLine) bool {
	select {
	case lines <- item:
		return true
	case <-time.After(2 * time.Second):
		clear(item.line)
		return false
	}
}

func readBoundedLine(reader *bufio.Reader, maxBytes int) ([]byte, error) {
	line := make([]byte, 0, 64*1024)
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > maxBytes {
			clear(line)
			return nil, errMCPLineTooLarge
		}
		line = append(line, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

func (c *MCPProcess) discoverReadTools() error {
	result, err := c.rpc("tools/list", map[string]any{})
	if err != nil {
		return sanitized("Could not inspect Caido MCP tool capabilities")
	}
	defer clear(result)
	var payload struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if json.Unmarshal(result, &payload) != nil {
		return sanitized("Caido MCP tool catalog has an unsupported shape")
	}
	available := make(map[string]struct{}, len(payload.Tools))
	for _, tool := range payload.Tools {
		available[tool.Name] = struct{}{}
	}
	for name := range readTools {
		if _, ok := available[name]; !ok {
			return sanitized("Caido MCP server is missing a required history read tool")
		}
	}
	return nil
}

// CallTool only exposes the fixed Caido history read allowlist.
func (c *MCPProcess) CallTool(name string, arguments map[string]any) (map[string]any, error) {
	return c.CallToolContext(c.ctx, name, arguments)
}

// CallToolContext supports bounded cleanup calls after a user cancellation,
// such as restoring the Caido project that was active before a read.
func (c *MCPProcess) CallToolContext(ctx context.Context, name string, arguments map[string]any) (map[string]any, error) {
	if _, ok := readTools[name]; !ok {
		return nil, errors.New("The Caido import bridge permits history read tools only")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	result, err := c.rpcContext(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		return nil, ErrMCP
	}
	defer clear(result)
	var envelope map[string]any
	if json.Unmarshal(result, &envelope) != nil {
		releaseDecodedMCPValue(envelope)
		return nil, sanitized("Caido MCP result has an unsupported shape")
	}
	if isError, ok := envelope["isError"].(bool); ok && isError {
		releaseDecodedMCPValue(envelope)
		return nil, sanitized("Caido MCP call returned an error")
	}
	return envelope, nil
}

func (c *MCPProcess) rpc(method string, params any) (json.RawMessage, error) {
	return c.rpcContext(c.ctx, method, params)
}

func (c *MCPProcess) rpcContext(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.stdin == nil {
		return nil, errors.New("MCP process closed")
	}
	c.nextID++
	id := c.nextID
	idRaw := json.RawMessage(strconv.FormatUint(id, 10))
	request, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		return nil, sanitized("Could not encode Caido MCP request")
	}
	if _, err := c.stdin.Write(append(request, '\n')); err != nil {
		return nil, sanitized("Caido MCP stdio server disconnected")
	}
	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	for {
		select {
		case item := <-c.lines:
			if item.err != nil {
				clear(item.line)
				return nil, sanitized("Caido MCP stdio server disconnected")
			}
			var response jsonRPCMessage
			if json.Unmarshal(item.line, &response) != nil || response.JSONRPC != "2.0" {
				clear(item.line)
				clearJSONRPCMessage(&response)
				return nil, sanitized("Caido MCP stdio response has an unsupported shape")
			}
			if len(response.ID) == 0 || !bytes.Equal(bytes.TrimSpace(response.ID), idRaw) {
				// MCP notifications have no ID and are ignored. A response with
				// another ID cannot satisfy the current serialized request.
				clear(item.line)
				clearJSONRPCMessage(&response)
				continue
			}
			if response.Error != nil {
				clear(item.line)
				clearJSONRPCMessage(&response)
				return nil, sanitized("Caido MCP JSON-RPC request failed")
			}
			if len(response.Result) == 0 {
				clear(item.line)
				clearJSONRPCMessage(&response)
				return nil, sanitized("Caido MCP JSON-RPC result is missing")
			}
			result := append(json.RawMessage(nil), response.Result...)
			clear(item.line)
			clearJSONRPCMessage(&response)
			return result, nil
		case <-timer.C:
			return nil, sanitized("Caido MCP history read timed out")
		case <-ctx.Done():
			return nil, sanitized("Caido MCP history read was interrupted")
		}
	}
}

func (c *MCPProcess) notify(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.stdin == nil || c.ctx.Err() != nil {
		return errors.New("MCP process closed")
	}
	request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(request, '\n'))
	return err
}

func (c *MCPProcess) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	cmd := c.cmd
	c.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	if c.readerDone != nil {
		<-c.readerDone
	}
	for {
		select {
		case item := <-c.lines:
			clear(item.line)
		default:
			return
		}
	}
}

func clearJSONRPCMessage(message *jsonRPCMessage) {
	if message == nil {
		return
	}
	clear(message.ID)
	clear(message.Params)
	clear(message.Result)
	message.JSONRPC = ""
	message.Method = ""
	if message.Error != nil {
		clear(message.Error.Data)
		message.Error.Message = ""
	}
}

func releaseDecodedMCPValue(value any) {
	switch item := value.(type) {
	case map[string]any:
		for _, child := range item {
			releaseDecodedMCPValue(child)
		}
		clear(item)
	case []any:
		for _, child := range item {
			releaseDecodedMCPValue(child)
		}
		clear(item)
	}
}

func verifyExecutable(command, expected string) (string, error) {
	resolved, err := exec.LookPath(command)
	if err != nil {
		return "", sanitized("Configured Caido MCP server executable was not found")
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", sanitized("Configured Caido MCP server path is unavailable")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return "", sanitized("Configured Caido MCP server is not an executable file")
	}
	if expected == "" {
		expected = os.Getenv("BB_ASSISTANT_CAIDO_MCP_SHA256")
	}
	expected = strings.ToLower(strings.TrimSpace(expected))
	if !sha256Pattern.MatchString(expected) {
		return "", sanitized("Set BB_ASSISTANT_CAIDO_MCP_SHA256 to the verified SHA-256 of the pinned v4.3.0 binary")
	}
	f, err := os.Open(resolved)
	if err != nil {
		return "", sanitized("Configured Caido MCP server path is unavailable")
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), expected) {
		return "", sanitized("Configured Caido MCP server SHA-256 does not match the pinned binary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, resolved, "--version")
	probe.Env = minimalProbeEnvironment()
	output, err := probe.Output()
	if err != nil {
		return "", sanitized("Could not verify the Caido MCP server version")
	}
	parts := strings.Fields(string(output))
	if len(parts) == 0 || strings.TrimPrefix(parts[len(parts)-1], "v") != strings.TrimPrefix(expectedServerVersion, "v") {
		return "", fmt.Errorf("Caido MCP server must report version %s", expectedServerVersion)
	}
	return resolved, nil
}

func minimalProbeEnvironment() []string {
	keys := []string{"PATH", "SYSTEMROOT"}
	var env []string
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func childEnvironment(tokenFile string) ([]string, error) {
	keys := []string{"PATH", "HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME", "SYSTEMROOT", "CAIDO_URL", "CAIDO_ACCESS_TOKEN"}
	env := make([]string, 0, len(keys)+2)
	hasToken := false
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok && value != "" {
			env = append(env, key+"="+value)
			if key == "CAIDO_ACCESS_TOKEN" {
				hasToken = true
			}
		}
	}
	if !hasToken {
		explicitTokenFile := tokenFile != "" || os.Getenv("BB_ASSISTANT_CAIDO_TOKEN_FILE") != ""
		if tokenFile == "" {
			tokenFile = os.Getenv("BB_ASSISTANT_CAIDO_TOKEN_FILE")
		}
		if tokenFile == "" {
			if home, err := os.UserHomeDir(); err == nil {
				tokenFile = filepath.Join(home, ".caido-mcp", "token.json")
			}
		}
		if tokenFile != "" {
			if _, err := os.Stat(tokenFile); err == nil {
				token, err := readAccessToken(tokenFile)
				if err != nil {
					return nil, err
				}
				env = append(env, "CAIDO_ACCESS_TOKEN="+token)
			} else if explicitTokenFile {
				return nil, sanitized("Configured Caido token file is unavailable")
			}
		}
	}
	if os.Getenv("CAIDO_URL") == "" {
		env = append(env, "CAIDO_URL=http://127.0.0.1:8080")
	}
	env = append(env, "CAIDO_ALLOW_SENSITIVE_HEADERS=true")
	return env, nil
}

func readAccessToken(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", sanitized("Caido token file is not a regular file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return "", sanitized("Caido token file permissions are not private")
	}
	currentUser, userErr := user.Current()
	stat, statOK := info.Sys().(*syscall.Stat_t)
	if userErr != nil || !statOK || strconv.FormatUint(uint64(stat.Uid), 10) != currentUser.Uid {
		return "", sanitized("Caido token file is not owned by the service user")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", sanitized("Caido token file cannot be read")
	}
	var payload struct {
		AccessToken string `json:"accessToken"`
		ExpiresAt   string `json:"expiresAt"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.AccessToken == "" || payload.ExpiresAt == "" || strings.ContainsAny(payload.AccessToken, "\r\n\x00") {
		return "", sanitized("Caido token file has an unsupported format")
	}
	expiry, err := time.Parse(time.RFC3339Nano, payload.ExpiresAt)
	if err != nil {
		return "", sanitized("Caido token expiry is invalid")
	}
	if !expiry.After(time.Now().Add(5 * time.Minute)) {
		return "", sanitized("Caido access token is expired or expiring; renew the Caido login")
	}
	return payload.AccessToken, nil
}
