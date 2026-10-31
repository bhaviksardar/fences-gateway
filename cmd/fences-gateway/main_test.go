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
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bhaviksardar/fences-gateway/pkg/bundle"
)

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// The whole process: enroll, get a bundle by sync, serve an MCP call through to an upstream, ship its event.
func TestRun(t *testing.T) {
	// An upstream MCP server that checks the gateway's own credential
	up := sdk.NewServer(&sdk.Implementation{Name: "crm"}, nil)
	sdk.AddTool(up, &sdk.Tool{Name: "get_ticket"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ticket 7"}}}, nil, nil
	})
	upHandler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return up }, nil)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "upstream-secret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		upHandler.ServeHTTP(w, r)
	}))
	defer upstream.Close()

	pub, priv, _ := ed25519.GenerateKey(nil)
	sum := sha256.Sum256([]byte("tok-bot"))
	payload := `{"version": 1, "gateway_id": "gw_1", "policy": {"rules": [{"id": "reads", "on": "mcp", "effect": "allow", "where": [{"field": "tool", "op": "matches", "value": "get_*"}]}]},
		"agents": {"bot": {"credentials": [{"type": "token", "sha256": "` + hex.EncodeToString(sum[:]) + `"}], "upstreams": ["crm"]}}}`
	var mu gosync.Mutex
	var events []map[string]any
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/api/gateways/enroll":
			json.NewEncoder(w).Encode(map[string]any{"gateway_id": "gw_1", "sync_interval_s": 15,
				"signing_keys": []map[string]any{{"key_id": "k1", "public_key": []byte(pub)}}})
		case "/api/gateways/sync":
			reply := map[string]any{"commands": []any{}}
			if body["bundle_version"].(float64) < 1 {
				reply["bundle"] = bundle.Envelope{KeyID: "k1", Payload: base64.StdEncoding.EncodeToString([]byte(payload)),
					Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload)))}
			}
			json.NewEncoder(w).Encode(reply)
		case "/api/gateways/decisions":
			mu.Lock()
			var top float64
			for _, e := range body["events"].([]any) {
				events = append(events, e.(map[string]any))
				top = max(top, e.(map[string]any)["seq"].(float64))
			}
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"acked_seq": top})
		}
	}))
	defer platform.Close()

	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	os.WriteFile(config, []byte(`{"platform": "`+platform.URL+`", "name": "test", "data_dir": "`+filepath.Join(dir, "data")+`", "listen": "127.0.0.1:0",
		"upstreams": {"crm": {"url": "`+upstream.URL+`", "headers": {"X-Api-Key": "${TEST_UPSTREAM_KEY}"}}}}`), 0o600)
	t.Setenv("TEST_UPSTREAM_KEY", "upstream-secret")
	if c, _ := loadConfig(config); c.Upstreams["crm"].Headers["X-Api-Key"] != "upstream-secret" {
		t.Fatal("${TEST_UPSTREAM_KEY} not expanded")
	}
	os.WriteFile(filepath.Join(dir, "lit.json"), []byte(`{"platform": "https://x", "name": "n", "data_dir": "d", "upstreams": {"a": {"url": "http://u", "headers": {"K": "s3cr$tvalue"}}}}`), 0o600)
	if c, _ := loadConfig(filepath.Join(dir, "lit.json")); c.Upstreams["a"].Headers["K"] != "s3cr$tvalue" {
		t.Errorf("a bare $ in a secret must stay: %q", c.Upstreams["a"].Headers["K"])
	}

	if err := run(context.Background(), config, nil); err == nil || !strings.Contains(err.Error(), "FENCES_ENROLL_TOKEN") {
		t.Fatalf("without a token or an identity: %v", err)
	}
	t.Setenv("FENCES_ENROLL_TOKEN", "fge_ok")
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, config, ready) }()
	addr := <-ready
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()

	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if resp, err := http.Get("http://" + addr + "/readyz"); err == nil && resp.StatusCode == 200 {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("never became ready: no bundle")
		}
	}
	c := sdk.NewClient(&sdk.Implementation{Name: "agent"}, nil)
	s, err := c.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: "http://" + addr + "/mcp/crm", HTTPClient: &http.Client{Transport: bearer("tok-bot")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res, err := s.CallTool(ctx, &sdk.CallToolParams{Name: "get_ticket"})
	if err != nil || res.Content[0].(*sdk.TextContent).Text != "ticket 7" {
		t.Fatalf("call through the gateway: %v %v", res, err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		mu.Lock()
		n := len(events)
		mu.Unlock()
		if n > 0 {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("the decision event never reached the platform")
		}
	}
	if events[0]["decision"] != "allow" || events[0]["agent"] != "bot" || events[0]["target"] != "crm/get_ticket" {
		t.Errorf("event: %v", events[0])
	}
}

func TestConfigErrors(t *testing.T) {
	dir := t.TempDir()
	for name, cfg := range map[string]string{
		"misspelt setting": `{"platform": "https://x", "name": "n", "data_dir": "d", "upstreams": {"a": {"url": "http://u"}}, "listn": ":1"}`,
		"plain http":       `{"platform": "http://fences.example", "name": "n", "data_dir": "d", "upstreams": {"a": {"url": "http://u"}}}`,
		"no data_dir":      `{"platform": "https://x", "name": "n"}`,
		"slash in name":    `{"platform": "https://x", "name": "n", "data_dir": "d", "upstreams": {"a/b": {"url": "http://u"}}}`,
	} {
		p := filepath.Join(dir, "c.json")
		os.WriteFile(p, []byte(cfg), 0o600)
		if _, err := loadConfig(p); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
