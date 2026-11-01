package engine

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bhaviksardar/fences-gateway/internal/approvals"
	"github.com/bhaviksardar/fences-gateway/internal/shared"
	"github.com/bhaviksardar/fences-gateway/pkg/bundle"
	"github.com/bhaviksardar/fences-gateway/pkg/policy"
	"github.com/bhaviksardar/fences-gateway/pkg/queue"
	"github.com/bhaviksardar/fences-gateway/pkg/sync"
)

// The engine keeps running counts for limits instead of every allowed request. Random traffic must be decided
// exactly as pkg/policy decides it from the full history.
const testPolicy = `{"default": "allow", "rules": [
		{"id": "restarts", "on": "k8s", "effect": "allow", "where": [{"field": "verb", "op": "is", "value": "delete"}],
		 "limit": {"window_s": 60, "max": 3, "per": ["namespace"]}},
		{"id": "spend", "on": "http", "effect": "allow", "where": [{"field": "path", "op": "is", "value": "/refunds"}],
		 "limit": {"window_s": 30, "max": 500, "sum": "body.amount"}},
		{"id": "calls", "on": "http", "effect": "allow", "limit": {"window_s": 10, "max": 20}}]}`

func newEngine(t *testing.T) *Engine {
	payload := `{"version": 1, "gateway_id": "gw_1", "policy": ` + testPolicy + `, "agents": {}}`
	pub, priv, _ := ed25519.GenerateKey(nil)
	store := &bundle.Store{Path: filepath.Join(t.TempDir(), "b.json"), Keys: map[string]ed25519.PublicKey{"k1": pub}, GatewayID: "gw_1"}
	env, _ := json.Marshal(bundle.Envelope{KeyID: "k1", Payload: base64.StdEncoding.EncodeToString([]byte(payload)),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload)))})
	if err := store.Apply(env); err != nil {
		t.Fatal(err)
	}
	q, _ := queue.Open(filepath.Join(t.TempDir(), "q.db"), 0)
	t.Cleanup(func() { q.Close() })
	return &Engine{Store: store, Queue: q}
}

// The engine keeps running counts for limits instead of every allowed request. Random traffic must be decided
// exactly as pkg/policy decides it from the full history.
func TestCountsMatchHistory(t *testing.T) {
	e := newEngine(t)
	compare(t, []*Engine{e})
}

