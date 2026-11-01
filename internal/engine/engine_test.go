package engine

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"math/rand/v2"
	"path/filepath"
	"testing"

	"github.com/bhaviksardar/fences-gateway/pkg/bundle"
	"github.com/bhaviksardar/fences-gateway/pkg/policy"
	"github.com/bhaviksardar/fences-gateway/pkg/queue"
)

// The engine keeps running counts for limits instead of every allowed request. Random traffic must be decided
// exactly as pkg/policy decides it from the full history.
func TestCountsMatchHistory(t *testing.T) {
	pol := `{"default": "allow", "rules": [
		{"id": "restarts", "on": "k8s", "effect": "allow", "where": [{"field": "verb", "op": "is", "value": "delete"}],
		 "limit": {"window_s": 60, "max": 3, "per": ["namespace"]}},
		{"id": "spend", "on": "http", "effect": "allow", "where": [{"field": "path", "op": "is", "value": "/refunds"}],
		 "limit": {"window_s": 30, "max": 500, "sum": "body.amount"}},
		{"id": "calls", "on": "http", "effect": "allow", "limit": {"window_s": 10, "max": 20}}]}`
	payload := `{"version": 1, "gateway_id": "gw_1", "policy": ` + pol + `, "agents": {}}`
	pub, priv, _ := ed25519.GenerateKey(nil)
	store := &bundle.Store{Path: filepath.Join(t.TempDir(), "b.json"), Keys: map[string]ed25519.PublicKey{"k1": pub}, GatewayID: "gw_1"}
	env, _ := json.Marshal(bundle.Envelope{KeyID: "k1", Payload: base64.StdEncoding.EncodeToString([]byte(payload)),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(payload)))})
	if err := store.Apply(env); err != nil {
		t.Fatal(err)
	}
	q, _ := queue.Open(filepath.Join(t.TempDir(), "q.db"), 0)
	defer q.Close()
	e := &Engine{Store: store, Queue: q}
	ref := store.Current().Policy

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
		c := e.Decide(context.Background(), req)
		c.Done("", nil, 0)
		if c.Decision != want {
			t.Fatalf("request %d %v: engine %v, policy from history %v", i, req, c.Decision, want)
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
