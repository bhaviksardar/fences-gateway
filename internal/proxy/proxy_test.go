package proxy

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/bhaviksardar/fences-gateway/internal/approvals"
	"github.com/bhaviksardar/fences-gateway/internal/engine"
	"github.com/bhaviksardar/fences-gateway/pkg/bundle"
	"github.com/bhaviksardar/fences-gateway/pkg/queue"
	"github.com/bhaviksardar/fences-gateway/pkg/sync"
)

// The roadmap's refunds example: at most $500 an hour per agent, refunds over $100 need approval.
const refundPolicy = `{"rules": [
	{"id": "refund-cap", "on": "http", "effect": "allow", "where": [{"field": "method", "op": "is", "value": "POST"}, {"field": "path", "op": "is", "value": "/refunds"}],
	 "limit": {"window_s": 3600, "max": 500, "sum": "body.amount"}, "reason": "$500 of refunds an hour"},
	{"id": "big-refunds", "on": "http", "effect": "approve", "where": [{"field": "method", "op": "is", "value": "POST"}, {"field": "path", "op": "is", "value": "/refunds"},
	 {"field": "body.amount", "op": "gt", "value": 100}], "reason": "refunds over $100 need a person"},
	{"id": "reads", "on": "http", "effect": "allow", "where": [{"field": "method", "op": "is", "value": "GET"}]}
]}`

