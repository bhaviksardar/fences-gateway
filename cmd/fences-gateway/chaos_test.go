package main

import (
	"context"
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
	gosync "sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bhaviksardar/fences-gateway/pkg/bundle"
)

// flaky is a server that can be taken down: while down it drops every connection, as an unreachable host would.
type flaky struct {
	down atomic.Bool
	next http.Handler
}

func (f *flaky) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.down.Load() {
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			conn.Close()
		}
		return
	}
	f.next.ServeHTTP(w, r)
}

// TestChaos is the roadmap's chaos checks against the whole process: the platform unreachable (the cached bundle
// keeps working, approvals fail closed, a restart serves from the bundle on disk), the upstream MCP server down and
// back, and a restart while a call waits on an approval (it ends at once; the events made before it are kept).
func TestChaos(t *testing.T) {
	up := sdk.NewServer(&sdk.Implementation{Name: "crm"}, nil)
	for _, name := range []string{"get_ticket", "refund", "small_refund"} {
		sdk.AddTool(up, &sdk.Tool{Name: name}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "done " + name}}}, nil, nil
		})
	}
	upstream := &flaky{next: sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return up }, nil)}
	upSrv := httptest.NewServer(upstream)
	defer upSrv.Close()

	pub, priv, _ := ed25519.GenerateKey(nil)
	sum := sha256.Sum256([]byte("tok-bot"))
	payload := `{"version": 1, "gateway_id": "gw_1", "approval_timeout_s": 30, "policy": {"rules": [
		{"id": "reads", "on": "mcp", "effect": "allow", "where": [{"field": "tool", "op": "matches", "value": "get_*"}]},
		{"id": "refunds", "on": "mcp", "effect": "approve", "where": [{"field": "tool", "op": "is", "value": "refund"}]},
		{"id": "small", "on": "mcp", "effect": "approve", "where": [{"field": "tool", "op": "is", "value": "small_refund"}], "on_unavailable": "allow"}]},
		"agents": {"bot": {"credentials": [{"type": "token", "sha256": "` + hex.EncodeToString(sum[:]) + `"}], "upstreams": ["crm"]}}}`
	var mu gosync.Mutex
	events := map[float64]map[string]any{} // seq → event, as stored
	asked := 0
	var commands []any
	platform := &flaky{next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/gateways/enroll":
			json.NewEncoder(w).Encode(map[string]any{"gateway_id": "gw_1", "sync_interval_s": 0.2,
				"signing_keys": []map[string]any{{"key_id": "k1", "public_key": []byte(pub)}}})
		case "/api/gateways/sync":
			reply := map[string]any{"commands": commands}
			if body["bundle_version"].(float64) < 1 {
				reply["bundle"] = bundle.Envelope{KeyID: "k1", Payload: base64.StdEncoding.EncodeToString([]byte(payload)),
					Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload)))}
			}
			json.NewEncoder(w).Encode(reply)
		case "/api/gateways/decisions":
			top := 0.0
			for _, e := range body["events"].([]any) {
				ev := e.(map[string]any)
				events[ev["seq"].(float64)] = ev
				top = max(top, ev["seq"].(float64))
			}
			json.NewEncoder(w).Encode(map[string]any{"acked_seq": top})
		case "/api/gateways/approvals":
			asked++
			w.Write([]byte(`{"ok": true}`))
		}
	})}
	platSrv := httptest.NewServer(platform)
	defer platSrv.Close()

	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	os.WriteFile(config, []byte(`{"platform": "`+platSrv.URL+`", "name": "chaos", "data_dir": "`+filepath.Join(dir, "data")+`",
		"listen": "127.0.0.1:0", "upstreams": {"crm": {"url": "`+upSrv.URL+`"}}}`), 0o600)
	t.Setenv("FENCES_ENROLL_TOKEN", "fge_ok")

	start := func() (string, context.CancelFunc, chan error) {
		ctx, cancel := context.WithCancel(context.Background())
		ready, done := make(chan string, 1), make(chan error, 1)
		go func() { done <- run(ctx, config, ready) }()
		addr := <-ready
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if resp, err := http.Get("http://" + addr + "/readyz"); err == nil && resp.StatusCode == 200 {
				break
			} else if time.Now().After(deadline) {
				t.Fatal("never ready")
			}
		}
		return addr, cancel, done
	}
	connect := func(addr string) *sdk.ClientSession {
		s, err := sdk.NewClient(&sdk.Implementation{Name: "agent"}, nil).Connect(context.Background(),
			&sdk.StreamableClientTransport{Endpoint: "http://" + addr + "/mcp/crm", HTTPClient: &http.Client{Transport: bearer("tok-bot")}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	call := func(s *sdk.ClientSession, tool string) (string, bool) {
		res, err := s.CallTool(context.Background(), &sdk.CallToolParams{Name: tool})
		if err != nil {
			return err.Error(), true
		}
		return res.Content[0].(*sdk.TextContent).Text, res.IsError
	}

	addr, stop, done := start()
	s := connect(addr)
	if got, isErr := call(s, "get_ticket"); isErr || got != "done get_ticket" {
		t.Fatalf("healthy: %q", got)
	}

	// The platform unreachable: the cached bundle keeps deciding; an approval can't be asked, so it fails closed,
	// unless the rule says on_unavailable: allow
	platform.down.Store(true)
	if got, isErr := call(s, "get_ticket"); isErr || got != "done get_ticket" {
		t.Errorf("platform down, read: %q", got)
	}
	if got, isErr := call(s, "refund"); !isErr || !strings.Contains(got, "can't be reached") {
		t.Errorf("platform down, approval: %q", got)
	}
	if got, isErr := call(s, "small_refund"); isErr || got != "done small_refund" {
		t.Errorf("platform down, on_unavailable allow: %q", got)
	}

	// The upstream down, then back: a clean error, counted, then calls work again
	upstream.down.Store(true)
	if got, isErr := call(s, "get_ticket"); !isErr {
		t.Errorf("upstream down: %q", got)
	}
	upstream.down.Store(false)
	var got string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if got, _ = call(s, "get_ticket"); got == "done get_ticket" {
			break
		}
	}
	if got != "done get_ticket" {
		t.Errorf("upstream back: %q", got)
	}

	// A restart while a call waits on an approval: the call is denied at once, and the process stops within seconds
	platform.down.Store(false)
	held := make(chan string, 1)
	go func() { text, _ := call(s, "refund"); held <- text }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		n := asked
		mu.Unlock()
		if n > 0 {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("the approval was never asked")
		}
	}
	platform.down.Store(true) // the events from here on can only be shipped by the next process
	call(s, "get_ticket")
	began := time.Now()
	stop()
	if err := <-done; err != nil {
		t.Errorf("shutdown: %v", err)
	}
	if took := time.Since(began); took > 4*time.Second {
		t.Errorf("shutdown with a held call took %v", took)
	}
	select {
	case text := <-held:
		if !strings.Contains(text, "approval wait ended") && !strings.Contains(text, "EOF") {
			t.Errorf("the held call: %q", text)
		}
	case <-time.After(3 * time.Second):
		t.Error("the held call never ended")
	}
	s.Close()

	// Back up with the platform still down: the identity and last good bundle are on disk
	addr, stop, done = start()
	s = connect(addr)
	if got, isErr := call(s, "get_ticket"); isErr || got != "done get_ticket" {
		t.Errorf("restarted with the platform down: %q", got)
	}
	// The platform returns, and answers the approval the old process asked: the new one isn't waiting on it
	mu.Lock()
	commands = []any{map[string]any{"type": "approval", "id": "approval:gone", "approval_id": "gone", "granted": true, "by": "dana"}}
	mu.Unlock()
	platform.down.Store(false)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		mu.Lock()
		reads := 0
		for _, e := range events {
			if e["target"] == "crm/get_ticket" && e["decision"] == "allow" {
				reads++
			}
		}
		mu.Unlock()
		if reads >= 6 { // 2 healthy, 1 platform down, 1 upstream down, 1+ upstream back, 1 before and 1 after restart
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("events made while the platform was down weren't all shipped: %d reads", reads)
		}
	}
	if got, isErr := call(s, "get_ticket"); isErr || got != "done get_ticket" {
		t.Errorf("after the stale answer: %q", got)
	}
	s.Close()
	stop()
	if err := <-done; err != nil {
		t.Errorf("shutdown: %v", err)
	}
}
