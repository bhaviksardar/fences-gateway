package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhaviksardar/fences-gateway/internal/engine"
	"github.com/bhaviksardar/fences-gateway/internal/hook"
	"github.com/bhaviksardar/fences-gateway/pkg/bundle"
	"github.com/bhaviksardar/fences-gateway/pkg/queue"
)

// The hook against a real gateway engine with the example Claude Code policy (docs/examples/claude-code.json).
func TestHook(t *testing.T) {
	policy, err := os.ReadFile("../../docs/examples/claude-code.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("tok-dev"))
	payload := `{"version": 1, "gateway_id": "gw_1", "approval_timeout_s": 5, "policy": ` + string(policy) + `,
		"agents": {"dev-alice": {"credentials": [{"type": "token", "sha256": "` + hex.EncodeToString(sum[:]) + `"}], "upstreams": ["claude-code"]}}}`
	pub, priv, _ := ed25519.GenerateKey(nil)
	store := &bundle.Store{Path: filepath.Join(t.TempDir(), "b.json"), Keys: map[string]ed25519.PublicKey{"k1": pub}, GatewayID: "gw_1"}
	env, _ := json.Marshal(bundle.Envelope{KeyID: "k1", Payload: base64.StdEncoding.EncodeToString([]byte(payload)),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload)))})
	if err := store.Apply(env); err != nil {
		t.Fatal(err)
	}
	q, _ := queue.Open(filepath.Join(t.TempDir(), "q.db"), 0)
	defer q.Close()
	eng := &engine.Engine{Store: store, Queue: q, Version: "test"}
	mux := http.NewServeMux()
	mux.Handle("POST /hook/{name}", hook.Handler(eng))
	gw := httptest.NewServer(mux)
	defer gw.Close()

	vars := map[string]string{"FENCES_GATEWAY_URL": gw.URL, "FENCES_TOKEN": "tok-dev", "CLAUDE_PROJECT_DIR": "/work/app"}
	ask := func(tool string, input map[string]any) string {
		in, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": input, "cwd": "/work/app/src"})
		return decide(in, func(k string) string { return vars[k] })
	}
	for _, c := range []struct {
		tool  string
		input map[string]any
		want  string // "" allowed, else part of the reason
	}{
		{"Bash", map[string]any{"command": "go test ./..."}, ""},
		{"Bash", map[string]any{"command": "git push origin main"}, "open a pull request"},
		{"Bash", map[string]any{"command": "cd /tmp && rm -rf /var/lib"}, "absolute path"},
		{"Bash", map[string]any{"command": "rm -rf ~/projects"}, "home directory"},
		{"Bash", map[string]any{"command": "rm -rf ./dist"}, ""},
		{"Write", map[string]any{"file_path": "/work/app/src/main.go", "content": "x"}, ""},
		{"Edit", map[string]any{"file_path": "../../../etc/hosts"}, "outside the project"},
		{"Write", map[string]any{"file_path": "/work/app-other/x"}, "outside the project"},
		{"Read", map[string]any{"file_path": "/etc/hosts"}, ""},
	} {
		if got := ask(c.tool, c.input); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s %v: got %q, want %q", c.tool, c.input, got, c.want)
		}
	}
	if n := q.Depth(); n != 9 {
		t.Errorf("every decision is recorded: %d events", n)
	}

	req, _ := http.NewRequest("POST", gw.URL+"/hook/claude-code", strings.NewReader(`{"tool": "Bash", "args": {"command": "ls", "command": "git push origin main"}}`))
	req.Header.Set("Authorization", "Bearer tok-dev")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 400 {
		t.Errorf("a repeated key must be refused: %v %v", resp.StatusCode, err)
	}

	// It fails closed
	vars["FENCES_TOKEN"] = "tok-unknown"
	if got := ask("Bash", map[string]any{"command": "ls"}); !strings.Contains(got, "401") {
		t.Errorf("unknown token: %q", got)
	}
	vars["FENCES_GATEWAY_URL"] = "http://127.0.0.1:1"
	if got := ask("Bash", map[string]any{"command": "ls"}); !strings.Contains(got, "couldn't be asked") {
		t.Errorf("gateway down: %q", got)
	}
	vars["FENCES_GATEWAY_URL"] = "http://bad host"
	if got := ask("Bash", map[string]any{"command": "ls"}); !strings.Contains(got, "isn't set up right") {
		t.Errorf("a malformed URL must block, not crash: %q", got)
	}
	if got := decide([]byte("not json"), func(string) string { return "" }); got == "" {
		t.Error("bad input must block")
	}
}

func TestInProject(t *testing.T) {
	for _, c := range [][4]string{
		{"src/a.go", "/w/app", "/w/app", "src/a.go"},
		{"/w/app/a.go", "/w", "/w/app", "a.go"},
		{"../b", "/w/app", "/w/app", "/w/b"},
		{"/w/app/../app2/x", "/", "/w/app", "/w/app2/x"},
		{"/w/app", "/", "/w/app", "."},
	} {
		if got := inProject(c[0], c[1], c[2]); got != c[3] {
			t.Errorf("inProject(%q, cwd %q, project %q) = %q, want %q", c[0], c[1], c[2], got, c[3])
		}
	}
}
