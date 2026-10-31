// Package engine decides a request for any surface (MCP, HTTP, Kubernetes): it applies the current bundle and
// quarantines, keeps the history that limits count against, waits for approvals, and queues a decision event for
// every request.
package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	gosync "sync"
	"time"

	"github.com/bhaviksardar/fences-gateway/internal/approvals"
	"github.com/bhaviksardar/fences-gateway/pkg/bundle"
	"github.com/bhaviksardar/fences-gateway/pkg/decisions"
	"github.com/bhaviksardar/fences-gateway/pkg/policy"
	"github.com/bhaviksardar/fences-gateway/pkg/queue"
	"github.com/bhaviksardar/fences-gateway/pkg/sync"
)

// Engine is safe for concurrent use.
type Engine struct {
	Store        *bundle.Store
	Queue        *queue.Queue
	Approvals    *approvals.Manager
	ReportValues bool // report argument values in events, not just names
	Version      string
	Log          *slog.Logger

	mu          gosync.Mutex
	history     []policy.Request           // allowed requests, oldest first, for limits
	quarantined map[string]int64           // agent → the bundle version that takes over from the quarantine command
	held        map[string]map[*int]func() // agent → cancels for calls waiting on an approval
	unauthSec   int64
	unauthN     int

	stats stats
}

// Call is one decided request.
type Call struct {
	Decision policy.Decision
	e        *Engine
	req      policy.Request
	ev       decisions.Event
}

// Allowed says whether the request may go ahead.
func (c *Call) Allowed() bool { return c.Decision.Decision == policy.Allow }

// Message is what the agent is told when it isn't allowed.
func (c *Call) Message() string {
	msg := "Fences: " + c.Decision.Decision
	if c.Decision.Reason != "" {
		msg += ": " + c.Decision.Reason
	}
	return msg
}

// Done records the event. upstream is the upstream's name for its error count (or ""), status what it answered
// ("ok", "error", an HTTP status, or nil if not forwarded), and took how long it took.
func (c *Call) Done(upstream string, status any, took time.Duration) {
	c.ev.UpstreamStatus = status
	if status != nil {
		ms := float64(took.Microseconds()) / 1000
		c.ev.UpstreamMS = &ms
	}
	c.e.stats.upstream(upstream, status == "error")
	if _, err := c.e.Queue.Push(c.ev); err != nil {
		c.e.log().Error("fences: decision event not queued", "err", err)
	}
}

func (e *Engine) log() *slog.Logger {
	if e.Log == nil {
		return slog.Default()
	}
	return e.Log
}

