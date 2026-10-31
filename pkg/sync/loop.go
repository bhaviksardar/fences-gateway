package sync

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"math/rand/v2"
	gosync "sync"
	"time"

	"github.com/bhaviksardar/fences-gateway/pkg/bundle"
	"github.com/bhaviksardar/fences-gateway/pkg/queue"
)

// Backoff after a failed request: 1 s doubling to 5 minutes, with full jitter. After 5 failures in a row the circuit
// opens and it tries once a minute until a request succeeds (spec/PROTOCOL-GATEWAY.md, Sync). Variables so tests can
// shrink them.
var (
	firstBackoff = time.Second
	maxBackoff   = 5 * time.Minute
	openCircuit  = time.Minute
	revokedLog   = time.Minute
)

const failuresToOpen = 5

func backoff(failures int) time.Duration {
	if failures >= failuresToOpen {
		return openCircuit
	}
	d := min(firstBackoff<<(failures-1), maxBackoff)
	return rand.N(d) + 1
}

// Command is one command from a sync reply. Raw is the whole command, for the fields its Type adds.
type Command struct {
	Type string          `json:"type"`
	ID   string          `json:"id"`
	Raw  json.RawMessage `json:"-"`
}

// Ack acknowledges a command. Extra holds fields beyond id and type, such as containment and detail.
type Ack struct {
	ID         string
	Type       string
	ReceivedAt float64
	Extra      map[string]any
}

func (a Ack) MarshalJSON() ([]byte, error) {
	m := map[string]any{"id": a.ID, "type": a.Type}
	if a.ReceivedAt != 0 {
		m["received_at"] = a.ReceivedAt
	}
	for k, v := range a.Extra {
		m[k] = v
	}
	return json.Marshal(m)
}

// Syncer runs the sync loop: a heartbeat with health and acks, a newer bundle when there is one, and commands.
type Syncer struct {
	Client *Client
	Store  *bundle.Store
	// Health is the health object to send; Syncer sets its bundle_error.
	Health func() map[string]any
	// Apply carries out a command. Each command is applied once, in the order listed, and acknowledged in the next
	// sync, as often as the platform re-sends it. It runs inside the sync loop, so it must not block: start slow work
	// (a containment action) in a goroutine and report the result with Ack.
	Apply func(Command)
	// Urgent says an approval is pending, so the next sync comes in 2 s rather than sync_interval_s.
	Urgent func() bool
	Log    *slog.Logger

	mu         gosync.Mutex
	acks       []Ack
	applied    map[string]bool // ponytail: grows by one id per command for the life of the process
	bundleErr  error
	wake       chan struct{}
	initialize gosync.Once
}

func (s *Syncer) init() {
	s.initialize.Do(func() {
		s.wake = make(chan struct{}, 1)
		s.applied = map[string]bool{}
		if s.Log == nil {
			s.Log = slog.Default()
		}
	})
}

// Ack queues an acknowledgement for the next sync and makes that sync happen now. Use it for a command's second ack
// (a containment result); receipt acks are sent for you.
func (s *Syncer) Ack(a Ack) {
	s.init()
	s.mu.Lock()
	s.acks = append(s.acks, a)
	s.mu.Unlock()
	s.Wake()
}

