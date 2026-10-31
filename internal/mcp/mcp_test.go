package mcp

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bhaviksardar/fences-gateway/internal/approvals"
	"github.com/bhaviksardar/fences-gateway/internal/engine"
	"github.com/bhaviksardar/fences-gateway/pkg/bundle"
	"github.com/bhaviksardar/fences-gateway/pkg/queue"
	"github.com/bhaviksardar/fences-gateway/pkg/sync"
)

type args struct {
	ID string `json:"id,omitempty"`
}

// upstream is a real MCP server with the given tools, each answering "<server>:<tool>".
func upstream(t *testing.T, name string, tools ...string) string {
	srv := sdk.NewServer(&sdk.Implementation{Name: name}, nil)
	for _, tool := range tools {
		sdk.AddTool(srv, &sdk.Tool{Name: tool}, func(context.Context, *sdk.CallToolRequest, args) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: name + ":" + tool}}}, nil, nil
		})
	}
	ts := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil))
	t.Cleanup(ts.Close)
	return ts.URL
}

func hash(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

const testPolicy = `{"rules": [
	{"id": "reads", "on": "mcp", "effect": "allow", "where": [{"field": "tool", "op": "matches", "value": "get_*"}]},
	{"id": "no-deletes", "on": "mcp", "effect": "deny", "where": [{"field": "tool", "op": "is", "value": "delete_ticket"}], "reason": "agents can't delete tickets"},
	{"id": "mail-cap", "on": "mcp", "effect": "allow", "where": [{"field": "tool", "op": "is", "value": "send_mail"}], "limit": {"window_s": 600, "max": 3}},
	{"id": "refunds", "on": "mcp", "effect": "approve", "where": [{"field": "tool", "op": "is", "value": "refund"}], "reason": "refunds need a person"}
]}`

type harness struct {
	gw        string
	engine    *engine.Engine
	queue     *queue.Queue
	mu        gosync.Mutex
	approvals []map[string]any
}

func setup(t *testing.T) *harness {
	h := &harness{}
	// The platform: only approvals are needed here
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.approvals = append(h.approvals, body)
		h.mu.Unlock()
		w.Write([]byte(`{"ok": true}`))
	}))
	t.Cleanup(platform.Close)
	_, gwKey, _ := ed25519.GenerateKey(nil)
	client := &sync.Client{BaseURL: platform.URL, Kind: "gateway", Version: "test", ID: &sync.Identity{GatewayID: "gw_1", PrivateKey: gwKey}}

	pub, priv, _ := ed25519.GenerateKey(nil)
	store := &bundle.Store{Path: filepath.Join(t.TempDir(), "bundle.json"), Keys: map[string]ed25519.PublicKey{"k1": pub}, GatewayID: "gw_1"}
	payload := `{"version": 1, "gateway_id": "gw_1", "approval_timeout_s": 5, "policy": ` + testPolicy + `,
		"agents": {"bot": {"credentials": [{"type": "token", "sha256": "` + hash("tok-bot") + `"}], "upstreams": ["crm", "mail"]},
		           "crm-only": {"credentials": [{"type": "token", "sha256": "` + hash("tok-crm") + `"}], "upstreams": ["crm"]}}}`
	env, _ := json.Marshal(bundle.Envelope{KeyID: "k1", Payload: base64.StdEncoding.EncodeToString([]byte(payload)),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload)))})
	if err := store.Apply(env); err != nil {
		t.Fatal(err)
	}

	q, _ := queue.Open(filepath.Join(t.TempDir(), "q.db"), 0)
	t.Cleanup(func() { q.Close() })
	h.queue = q
	h.engine = &engine.Engine{Store: store, Queue: q, Approvals: &approvals.Manager{Client: client}, Version: "test"}
	g := &Gateway{Engine: h.engine, Upstreams: map[string]Upstream{
		"crm":  {URL: upstream(t, "crm", "get_ticket", "delete_ticket", "refund")},
		"mail": {URL: upstream(t, "mail", "send_mail")},
	}}
	ts := httptest.NewServer(g.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(g.Close) // before the upstreams close: they wait on the gateway's open streams
	h.gw = ts.URL
	return h
}

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, url, token string) (*sdk.ClientSession, error) {
	c := sdk.NewClient(&sdk.Implementation{Name: "agent"}, nil)
	s, err := c.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer(token)}}, nil)
	if err == nil {
		t.Cleanup(func() { s.Close() })
	}
	return s, err
}