// Agent is the agent whose Fences token is in the Authorization header (Bearer), and whether it may reach
// upstream (an MCP upstream, or a hook's name). The agent is found again on every request: a session id is never
// proof of who's calling. A quarantined agent is found too: each of its requests is denied and recorded.
func (e *Engine) Agent(authorization, upstream string) (string, bool) {
	token, ok := strings.CutPrefix(authorization, "Bearer ")
	b := e.Store.Current()
	if !ok || token == "" || b == nil {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	hash := []byte(hex.EncodeToString(sum[:]))
	for name, a := range b.Agents {
		for _, c := range a.Credentials {
			if c.Type == "token" && subtle.ConstantTimeCompare(hash, []byte(strings.ToLower(c.SHA256))) == 1 {
				return name, slices.Contains(a.Upstreams, upstream)
			}
		}
	}
	return "", false
}

// Unauthenticated records a request whose credential matched no agent, at most 10 a second: anyone can send these,
// and they mustn't push real decisions out of a full queue.
func (e *Engine) Unauthenticated(req policy.Request) {
	e.mu.Lock()
	sec := time.Now().Unix()
	if sec != e.unauthSec {
		e.unauthSec, e.unauthN = sec, 0
	}
	e.unauthN++
	skip := e.unauthN > 10
	e.mu.Unlock()
	if skip {
		return
	}
	(&Call{e: e, ev: e.event(req, policy.Decision{Decision: decisions.Unauthenticated, Reason: "unknown credential"})}).Done("", nil, 0)
}

// Decide decides req (agent, on, at and the subject's fields), waiting for an approval if the policy asks for one.
// Call Done on the result once the request has been forwarded, or straight away if it wasn't.
func (e *Engine) Decide(ctx context.Context, req policy.Request) *Call {
	start := time.Now()
	agent, _ := req["agent"].(string)
	b := e.Store.Current()

	e.mu.Lock()
	d, degraded := e.decideLocked(b, agent, req, false)
	e.mu.Unlock()

	var approvalID string
	waited := time.Duration(0)
	if d.Decision == policy.RequireApproval {
		t := time.Now()
		d, degraded, approvalID = e.approve(ctx, b, agent, req, d)
		waited = time.Since(t)
	}

	c := &Call{Decision: d, e: e, req: req, ev: e.event(req, d)}
	c.ev.Degraded = degraded
	if approvalID != "" {
		c.ev.ApprovalID = &approvalID
	}
	if b != nil {
		c.ev.BundleVersion = b.Version
	}
	took := time.Since(start) - waited // what the gateway added, not the person approving
	c.ev.LatencyMS = float64(took.Microseconds()) / 1000
	e.stats.decided(took)
	return c
}

// decideLocked decides and, if allowed, reserves the request in the history straight away, so two racing requests
// can't both take the last slot of a limit. Hold e.mu.
func (e *Engine) decideLocked(b *bundle.Bundle, agent string, req policy.Request, approved bool) (policy.Decision, bool) {
	if b == nil {
		return policy.Decision{Decision: policy.Deny, Reason: "no policy bundle yet"}, true
	}
	if v, ok := e.quarantined[agent]; ok && b.Version < v {
		return policy.Decision{Decision: policy.Deny, Reason: agent + " is quarantined"}, false
	}
	d := b.Policy.Decide(req, e.history, approved)
	if d.Decision == policy.Allow {
		e.reserve(b, req)
	}
	return d, false
}

func (e *Engine) reserve(b *bundle.Bundle, req policy.Request) {
	// ponytail: in memory and per replica, so limits are per replica and restart from zero; Phase 7 shares them
	var window float64
	for _, r := range b.Policy.Rules {
		if r.Limit != nil {
			window = max(window, r.Limit.WindowS)
		}
	}
	at, _ := req["at"].(float64)
	drop := 0
	for drop < len(e.history) {
		if hAt, _ := e.history[drop]["at"].(float64); hAt > at-window {
			break
		}
		drop++
	}
	e.history = e.history[drop:]
	if window > 0 {
		e.history = append(e.history, req)
	}
}

// approve asks for an approval and decides again with the answer, since limits may have changed meanwhile.
func (e *Engine) approve(ctx context.Context, b *bundle.Bundle, agent string, req policy.Request, d policy.Decision) (policy.Decision, bool, string) {
	timeout := time.Duration(b.ApprovalTimeoutS * float64(time.Second))
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	key := e.hold(agent, cancel)
	defer e.release(agent, key)

	on, _ := req["on"].(string)
	id, answer, err := e.Approvals.Ask(ctx, approvals.Request{
		Agent: agent, On: on, Target: decisions.Target(req), Rule: d.Rule, Reason: d.Reason,
		Summary: decisions.Summary(req), RequestHash: decisions.RequestHash(req),
	}, timeout)
	degraded := false
	switch {
	case errors.Is(err, approvals.ErrTimeout):
		return policy.Decision{Decision: policy.Deny, Rule: d.Rule, Reason: "approval timed out"}, false, id
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		return policy.Decision{Decision: policy.Deny, Rule: d.Rule, Reason: "approval wait ended"}, false, id
	case err != nil:
		// The platform can't be asked: the rule's on_unavailable decides (spec/PROTOCOL-GATEWAY.md, Fail mode)
		e.log().Warn("fences: approval request failed", "err", err)
		if rule := ruleByID(b, d.Rule); rule == nil || rule.OnUnavailable != policy.Allow {
			return policy.Decision{Decision: policy.Deny, Rule: d.Rule, Reason: "approval needed, and Fences can't be reached"}, true, id
		}
		degraded = true
	case !answer.Granted:
		reason := "not approved"
		if answer.Note != "" {
			reason += ": " + answer.Note
		}
		return policy.Decision{Decision: policy.Deny, Rule: d.Rule, Reason: reason}, false, id
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	d, deg := e.decideLocked(e.Store.Current(), agent, req, true)
	return d, degraded || deg, id
}

func ruleByID(b *bundle.Bundle, id string) *policy.Rule {
	for i := range b.Policy.Rules {
		if b.Policy.Rules[i].ID == id {
			return &b.Policy.Rules[i]
		}
	}
	return nil
}

func (e *Engine) hold(agent string, cancel func()) *int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.held == nil {
		e.held = map[string]map[*int]func(){}
	}
	if e.held[agent] == nil {
		e.held[agent] = map[*int]func(){}
	}
	key := new(int)
	e.held[agent][key] = cancel
	return key
}

func (e *Engine) release(agent string, key *int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.held[agent], key)
}

// Quarantine applies a quarantine command: every request from agent is denied until a bundle with at least
// bundleVersion decides, and calls waiting on an approval end now.
func (e *Engine) Quarantine(agent string, bundleVersion int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.quarantined == nil {
		e.quarantined = map[string]int64{}
	}
	e.quarantined[agent] = max(e.quarantined[agent], bundleVersion)
	for _, cancel := range e.held[agent] {
		cancel()
	}
}