// Two replicas sharing their counts in a Redis-protocol store decide random traffic, sent to either at random,
// exactly as one gateway would. FENCES_TEST_REDIS is the store, e.g. redis://:pass@127.0.0.1:6379.
func TestSharedLimits(t *testing.T) {
	url := os.Getenv("FENCES_TEST_REDIS")
	if url == "" {
		t.Skip("FENCES_TEST_REDIS not set")
	}
	prefix := fmt.Sprintf("fences:test:%d:", time.Now().UnixNano())
	a, b := newEngine(t), newEngine(t)
	for _, e := range []*Engine{a, b} {
		var err error
		if e.Shared, err = shared.Open(url, prefix); err != nil {
			t.Fatal(err)
		}
	}
	compare(t, []*Engine{a, b})
	if h := a.Health(); h["shared_limits"] != "ok" {
		t.Errorf("health: %v", h["shared_limits"])
	}

	// Racing: 60 restarts at once across both replicas, 3 allowed per namespace a minute; never more than 3 get through
	var wg gosync.WaitGroup
	var allowed atomic.Int32
	for i := range 60 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := []*Engine{a, b}[i%2].Decide(context.Background(), policy.Request{"agent": "race", "on": "k8s",
				"at": 1793630000.0 + float64(i)/1000, "verb": "delete", "namespace": "shop"})
			c.Done("", nil, 0)
			if c.Allowed() {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := allowed.Load(); n < 1 || n > 3 {
		t.Errorf("racing replicas let %d through, want 1 to 3", n)
	}

	// The store unreachable: each replica counts on its own, and says so
	down, _ := shared.Open("redis://127.0.0.1:1", prefix)
	a.Shared = down
	c := a.Decide(context.Background(), policy.Request{"agent": "a", "on": "k8s", "at": 1793620000.0, "verb": "delete", "namespace": "x"})
	c.Done("", nil, 0)
	if !c.Allowed() || !c.ev.Degraded {
		t.Errorf("store down: %v degraded=%v", c.Decision, c.ev.Degraded)
	}
	if h := a.Health(); h["shared_limits"] == "ok" {
		t.Error("health should show the store's error")
	}
}

// compare sends 5,000 random requests, each to one of engines, and checks every decision against pkg/policy deciding
// from the full history of allowed requests.
func compare(t *testing.T, engines []*Engine) {
	ref := engines[0].Store.Current().Policy
	rng := rand.New(rand.NewPCG(1, 2))
	var history []policy.Request
	at := 1793610000.0
	counts := map[string]int{}
	for i := range 5000 {
		at += rng.Float64() * 0.7
		agent := []string{"a", "b"}[rng.IntN(2)]
		var req policy.Request
		if rng.IntN(2) == 0 {
			req = policy.Request{"agent": agent, "on": "k8s", "at": at, "verb": []string{"delete", "get"}[rng.IntN(2)],
				"kind": "Pod", "namespace": []string{"shop", "search"}[rng.IntN(2)]}
		} else {
			body := map[string]any{"amount": float64(rng.IntN(120))}
			if rng.IntN(10) == 0 {
				body = map[string]any{"amount": "lots"} // can't be checked: over the limit
			}
			req = policy.Request{"agent": agent, "on": "http", "at": at, "method": "POST",
				"path": []string{"/refunds", "/orders"}[rng.IntN(2)], "body": body}
		}
		want := ref.Decide(req, history, false)
		c := engines[rng.IntN(len(engines))].Decide(context.Background(), req)
		c.Done("", nil, 0)
		if c.Decision != want {
			t.Fatalf("request %d %v: engine %v, policy from history %v (degraded %v)", i, req, c.Decision, want, c.ev.Degraded)
		}
		if want.Decision == policy.Allow {
			history = append(history, req)
		}
		counts[want.Decision]++
	}
	if counts[policy.Allow] == 0 || counts[policy.RateLimited] < 100 {
		t.Errorf("the traffic should hit the limits often: %v", counts)
	}
}

func signedBundle(t *testing.T, store *bundle.Store, priv ed25519.PrivateKey, version int, pol string) {
	payload := fmt.Sprintf(`{"version": %d, "gateway_id": "gw_1", "approval_timeout_s": 60, "policy": %s, "agents": {}}`, version, pol)
	env, _ := json.Marshal(bundle.Envelope{KeyID: "k1", Payload: base64.StdEncoding.EncodeToString([]byte(payload)),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload)))})
	if err := store.Apply(env); err != nil {
		t.Fatal(err)
	}
}

