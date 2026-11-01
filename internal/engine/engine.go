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
	"github.com/bhaviksardar/fences-gateway/internal/shared"
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
	ReportValues bool          // report argument values in events, not just names
	Shared       *shared.Store // limits counted with the group's other replicas; nil: here only
	Version      string
	Log          *slog.Logger

	mu           gosync.Mutex
	counts       map[string]*count       // limit counts: rule signature + limit key → what it let through, for limits
	sigs         map[*policy.Rule]string // the current bundle's limited rules' signatures
	sigsFor      *bundle.Bundle
	swept        time.Time
	sharedErr    string
	sharedLogged time.Time
	quarantined  map[string]int64           // agent → the bundle version that takes over from the quarantine command
	held         map[string]map[*int]func() // agent → cancels for calls waiting on an approval
	later        map[string]*later          // agent + request_hash → an approval asked for a request that couldn't wait
	unauthSec    int64
	unauthN      int

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
	code, _ := status.(int)
	c.e.stats.upstream(upstream, status == "error" || code >= 500)
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

// ServiceAccount is the agent whose k8s_service_account credential is username, the name the Kubernetes API server
// authenticated (system:serviceaccount:<namespace>:<name>). A Kubernetes agent needs no upstream assignment: its
// credential in this gateway's bundle is the assignment.
func (e *Engine) ServiceAccount(username string) (string, bool) {
	b := e.Store.Current()
	if b == nil || username == "" {
		return "", false
	}
	for name, a := range b.Agents {
		for _, c := range a.Credentials {
			if c.Type == "k8s_service_account" && c.Name == username {
				return name, true
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

	d, degraded := e.decide(ctx, b, agent, req, false)

	var approvalID string
	waited := time.Duration(0)
	if on, _ := req["on"].(string); d.Decision == policy.RequireApproval && on == "k8s" {
		d, degraded, approvalID = e.approveLater(ctx, b, agent, req, d)
	} else if d.Decision == policy.RequireApproval {
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

// decide decides with the limits counted here, or in the shared store with one (spec/PROTOCOL-GATEWAY.md, Replicas).
// The store is asked outside the lock: it's atomic itself, and a round trip mustn't hold up every other decision.
func (e *Engine) decide(ctx context.Context, b *bundle.Bundle, agent string, req policy.Request, approved bool) (policy.Decision, bool) {
	e.mu.Lock()
	if e.Shared == nil || b == nil {
		defer e.mu.Unlock()
		return e.decideLocked(b, agent, req, approved)
	}
	if v, ok := e.quarantined[agent]; ok && b.Version < v {
		e.mu.Unlock()
		return policy.Decision{Decision: policy.Deny, Reason: agent + " is quarantined"}, false
	}
	var limits []shared.Limit
	var rules []*policy.Rule
	for r, sig := range e.signatures(b) {
		if r.CountsToward(req) {
			amount := 0.0
			if r.Limit.Sum != "" {
				amount, _ = r.Amount(req)
			}
			limits = append(limits, shared.Limit{Key: sig + "\x00" + r.LimitKey(req), WindowS: r.Limit.WindowS, Amount: amount})
			rules = append(rules, r)
		}
	}
	e.mu.Unlock()
	if len(limits) == 0 { // nothing to count
		return b.Policy.DecideCounts(req, func(*policy.Rule, policy.Request) (int, float64) { return 0, 0 }, approved), false
	}
	at, _ := req["at"].(float64)
	member, counts, err := e.Shared.Reserve(ctx, at, limits)
	e.sharedResult(err)
	if err != nil { // the store is unreachable: count here, and say the decision was made by the fail mode
		e.mu.Lock()
		defer e.mu.Unlock()
		d, _ := e.decideLocked(b, agent, req, approved)
		return d, true
	}
	d := b.Policy.DecideCounts(req, func(r *policy.Rule, _ policy.Request) (int, float64) {
		for i := range rules {
			if rules[i] == r {
				return counts[i].N, counts[i].Total
			}
		}
		return 0, 0
	}, approved)
	if d.Decision != policy.Allow { // only allowed requests count; one left in by a failed release only makes it stricter
		e.sharedResult(e.Shared.Release(context.WithoutCancel(ctx), member, limits))
	}
	return d, false
}

// sharedResult keeps the shared store's state for health, and logs a failure at most once a minute.
func (e *Engine) sharedResult(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil {
		e.sharedErr = ""
		return
	}
	e.sharedErr = err.Error()
	if time.Since(e.sharedLogged) > time.Minute {
		e.sharedLogged = time.Now()
		e.log().Warn("fences: shared limits unreachable; counting locally (degraded)", "err", err)
	}
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
	d := b.Policy.DecideCounts(req, e.count(b), approved)
	if d.Decision == policy.Allow {
		e.reserve(b, req)
	}
	return d, false
}

// count is what one limit let through for one key: allowed requests' times and amounts, oldest first.
type count struct {
	at, amount []float64
	total      float64
}

// expire drops what fell out of the window ending at now.
func (c *count) expire(now, window float64) {
	i := 0
	for i < len(c.at) && c.at[i] <= now-window {
		c.total -= c.amount[i]
		i++
	}
	c.at, c.amount = c.at[i:], c.amount[i:]
	if len(c.at) == 0 {
		c.total = 0 // no drift carried into the next requests
	}
}

// signatures names each limited rule by what decides which requests it counts: its id, subject, conditions, sum and
// per. A bundle that changes those starts that limit's count afresh; one that changes only max or window_s doesn't.
// ponytail: the spec counts every allowed request against a changed rule; counting from the change is what an engine
// can do without keeping every request (spec/rules/README.md, Limits)
func (e *Engine) signatures(b *bundle.Bundle) map[*policy.Rule]string {
	if e.sigsFor != b {
		e.sigs, e.sigsFor = map[*policy.Rule]string{}, b
		for i := range b.Policy.Rules {
			if r := &b.Policy.Rules[i]; r.Limit != nil {
				sig, _ := json.Marshal([]any{r.ID, r.On, r.Where, r.Limit.Sum, r.Limit.Per})
				e.sigs[r] = string(sig)
			}
		}
		// Counts of rules that are gone go in reserve's sweep, not here: a request still deciding with the previous
		// bundle would switch back, and each switch would wipe the other bundle's counts
	}
	return e.sigs
}

func (e *Engine) count(b *bundle.Bundle) policy.Counts {
	sigs := e.signatures(b)
	return func(r *policy.Rule, req policy.Request) (int, float64) {
		c := e.counts[sigs[r]+"\x00"+r.LimitKey(req)]
		if c == nil {
			return 0, 0
		}
		now, _ := req["at"].(float64)
		c.expire(now, r.Limit.WindowS)
		return len(c.at), c.total
	}
}

// reserve counts an allowed request against every limit it falls under.
// ponytail: in memory and per replica, so limits are per replica and restart from zero; 16 bytes a counted request
func (e *Engine) reserve(b *bundle.Bundle, req policy.Request) {
	sigs := e.signatures(b)
	at, _ := req["at"].(float64)
	if e.counts == nil {
		e.counts = map[string]*count{}
	}
	for r, sig := range sigs {
		if !r.CountsToward(req) {
			continue
		}
		key := sig + "\x00" + r.LimitKey(req)
		c := e.counts[key]
		if c == nil {
			c = &count{}
			e.counts[key] = c
		}
		amount := 0.0
		if r.Limit.Sum != "" {
			amount, _ = r.Amount(req)
		}
		c.at, c.amount, c.total = append(c.at, at), append(c.amount, amount), c.total+amount
	}
	if time.Since(e.swept) > time.Minute { // keys nobody asks about again would otherwise keep their entries
		e.swept = time.Now()
		windows := map[string]float64{}
		for r, sig := range sigs {
			windows[sig] = r.Limit.WindowS
		}
		for k, c := range e.counts {
			if c.expire(at, windows[k[:strings.IndexByte(k, 0)]]); len(c.at) == 0 {
				delete(e.counts, k)
			}
		}
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
	case ctx.Err() != nil: // a quarantine, shutdown, or the caller gone or out of time: never "the platform is down"
		return policy.Decision{Decision: policy.Deny, Rule: d.Rule, Reason: "approval wait ended"}, false, id
	case err != nil:
		if denied, ok := e.unavailable(b, d, err); !ok {
			return denied, true, id
		}
		degraded = true
	case !answer.Granted:
		reason := "not approved"
		if answer.Note != "" {
			reason += ": " + answer.Note
		}
		return policy.Decision{Decision: policy.Deny, Rule: d.Rule, Reason: reason}, false, id
	}
	d, deg := e.decide(ctx, e.Store.Current(), agent, req, true)
	return d, degraded || deg, id
}

// unavailable is the fail mode when the platform can't be asked for an approval: the rule's on_unavailable decides
// (spec/PROTOCOL-GATEWAY.md, Fail mode). ok means go ahead as if approved.
func (e *Engine) unavailable(b *bundle.Bundle, d policy.Decision, err error) (policy.Decision, bool) {
	e.log().Warn("fences: approval request failed", "err", err)
	if rule := ruleByID(b, d.Rule); rule == nil || rule.OnUnavailable != policy.Allow {
		return policy.Decision{Decision: policy.Deny, Rule: d.Rule, Reason: "approval needed, and Fences can't be reached"}, false
	}
	return d, true
}

// GrantTTL is how long a granted approval waits for its request to come back (spec/PROTOCOL-GATEWAY.md, Approvals).
const GrantTTL = 10 * time.Minute

type later struct {
	id       string
	answered bool
	granted  bool
	reason   string
	at       time.Time
}

// approveLater is approval for a surface that can't hold a request open: the first request asks and is refused at
// once, naming the approval; the same request (same request_hash, same agent) within GrantTTL of the grant goes ahead.
// Each grant is used once.
func (e *Engine) approveLater(ctx context.Context, b *bundle.Bundle, agent string, req policy.Request, d policy.Decision) (policy.Decision, bool, string) {
	key := agent + "\x00" + decisions.RequestHash(req)
	waiting := func(id string) policy.Decision {
		msg := "Waiting for approval"
		if id != "" {
			msg += " " + id
		}
		if r := strings.TrimSuffix(d.Reason, "."); r != "" {
			msg += ": " + r
		}
		return policy.Decision{Decision: policy.RequireApproval, Rule: d.Rule, Reason: msg + ". Retry once it's approved."}
	}
	e.mu.Lock()
	if e.later == nil {
		e.later = map[string]*later{}
	}
	switch a := e.later[key]; {
	case a != nil && a.granted && time.Since(a.at) <= GrantTTL:
		delete(e.later, key)
		e.mu.Unlock()
		d, degraded := e.decide(ctx, e.Store.Current(), agent, req, true)
		return d, degraded, a.id
	case a != nil && !a.answered:
		e.mu.Unlock()
		return waiting(a.id), false, a.id
	case a != nil && !a.granted: // refused or timed out: said once, and the next retry asks again
		delete(e.later, key)
		e.mu.Unlock()
		return policy.Decision{Decision: policy.Deny, Rule: d.Rule, Reason: a.reason}, false, a.id
	}
	a := &later{} // in place before asking, so the same request at the same time doesn't ask twice
	e.later[key] = a
	e.mu.Unlock()

	timeout := time.Duration(b.ApprovalTimeoutS * float64(time.Second))
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	on, _ := req["on"].(string)
	id, answer, stop, err := e.Approvals.Send(ctx, approvals.Request{
		Agent: agent, On: on, Target: decisions.Target(req), Rule: d.Rule, Reason: d.Reason,
		Summary: decisions.Summary(req), RequestHash: decisions.RequestHash(req),
	}, timeout)
	if err != nil {
		e.mu.Lock()
		delete(e.later, key)
		e.mu.Unlock()
		if denied, ok := e.unavailable(b, d, err); !ok {
			return denied, true, id
		}
		d, _ := e.decide(ctx, e.Store.Current(), agent, req, true)
		return d, true, id
	}
	e.mu.Lock()
	a.id = id
	e.mu.Unlock()
	go func() {
		defer stop()
		reason := "approval timed out"
		var ans approvals.Answer
		select {
		case ans = <-answer:
			reason = "not approved"
			if ans.Note != "" {
				reason += ": " + ans.Note
			}
		case <-time.After(timeout):
		}
		e.mu.Lock()
		a.answered, a.granted, a.reason, a.at = true, ans.Granted, reason, time.Now()
		e.mu.Unlock()
		time.AfterFunc(GrantTTL, func() { // never retried: forget it
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.later[key] == a {
				delete(e.later, key)
			}
		})
	}()
	return waiting(id), false, id
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

// EndWaits ends every call waiting on an approval: each is denied ("approval wait ended"). For shutdown, so a restart
// doesn't wait on a person; the agent can ask again once the gateway is back.
func (e *Engine) EndWaits() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, held := range e.held {
		for _, cancel := range held {
			cancel()
		}
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
	h := map[string]any{
		"version": e.Version, "uptime_s": math.Round(uptime.Seconds()), "queue_depth": e.Queue.Depth(),
		"dropped_events": e.Queue.Dropped(), "decisions_per_s": perS, "p99_added_ms": p99, "upstream_errors": upstreamErrors,
	}
	if e.Shared != nil {
		e.mu.Lock()
		h["shared_limits"] = "ok"
		if e.sharedErr != "" {
			h["shared_limits"] = e.sharedErr
		}
		e.mu.Unlock()
	}
	return h
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
