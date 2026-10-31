package sync_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/bhaviksardar/fences-gateway/pkg/bundle"
	"github.com/bhaviksardar/fences-gateway/pkg/decisions"
	"github.com/bhaviksardar/fences-gateway/pkg/policy"
	"github.com/bhaviksardar/fences-gateway/pkg/queue"
	"github.com/bhaviksardar/fences-gateway/pkg/sync"
)

// A client of the platform built from pkg/ alone: enroll once, keep the last good bundle, sync, decide locally, and
// ship a decision event for every request. fences-edge starts from this with Kind "device"; cmd/fences-gateway is
// the same shape.
func Example() {
	ctx := context.Background()
	dir := "/var/lib/fences-edge"
	client := &sync.Client{BaseURL: "https://fences.example.com", Kind: "device", Version: "0.1.0"}

	// 1. Identity: enrolled once with a one-time token, then read from disk (mode 0600)
	id, err := sync.LoadIdentity(filepath.Join(dir, "identity.json"))
	if errors.Is(err, os.ErrNotExist) {
		id, err = sync.Enroll(ctx, client, os.Getenv("FENCES_ENROLL_TOKEN"), "laptop-42", filepath.Join(dir, "identity.json"))
	}
	if err != nil {
		log.Fatal(err)
	}
	client.ID = id

	// 2. The last good bundle, verified against the platform's keys; with none, deny everything
	keys := map[string]ed25519.PublicKey{}
	for k, v := range id.SigningKeys {
		keys[k] = v
	}
	store := &bundle.Store{Path: filepath.Join(dir, "bundle.json"), Keys: keys, GatewayID: id.GatewayID}
	if err := store.Load(); err != nil {
		log.Print(err) // report it; keep running and wait for a sync
	}

	// 3. Events are queued on disk before the request is answered, and shipped in the background
	q, err := queue.Open(filepath.Join(dir, "events.db"), 0)
	if err != nil {
		log.Fatal(err)
	}
	go sync.Ship(ctx, client, q, nil)
	syncer := &sync.Syncer{Client: client, Store: store,
		Health: func() map[string]any { return map[string]any{"queue_depth": q.Depth(), "dropped_events": q.Dropped()} },
		Apply:  func(cmd sync.Command) { log.Printf("command %s %s", cmd.Type, cmd.ID) }} // quarantines and approvals
	go syncer.Run(ctx)

	// 4. Deciding a request: locally, from the bundle
	req := policy.Request{"agent": "claude-code", "on": "mcp", "at": float64(time.Now().Unix()), "server": "fs", "tool": "write_file",
		"args": map[string]any{"path": "/etc/hosts"}}
	d := policy.Decision{Decision: policy.Deny, Reason: "no policy bundle yet"}
	if b := store.Current(); b != nil {
		d = b.Policy.Decide(req, nil, false) // nil history: no limits counted in this sketch
	}
	agent := "claude-code"
	q.Push(decisions.Event{At: req["at"].(float64), Agent: &agent, On: "mcp", Target: decisions.Target(req),
		Arguments: decisions.Arguments(req, false), Decision: d.Decision})
}
