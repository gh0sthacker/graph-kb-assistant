package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"bb-graph-assistant/internal/ingest"
	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/store"
)

func TestCaidoTailCLIEmptyPageExit(t *testing.T) {
	if os.Getenv("BB_TAIL_TEST_CHILD") == "1" {
		os.Args = append([]string{os.Args[0]}, strings.Split(os.Getenv("BB_TAIL_TEST_ARGS"), "\n")...)
		main()
		os.Exit(0)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for the stdio MCP fixture")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"empty", "transport", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			ref := project.CaidoProjectRef{InstanceID: "fixture-instance", ProjectID: "fixture-project"}
			if err := project.RegisterProject(ref, root); err != nil {
				t.Fatal(err)
			}
			selected, err := store.CreateProject(context.Background(), ref, root)
			if err != nil {
				t.Fatal(err)
			}
			if err := selected.Close(); err != nil {
				t.Fatal(err)
			}
			key, err := ref.StorageKey()
			if err != nil {
				t.Fatal(err)
			}
			script := "#!" + python + "\n" + strings.ReplaceAll(emptyTailMCPFixture, "FIXTURE_MODE", mode)
			mcp := filepath.Join(root, "mcp-fixture")
			if err := os.WriteFile(mcp, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte(script))
			args := []string{"caido-tail", "--project-key", key, "--data-root", root, "--mcp-command", mcp, "--mcp-sha256", hex.EncodeToString(digest[:]), "--max-pages", "10", "--require-scope", "--scope-scheme", "http", "--scope-host", "fixture.invalid", "--scope-port", "3000"}
			cmd := exec.Command(self, "-test.run=^TestCaidoTailCLIEmptyPageExit$")
			cmd.Env = append(os.Environ(), "BB_TAIL_TEST_CHILD=1", "BB_TAIL_TEST_ARGS="+strings.Join(args, "\n"), "BB_ASSISTANT_VAULT_KEY="+base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)), "CAIDO_ACCESS_TOKEN=synthetic-fixture-token")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err = cmd.Run()
			var result ingest.LiveTailResult
			if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
				t.Fatalf("decode result: %v; stdout=%s stderr=%s err=%v", decodeErr, &stdout, &stderr, err)
			}
			if mode == "empty" {
				if err != nil || result.State != "complete" || result.CursorPending || result.ResponseRefreshPending {
					t.Fatalf("empty page: want exit 0 and complete, got result=%+v err=%v stderr=%s", result, err, &stderr)
				}
			} else {
				exit, ok := err.(*exec.ExitError)
				if !ok || exit.ExitCode() != 2 || result.State != "complete" {
					t.Fatalf("%s failure: want exit 2 and complete when no work remains, got result=%+v err=%v stderr=%s", mode, result, err, &stderr)
				}
			}
		})
	}
}

const emptyTailMCPFixture = `import json, sys
if sys.argv[1:] == ["--version"]:
    print("caido-mcp-server v4.3.0")
    sys.exit(0)
for line in sys.stdin:
    message = json.loads(line)
    if "id" not in message:
        continue
    method = message["method"]
    if method == "initialize":
        result = {"protocolVersion": "2024-11-05", "capabilities": {}, "serverInfo": {"name": "fixture", "version": "4.3.0"}}
    elif method == "tools/list":
        result = {"tools": [{"name": name} for name in ["caido_list_projects", "caido_select_project", "caido_list_requests", "caido_get_request"]]}
    elif method == "tools/call":
        name = message["params"]["name"]
        if name == "caido_list_projects":
            payload = {"projects": [{"id": "fixture-project", "name": "fixture", "status": "ready", "version": "1", "isCurrent": True}]}
        elif name == "caido_list_requests":
            if "FIXTURE_MODE" == "transport":
                sys.exit(1)
            payload = {} if "FIXTURE_MODE" == "malformed" else {"requests": [], "hasMore": True, "nextCursor": "empty-next-page"}
        else:
            raise RuntimeError("unexpected detail or mutation call")
        result = {"structuredContent": payload}
    else:
        raise RuntimeError("unexpected RPC")
    print(json.dumps({"jsonrpc": "2.0", "id": message["id"], "result": result}), flush=True)
`