// call calls a tool and returns its text and whether it was an error result.
func call(t *testing.T, s *sdk.ClientSession, tool string, a map[string]any) (string, bool) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: a})
	if err != nil {
		t.Error(err)
		return "", true
	}
	return res.Content[0].(*sdk.TextContent).Text, res.IsError
}

func text(t *testing.T, s *sdk.ClientSession, tool string, a map[string]any) string {
	t.Helper()
	got, _ := call(t, s, tool, a)
	return got
}

func TestGateway(t *testing.T) {
	h := setup(t)
	ctx := context.Background()

	if _, err := connect(t, h.gw+"/mcp/crm", "wrong"); err == nil {
		t.Error("an unknown token must be refused")
	}
	if _, err := connect(t, h.gw+"/mcp/mail", "tok-crm"); err == nil {
		t.Error("an agent must only reach its own upstreams")
	}

	crm, err := connect(t, h.gw+"/mcp/crm", "tok-bot")
	if err != nil {
		t.Fatal(err)
	}
	mail, _ := connect(t, h.gw+"/mcp/mail", "tok-bot")

	// A tool the agent may never call is hidden; calling it anyway is still denied
	list, err := crm.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "get_ticket,refund" {
		t.Errorf("tools/list: %v, want get_ticket and refund", names)
	}
	if got := text(t, crm, "get_ticket", map[string]any{"id": "7"}); got != "crm:get_ticket" {
		t.Errorf("allowed call: %q", got)
	}
	if got, isErr := call(t, crm, "delete_ticket", nil); !isErr || !strings.Contains(got, "agents can't delete tickets") {
		t.Errorf("hidden tool called anyway: %q", got)
	}

	// Rate limit: the fourth call within the window is blocked
	for i := 1; i <= 4; i++ {
		if got := text(t, mail, "send_mail", nil); (i <= 3) != (got == "mail:send_mail") || (i == 4) != strings.Contains(got, "rate_limited") {
			t.Errorf("send_mail call %d: %q", i, got)
		}
	}

	// Approval: the call waits for the answer, which arrives as a command
	answer := func(granted bool) {
		for {
			h.mu.Lock()
			n := len(h.approvals)
			h.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		h.mu.Lock()
		id := h.approvals[0]["approval_id"].(string)
		h.approvals = nil
		h.mu.Unlock()
		raw, _ := json.Marshal(map[string]any{"type": "approval", "id": "approval:" + id, "approval_id": id, "granted": granted, "by": "dana"})
		h.engine.Apply(sync.Command{Type: "approval", ID: "approval:" + id, Raw: raw})
	}
	go answer(true)
	if got := text(t, crm, "refund", map[string]any{"id": "9"}); got != "crm:refund" {
		t.Errorf("approved call: %q", got)
	}
	go answer(false)
	if got := text(t, crm, "refund", nil); !strings.Contains(got, "not approved") {
		t.Errorf("refused call: %q", got)
	}

	// Quarantine: a held call ends at once, and every later call is denied
	done := make(chan string)
	go func() {
		done <- text(t, crm, "refund", nil)
	}()
	for !h.engine.Approvals.Pending() {
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	raw, _ := json.Marshal(map[string]any{"type": "quarantine", "id": "quarantine:bot:1", "agent": "bot", "bundle_version": 2})
	h.engine.Apply(sync.Command{Type: "quarantine", ID: "quarantine:bot:1", Raw: raw})
	if got := <-done; !strings.Contains(got, "deny") || time.Since(start) > 2*time.Second {
		t.Errorf("held call after quarantine: %q after %v", got, time.Since(start))
	}
	if got := text(t, crm, "get_ticket", nil); !strings.Contains(got, "quarantined") {
		t.Errorf("call after quarantine: %q", got)
	}
	if list, _ := crm.ListTools(ctx, nil); len(list.Tools) != 0 {
		t.Errorf("a quarantined agent sees no tools, got %d", len(list.Tools))
	}

	// Every decision is an event
	events, _ := h.queue.Batch(100)
	var got []string
	var first map[string]any
	unauth := map[string]bool{}
	for _, raw := range events {
		var ev map[string]any
		json.Unmarshal(raw, &ev)
		if ev["decision"] == "unauthenticated" { // one or more per refused connection, depending on the client
			unauth[ev["target"].(string)] = ev["agent"] == nil
			continue
		}
		if first == nil {
			first = ev
		}
		got = append(got, ev["decision"].(string)+" "+ev["target"].(string))
	}
	if !unauth["crm/"] || !unauth["mail/"] {
		t.Errorf("refused connections must be recorded, with no agent: %v", unauth)
	}
	want := "allow crm/get_ticket,deny crm/delete_ticket,allow mail/send_mail," +
		"allow mail/send_mail,allow mail/send_mail,rate_limited mail/send_mail,allow crm/refund,deny crm/refund,deny crm/refund,deny crm/get_ticket"
	if strings.Join(got, ",") != want {
		t.Errorf("events:\n got  %s\n want %s", strings.Join(got, ","), want)
	}
	if first["upstream_status"] != "ok" || first["arguments"].([]any)[0] != "id" || first["rule"] != "reads" {
		t.Errorf("allowed event: %v", first)
	}
}

func TestArgumentsAndLimits(t *testing.T) {
	h := setup(t)
	crm, err := connect(t, h.gw+"/mcp/crm", "tok-bot")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// A repeated key: parsers disagree on which copy wins, so the call is refused before any decision
	for _, raw := range []string{`{"id": "1", "id": "2"}`, `{"a": {"x": 1, "x": 2}}`, `{"a": [{"x": 1, "x": 2}]}`} {
		if _, err := crm.CallTool(ctx, &sdk.CallToolParams{Name: "get_ticket", Arguments: json.RawMessage(raw)}); err == nil || !strings.Contains(err.Error(), "repeated") {
			t.Errorf("%s: %v", raw, err)
		}
	}
	// Without repeats, nested arguments are forwarded (the test upstream then rejects them by its own schema)
	if _, err := crm.CallTool(ctx, &sdk.CallToolParams{Name: "get_ticket", Arguments: json.RawMessage(`{"a": {"x": 1}, "b": [1, 1]}`)}); err != nil {
		t.Errorf("nested arguments without repeats: %v", err)
	}
	// An upstream reply over the size limit is an error, not passed through
	MaxResponse = 10
	defer func() { MaxResponse = 16 << 20 }()
	if _, err := crm.CallTool(ctx, &sdk.CallToolParams{Name: "get_ticket"}); err == nil {
		t.Error("a reply over MaxResponse must fail")
	}
}

// When the approval request can't be sent, the rule's on_unavailable decides: deny by default
func TestApprovalPlatformDown(t *testing.T) {
	h := setup(t)
	h.engine.Approvals.Client.BaseURL = "http://127.0.0.1:1" // nothing listens
	crm, err := connect(t, h.gw+"/mcp/crm", "tok-bot")
	if err != nil {
		t.Fatal(err)
	}
	if got := text(t, crm, "refund", nil); !strings.Contains(got, "can't be reached") {
		t.Errorf("approval with the platform down: %q", got)
	}
	events, _ := h.queue.Batch(10)
	var ev map[string]any
	json.Unmarshal(events[len(events)-1], &ev)
	if ev["decision"] != "deny" || ev["degraded"] != true {
		t.Errorf("a fail-mode decision is reported degraded: %v", ev)
	}
}