// Quarantined says whether agent is quarantined, by the current bundle or by a command.
func (e *Engine) Quarantined(agent string) bool {
	b := e.Store.Current()
	e.mu.Lock()
	v, ok := e.quarantined[agent]
	e.mu.Unlock()
	return ok && (b == nil || b.Version < v) || b != nil && slices.Contains(b.Policy.Quarantined, agent)
}

// Apply carries out a command from the platform (sync.Syncer.Apply).
func (e *Engine) Apply(cmd sync.Command) {
	switch cmd.Type {
	case "quarantine":
		var q struct {
			Agent         string `json:"agent"`
			BundleVersion int64  `json:"bundle_version"`
		}
		if json.Unmarshal(cmd.Raw, &q) != nil || q.Agent == "" {
			return
		}
		if q.BundleVersion <= 0 { // not given: hold until the next bundle, never release with the current one
			if b := e.Store.Current(); b != nil {
				q.BundleVersion = b.Version + 1
			} else {
				q.BundleVersion = 1
			}
		}
		e.Quarantine(q.Agent, q.BundleVersion)
	case "approval":
		e.Approvals.Answer(cmd)
	}
}

func (e *Engine) event(req policy.Request, d policy.Decision) decisions.Event {
	ev := decisions.Event{
		At: float64(time.Now().UnixMilli()) / 1000, Target: decisions.Target(req),
		Arguments: decisions.Arguments(req, e.ReportValues), Decision: d.Decision,
	}
	ev.On, _ = req["on"].(string)
	if agent, ok := req["agent"].(string); ok && d.Decision != decisions.Unauthenticated {
		ev.Agent = &agent
	}
	if d.Rule != "" {
		ev.Rule = &d.Rule
	}
	if d.Reason != "" {
		ev.Reason = &d.Reason
	}
	return ev
}

// ParseObject decodes a JSON object for the policy to decide on, refusing a key repeated at any depth: parsers
// disagree on which copy wins, so the upstream could act on a value the policy never saw.
func ParseObject(data []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil || m == nil { // first: it also caps the nesting depth
		return nil, errors.New("must be a JSON object")
	}
	if err := noRepeats(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return nil, err
	}
	return m, nil
}

func noRepeats(dec *json.Decoder) error {
	t, err := dec.Token()
	if err != nil {
		return err
	}
	switch t {
	case json.Delim('{'):
		seen := map[string]bool{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return err
			}
			if key := k.(string); seen[key] {
				return fmt.Errorf("key %q is repeated", key)
			} else {
				seen[key] = true
			}
			if err := noRepeats(dec); err != nil {
				return err
			}
		}
	case json.Delim('['):
		for dec.More() {
			if err := noRepeats(dec); err != nil {
				return err
			}
		}
	default:
		return nil
	}
	_, err = dec.Token() // the closing } or ]
	return err
}

// Health is the health object for sync (spec/PROTOCOL-GATEWAY.md, Sync).
func (e *Engine) Health() map[string]any {
	perS, p99, upstreamErrors, uptime := e.stats.read()
	return map[string]any{
		"version": e.Version, "uptime_s": math.Round(uptime.Seconds()), "queue_depth": e.Queue.Depth(),
		"dropped_events": e.Queue.Dropped(), "decisions_per_s": perS, "p99_added_ms": p99, "upstream_errors": upstreamErrors,
	}
}

// stats are the gateway's metrics: decisions per second, added latency, upstream errors.
type stats struct {
	mu        gosync.Mutex
	since     time.Time // start of the current decisions-per-second count
	count     int
	latencies [1024]time.Duration // the most recent, for p99
	n         int
	upErrors  map[string]int
}

func (s *stats) decided(took time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
	s.latencies[s.n%len(s.latencies)] = took
	s.n++
}

func (s *stats) upstream(name string, failed bool) {
	if name == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upErrors == nil {
		s.upErrors = map[string]int{}
	}
	n := s.upErrors[name] // listed at 0 once seen, so health shows every upstream
	if failed {
		n++
	}
	s.upErrors[name] = n
}

// read returns decisions per second since the last read, p99 added latency in ms, upstream errors and uptime.
func (s *stats) read() (float64, float64, map[string]int, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	perS := 0.0
	if el := now.Sub(s.since).Seconds(); !s.since.IsZero() && el > 0 {
		perS = math.Round(float64(s.count)/el*10) / 10
	}
	s.count, s.since = 0, now
	recent := slices.Clone(s.latencies[:min(s.n, len(s.latencies))])
	slices.Sort(recent)
	p99 := 0.0
	if len(recent) > 0 {
		p99 = float64(recent[len(recent)*99/100].Microseconds()) / 1000
	}
	return perS, p99, maps.Clone(s.upErrors), now.Sub(started)
}

var started = time.Now()