func TestRefunds(t *testing.T) {
	var mu gosync.Mutex
	var asked []map[string]any
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		asked = append(asked, body)
		mu.Unlock()
		w.Write([]byte(`{"ok": true}`))
	}))
	defer platform.Close()
	var got []*http.Request
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
		w.WriteHeader(201)
		w.Write([]byte(`{"refunded": true, "body": ` + string(body) + `}`))
	}))
	defer api.Close()

	sum := sha256.Sum256([]byte("tok-bot"))
	payload := `{"version": 1, "gateway_id": "gw_1", "approval_timeout_s": 5, "policy": ` + refundPolicy + `,
		"agents": {"bot": {"credentials": [{"type": "token", "sha256": "` + hex.EncodeToString(sum[:]) + `"}], "upstreams": ["refunds"]}}}`
	pub, priv, _ := ed25519.GenerateKey(nil)
	store := &bundle.Store{Path: filepath.Join(t.TempDir(), "b.json"), Keys: map[string]ed25519.PublicKey{"k1": pub}, GatewayID: "gw_1"}
	env, _ := json.Marshal(bundle.Envelope{KeyID: "k1", Payload: base64.StdEncoding.EncodeToString([]byte(payload)),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload)))})
	if err := store.Apply(env); err != nil {
		t.Fatal(err)
	}
	q, _ := queue.Open(filepath.Join(t.TempDir(), "q.db"), 0)
	defer q.Close()
	_, gwKey, _ := ed25519.GenerateKey(nil)
	client := &sync.Client{BaseURL: platform.URL, Kind: "gateway", Version: "test", ID: &sync.Identity{GatewayID: "gw_1", PrivateKey: gwKey}}
	eng := &engine.Engine{Store: store, Queue: q, Approvals: &approvals.Manager{Client: client}, Version: "test"}
	gw := httptest.NewServer((&Proxy{Engine: eng, APIs: map[string]API{
		"refunds": {URL: api.URL + "/v1", Headers: map[string]string{"X-Api-Key": "api-secret"}},
		"down":    {URL: "http://127.0.0.1:1"},
	}}).Handler())
	defer gw.Close()

	send := func(method, path, body, token string, header ...string) (int, string) {
		req, _ := http.NewRequest(method, gw.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	approve := func(granted bool, amount string) {
		for {
			mu.Lock()
			n := len(asked)
			mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		mu.Lock()
		id := asked[0]["approval_id"].(string)
		summary := asked[0]["summary"].(string)
		asked = nil
		mu.Unlock()
		if !strings.Contains(summary, amount) {
			t.Errorf("the approval shows the amount: %q", summary)
		}
		raw, _ := json.Marshal(map[string]any{"type": "approval", "id": "approval:" + id, "approval_id": id, "granted": granted, "by": "dana"})
		eng.Apply(sync.Command{Type: "approval", ID: "approval:" + id, Raw: raw})
	}

	// Under $100: forwarded, with the API's credential and without the agent's token
	if code, body := send("POST", "/http/refunds/refunds", `{"amount": 80}`, "tok-bot"); code != 201 || !strings.Contains(body, `"amount": 80`) {
		t.Fatalf("small refund: %d %s", code, body)
	}
	if r := got[0]; r.URL.Path != "/v1/refunds" || r.Header.Get("X-Api-Key") != "api-secret" || r.Header.Get("Authorization") != "" {
		t.Errorf("forwarded as %s with %v", r.URL.Path, r.Header)
	}
	// Over $100: held until approved
	go approve(true, "240")
	if code, _ := send("POST", "/http/refunds/refunds", `{"amount": 240}`, "tok-bot"); code != 201 {
		t.Errorf("approved refund: %d", code)
	}
	go approve(false, "150")
	if code, body := send("POST", "/http/refunds/refunds", `{"amount": 150}`, "tok-bot"); code != 403 || !strings.Contains(body, "not approved") {
		t.Errorf("refused refund: %d %s", code, body)
	}
	// $500 an hour: 80 + 240 + 90 = 410, so another 100 is over, whatever Content-Type claims the body is
	if code, _ := send("POST", "/http/refunds/refunds", `{"amount": 90}`, "tok-bot"); code != 201 {
		t.Errorf("third refund: %d", code)
	}
	if code, body := send("POST", "/http/refunds/refunds", `{"amount": 100}`, "tok-bot", "Content-Type", "text/plain"); code != 429 || !strings.Contains(body, "$500 of refunds an hour") {
		t.Errorf("over the hour's cap: %d %s", code, body)
	}
	if code, _ := send("POST", "/http/refunds/refunds", `amount=10`, "tok-bot"); code != 429 {
		t.Errorf("an amount the gateway can't read counts as over the cap: %d", code)
	}

	// Refused before anything is decided or forwarded
	for _, c := range []struct {
		method, path, body, token string
		want                      int
	}{
		{"GET", "/http/refunds/refunds?id=1&id=2", "", "tok-bot", 400},
		{"POST", "/http/refunds/refunds", `{"amount": 1, "amount": 900}`, "tok-bot", 400},
		{"GET", "/http/refunds/refunds", "", "tok-nobody", 401},
		{"GET", "/http/down/x", "", "tok-bot", 401}, // not assigned
		{"GET", "/http/nope/x", "", "tok-bot", 404},
		{"DELETE", "/http/refunds/refunds/7", "", "tok-bot", 403}, // no rule: the default is deny
	} {
		if code, body := send(c.method, c.path, c.body, c.token); code != c.want {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.path, code, body, c.want)
		}
	}
	if len(got) != 3 {
		t.Errorf("the API saw %d requests, want 3", len(got))
	}

	// Quarantine: a read is denied too
	raw, _ := json.Marshal(map[string]any{"type": "quarantine", "id": "quarantine:bot:1", "agent": "bot", "bundle_version": 2})
	eng.Apply(sync.Command{Type: "quarantine", ID: "quarantine:bot:1", Raw: raw})
	if code, body := send("GET", "/http/refunds/refunds/7", "", "tok-bot"); code != 403 || !strings.Contains(body, "quarantined") {
		t.Errorf("after quarantine: %d %s", code, body)
	}

	// Every decision is an event, with the API's status when forwarded
	events, _ := q.Batch(100, 1<<20)
	var statuses []any
	for _, e := range events {
		var ev map[string]any
		json.Unmarshal(e, &ev)
		if ev["target"] == "POST /refunds" && ev["upstream_status"] != nil {
			statuses = append(statuses, ev["upstream_status"])
		}
	}
	if len(statuses) != 3 || statuses[0] != float64(201) {
		t.Errorf("upstream statuses: %v (%d events)", statuses, len(events))
	}
}

func TestUpstreamDown(t *testing.T) {
	sum := sha256.Sum256([]byte("tok"))
	payload := `{"version": 1, "gateway_id": "gw_1", "policy": {"default": "allow", "rules": []},
		"agents": {"bot": {"credentials": [{"type": "token", "sha256": "` + hex.EncodeToString(sum[:]) + `"}], "upstreams": ["down"]}}}`
	pub, priv, _ := ed25519.GenerateKey(nil)
	store := &bundle.Store{Path: filepath.Join(t.TempDir(), "b.json"), Keys: map[string]ed25519.PublicKey{"k1": pub}, GatewayID: "gw_1"}
	env, _ := json.Marshal(bundle.Envelope{KeyID: "k1", Payload: base64.StdEncoding.EncodeToString([]byte(payload)),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload)))})
	store.Apply(env)
	q, _ := queue.Open(filepath.Join(t.TempDir(), "q.db"), 0)
	defer q.Close()
	eng := &engine.Engine{Store: store, Queue: q, Version: "test"}
	gw := httptest.NewServer((&Proxy{Engine: eng, APIs: map[string]API{"down": {URL: "http://127.0.0.1:1"}}}).Handler())
	defer gw.Close()
	req, _ := http.NewRequest("GET", gw.URL+"/http/down/x", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 502 {
		t.Fatalf("down API: %v %v", resp, err)
	}
	if h := eng.Health(); !strings.Contains(mustJSON(h), `"down":1`) {
		t.Errorf("an unreachable API counts as an upstream error: %s", mustJSON(h))
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