// Wake makes the next sync happen now.
func (s *Syncer) Wake() {
	s.init()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run syncs until ctx ends. If the platform revokes this client's key it stops syncing and logs an error every
// minute; the store keeps enforcing the last good bundle either way.
func (s *Syncer) Run(ctx context.Context) {
	s.init()
	failures := 0
	for {
		err := s.Once(ctx)
		var wait time.Duration
		switch {
		case errors.Is(err, ErrRevoked):
			s.revoked(ctx)
			return
		case err != nil:
			failures++
			wait = backoff(failures)
			s.Log.Warn("fences sync failed", "err", err, "failures", failures, "retry_in", wait)
		default:
			failures = 0
			wait = s.interval()
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-time.After(wait):
		}
	}
}

func (s *Syncer) interval() time.Duration {
	if s.Urgent != nil && s.Urgent() {
		return 2 * time.Second
	}
	every := s.Client.ID.SyncIntervalS
	if every <= 0 {
		every = 15
	}
	return time.Duration(every * (0.8 + 0.4*rand.Float64()) * float64(time.Second)) // ±20%
}

func (s *Syncer) revoked(ctx context.Context) {
	for {
		s.Log.Error("fences: the platform refused this client's key; enforcing the last good bundle, not syncing")
		select {
		case <-ctx.Done():
			return
		case <-time.After(revokedLog):
		}
	}
}

// Once runs one sync: it sends acks and health, applies a newer bundle and the commands in the reply.
func (s *Syncer) Once(ctx context.Context) error {
	s.init()
	s.mu.Lock()
	acks := s.acks
	bundleErr := s.bundleErr
	s.mu.Unlock()

	health := map[string]any{}
	if s.Health != nil {
		health = maps.Clone(s.Health()) // the caller's map isn't ours to change
	}
	health["bundle_error"] = nil
	if bundleErr != nil {
		health["bundle_error"] = bundleErr.Error()
	}
	var version int64
	if b := s.Store.Current(); b != nil {
		version = b.Version
	}
	var reply struct {
		Bundle   json.RawMessage   `json:"bundle"`
		Commands []json.RawMessage `json:"commands"`
	}
	req := map[string]any{"bundle_version": version, "acks": append([]Ack{}, acks...), "health": health}
	if err := s.Client.Post(ctx, "/api/gateways/sync", req, &reply); err != nil {
		return err
	}
	s.mu.Lock()
	s.acks = s.acks[len(acks):] // sent; acks added meanwhile wait for the next sync
	s.mu.Unlock()

	if len(reply.Bundle) > 0 && string(reply.Bundle) != "null" {
		err := s.Store.Apply(reply.Bundle)
		if err != nil {
			s.Log.Error("fences: problem with the new bundle", "err", err)
		}
		s.mu.Lock()
		s.bundleErr = err
		s.mu.Unlock()
	}
	received := false
	for _, raw := range reply.Commands {
		var c Command
		if json.Unmarshal(raw, &c) != nil || c.ID == "" || c.Type == "" {
			continue // a malformed command is skipped; the ones after it still apply
		}
		c.Raw = raw
		s.mu.Lock()
		first := !s.applied[c.ID]
		s.applied[c.ID] = true
		s.acks = append(s.acks, Ack{ID: c.ID, Type: c.Type, ReceivedAt: float64(time.Now().UnixMilli()) / 1000})
		s.mu.Unlock()
		if first && s.Apply != nil {
			s.Apply(c)
		}
		received = true
	}
	if received {
		s.Wake() // receipt acks go out at once
	}
	return nil
}

// Ship sends queued events to the platform until ctx ends: every 2 s, or as soon as 100 are waiting, up to 500 per
// batch (spec/PROTOCOL-GATEWAY.md, Decision events). Events stay queued until the platform acknowledges them.
func Ship(ctx context.Context, c *Client, q *queue.Queue, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	failures := 0
	last := time.Now()
	for {
		// ponytail: polls the queue depth every 100 ms; a channel from Push if that ever shows up in a profile
		wait := 100 * time.Millisecond
		if failures > 0 {
			wait = backoff(failures)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if q.Depth() == 0 || (failures == 0 && q.Depth() < 100 && time.Since(last) < 2*time.Second) {
			continue
		}
		last = time.Now()
		if err := shipOnce(ctx, c, q, log); err != nil {
			if errors.Is(err, ErrRevoked) {
				return // Syncer logs it; events stay queued
			}
			failures++
			log.Warn("fences: sending decision events failed", "err", err, "failures", failures)
			continue
		}
		failures = 0
	}
}

func shipOnce(ctx context.Context, c *Client, q *queue.Queue, log *slog.Logger) error {
	events, err := q.Batch(500)
	if err != nil || len(events) == 0 {
		return err
	}
	var last struct {
		Seq int64 `json:"seq"`
	}
	json.Unmarshal(events[len(events)-1], &last)
	var reply struct {
		AckedSeq *int64 `json:"acked_seq"`
	}
	err = c.post(ctx, "/api/gateways/decisions", map[string]any{"events": events}, &reply, true)
	if err == errRepeat { // the platform has the whole batch already
		return q.Ack(last.Seq)
	}
	if errors.Is(err, ErrForbidden) {
		// The batch named an agent this gateway doesn't serve: drop it (spec, Conventions), or it would block the rest
		log.Error("fences: the platform refused a batch of decision events for an agent this gateway doesn't serve; dropped",
			"events", len(events), "up_to_seq", last.Seq)
		return q.Ack(last.Seq)
	}
	if err != nil {
		return err
	}
	if reply.AckedSeq == nil { // without it, nothing is known to be stored
		return errors.New("sync: the decisions reply has no acked_seq")
	}
	return q.Ack(*reply.AckedSeq)
}