// A caller's deadline running out while it waits on an approval is the wait ending, never "the platform can't be
// asked": that would let on_unavailable: allow through without anyone approving.
func TestApprovalDeadlineIsNotUnavailable(t *testing.T) {
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok": true}`)) // asked; nobody answers
	}))
	defer platform.Close()
	pub, priv, _ := ed25519.GenerateKey(nil)
	store := &bundle.Store{Path: filepath.Join(t.TempDir(), "b.json"), Keys: map[string]ed25519.PublicKey{"k1": pub}, GatewayID: "gw_1"}
	signedBundle(t, store, priv, 1, `{"rules": [{"id": "pay", "on": "http", "effect": "approve", "on_unavailable": "allow"}]}`)
	q, _ := queue.Open(filepath.Join(t.TempDir(), "q.db"), 0)
	defer q.Close()
	_, gwKey, _ := ed25519.GenerateKey(nil)
	e := &Engine{Store: store, Queue: q, Approvals: &approvals.Manager{Client: &sync.Client{BaseURL: platform.URL, Kind: "gateway",
		ID: &sync.Identity{GatewayID: "gw_1", PrivateKey: gwKey}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c := e.Decide(ctx, policy.Request{"agent": "a", "on": "http", "at": 1793610000.0, "method": "POST", "path": "/pay"})
	if c.Allowed() || c.Decision.Reason != "approval wait ended" {
		t.Errorf("deadline while waiting: %+v", c.Decision)
	}
}

// A request still deciding with the previous bundle mustn't wipe the counts the new bundle keeps.
func TestCountsSurviveBundleSwitches(t *testing.T) {
	e := newEngine(t)
	old := e.Store.Current()
	pub, priv, _ := ed25519.GenerateKey(nil)
	e.Store.Keys = map[string]ed25519.PublicKey{"k1": pub}
	pol := `{"rules": [{"id": "new-cap", "on": "http", "effect": "allow", "limit": {"window_s": 60, "max": 2}}]}`
	signedBundle(t, e.Store, priv, 2, pol)
	cur := e.Store.Current()
	ask := func(b *bundle.Bundle, i int) string {
		d, _ := e.decide(context.Background(), b, "a", policy.Request{"agent": "a", "on": "http", "at": 1793610000.0 + float64(i),
			"method": "GET", "path": "/x"}, false)
		return d.Decision
	}
	got := []string{ask(cur, 0), ask(old, 1), ask(cur, 2), ask(old, 3), ask(cur, 4)}
	if got[0] != "allow" || got[2] != "allow" || got[4] != "rate_limited" {
		t.Errorf("new-cap allows 2 a minute whatever the old bundle's requests did: %v", got)
	}
}

// Go's decoder, and others, match keys ignoring case: the policy must see the copy the upstream acts on.
func TestParseObjectCaseFold(t *testing.T) {
	for _, body := range []string{`{"amount": 50, "AMOUNT": 5000}`, `{"amount": 50, "Amount": 5000}`,
		`{"a": {"key": 1, "Key": 2}}`, `{"list": [{"s": 1, "ſ": 2}]}`} {
		if _, err := ParseObject([]byte(body)); err == nil {
			t.Errorf("%s: want refused", body)
		}
	}
	if _, err := ParseObject([]byte(`{"amount": 50, "amounts": 1, "nested": {"amount": 2}}`)); err != nil {
		t.Errorf("different keys, or the same key at different depths, are fine: %v", err)
	}
}

// on_unavailable is for a platform that can't be reached. One that answers 429 (or another 4xx) is there and refused:
// otherwise an agent floods approval requests until it's rate-limited, then goes ahead unapproved.
func TestApprovalRefusedIsNotUnavailable(t *testing.T) {
	for _, c := range []struct {
		name    string
		status  int // 0: the platform can't be reached
		allowed bool
	}{{"429", 429, false}, {"422", 422, false}, {"unreachable", 0, true}, {"503", 503, true}} {
		platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(c.status) }))
		url := platform.URL
		if c.status == 0 {
			platform.Close()
		}
		pub, priv, _ := ed25519.GenerateKey(nil)
		store := &bundle.Store{Path: filepath.Join(t.TempDir(), "b.json"), Keys: map[string]ed25519.PublicKey{"k1": pub}, GatewayID: "gw_1"}
		signedBundle(t, store, priv, 1, `{"rules": [{"id": "pay", "on": "http", "effect": "approve", "on_unavailable": "allow"}]}`)
		q, _ := queue.Open(filepath.Join(t.TempDir(), "q.db"), 0)
		_, gwKey, _ := ed25519.GenerateKey(nil)
		e := &Engine{Store: store, Queue: q, Approvals: &approvals.Manager{Client: &sync.Client{BaseURL: url, Kind: "gateway",
			ID: &sync.Identity{GatewayID: "gw_1", PrivateKey: gwKey}}}}
		d := e.Decide(context.Background(), policy.Request{"agent": "a", "on": "http", "at": 1793610000.0, "method": "POST", "path": "/pay"})
		if d.Allowed() != c.allowed {
			t.Errorf("platform %s: %+v", c.name, d.Decision)
		}
		q.Close()
		platform.Close()
	}
}

// The agent picks a limit's per values: they mustn't decide how much memory a count's key holds.
func TestCountKeysAreSmall(t *testing.T) {
	e := newEngine(t)
	huge := strings.Repeat("x", 1<<20)
	c := e.Decide(context.Background(), policy.Request{"agent": "a", "on": "k8s", "at": 1793610000.0, "verb": "delete", "namespace": huge})
	c.Done("", nil, 0)
	if !c.Allowed() || len(e.counts) == 0 {
		t.Fatalf("decision %v, %d counts", c.Decision, len(e.counts))
	}
	for k := range e.counts {
		if len(k) > 64 {
			t.Errorf("a count's key is %d bytes", len(k))
		}
	}
}
