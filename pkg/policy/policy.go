// Package policy is the Fences rules engine: given a policy, a request and the requests already let through, it
// decides allow, deny, require_approval or rate_limited. The rules are spec/rules/README.md, and this engine must pass
// every case in spec/conformance/.
//
// It is a public API, imported by fences-edge at a tagged version: no gateway-only imports here.
package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // time windows work in images without a zoneinfo database
)

// Decisions, as reported in Decision.Decision.
const (
	Allow           = "allow"
	Deny            = "deny"
	RequireApproval = "require_approval"
	RateLimited     = "rate_limited"
)

// Policy is a parsed, validated policy. Build one with Parse; Decide relies on what Parse checks and compiles.
type Policy struct {
	Default     string   `json:"default,omitempty"`
	Quarantined []string `json:"quarantined,omitempty"`
	Rules       []Rule   `json:"rules,omitempty"`
}

// Rule is one rule of a policy (spec/rules/README.md, Rules).
type Rule struct {
	ID            string      `json:"id"`
	On            string      `json:"on"`
	Effect        string      `json:"effect"`
	Agents        []string    `json:"agents,omitempty"`
	Where         []Condition `json:"where,omitempty"`
	When          *Window     `json:"when,omitempty"`
	Limit         *Limit      `json:"limit,omitempty"`
	Reason        string      `json:"reason,omitempty"`
	OnUnavailable string      `json:"on_unavailable,omitempty"`
}

// Condition is one `where` entry: Field (a dotted path into the request) compared by Op with Value.
type Condition struct {
	Field string          `json:"field"`
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value"`

	text string
	list []string
	num  float64
	glob *regexp.Regexp
}

// Window is a rule's `when`: the days and time of day it applies, in TZ.
type Window struct {
	Days []string `json:"days,omitempty"`
	From string   `json:"from,omitempty"`
	To   string   `json:"to,omitempty"`
	TZ   string   `json:"tz,omitempty"`

	loc        *time.Location
	start, end int // minutes after midnight
}

// Limit is a rule's rate or amount cap.
type Limit struct {
	WindowS int64    `json:"window_s"`
	Max     *float64 `json:"max"`
	Sum     string   `json:"sum,omitempty"`
	Per     []string `json:"per,omitempty"`
}

// Request is what the engine decides on: "agent", "on" and "at" (Unix seconds), plus the subject's fields, as decoded
// from JSON (spec/rules/README.md, Requests). History entries have the same shape.
type Request = map[string]any

// Decision is the engine's answer. Rule is the id of the rule that decided it, "" when none did (quarantine or the
// policy's default).
type Decision struct {
	Decision string `json:"decision"`
	Rule     string `json:"rule,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

var (
	ruleID   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	decimal  = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)
	hhmm     = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	weekdays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}
)

type missingValue struct{}

var missing = missingValue{}

// Parse reads a policy and rejects anything spec/rules/schema.json rejects, plus duplicate rule ids. It never guesses.
func Parse(data []byte) (*Policy, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	if err := p.compile(); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	return &p, nil
}

func (p *Policy) compile() error {
	if p.Default != "" && p.Default != Allow && p.Default != Deny {
		return fmt.Errorf("bad default %q", p.Default)
	}
	seen := map[string]bool{}
	for i := range p.Rules {
		r := &p.Rules[i]
		if !ruleID.MatchString(r.ID) {
			return fmt.Errorf("missing or malformed id %q", r.ID)
		}
		if seen[r.ID] {
			return fmt.Errorf("duplicate id %q", r.ID)
		}
		seen[r.ID] = true
		if err := r.compile(); err != nil {
			return fmt.Errorf("%s: %w", r.ID, err)
		}
	}
	return nil
}

func (r *Rule) compile() error {
	switch r.On {
	case "mcp", "http", "k8s":
	default:
		return fmt.Errorf("bad on %q", r.On)
	}
	switch r.Effect {
	case Allow, Deny, "approve":
	default:
		return fmt.Errorf("bad effect %q", r.Effect)
	}
	if r.OnUnavailable != "" && r.OnUnavailable != Allow && r.OnUnavailable != Deny {
		return fmt.Errorf("bad on_unavailable %q", r.OnUnavailable)
	}
	if len(r.Reason) > 500 {
		return fmt.Errorf("reason over 500 characters")
	}
	for i := range r.Where {
		if err := r.Where[i].compile(); err != nil {
			return err
		}
	}
	if r.When != nil {
		if err := r.When.compile(); err != nil {
			return err
		}
	}
	if l := r.Limit; l != nil {
		if l.WindowS < 1 || l.Max == nil || *l.Max < 0 {
			return fmt.Errorf("limit needs window_s ≥ 1 and max ≥ 0")
		}
		for _, f := range append([]string{l.Sum}, l.Per...) {
			if f != "" && !validField(f) {
				return fmt.Errorf("bad field %q", f)
			}
		}
	}
	return nil
}

func validField(f string) bool {
	for _, part := range strings.Split(f, ".") {
		if part == "" {
			return false
		}
	}
	return true
}

func (c *Condition) compile() error {
	if !validField(c.Field) {
		return fmt.Errorf("bad field %q", c.Field)
	}
	var err error
	switch c.Op {
	case "is", "is_not", "contains", "not_contains", "matches":
		err = json.Unmarshal(c.Value, &c.text)
		if c.Op == "matches" && err == nil {
			c.glob = compileGlob(c.text)
		}
	case "in":
		err = json.Unmarshal(c.Value, &c.list)
	case "gt", "gte", "lt", "lte", "eq":
		err = json.Unmarshal(c.Value, &c.num)
	default:
		return fmt.Errorf("unknown op %q", c.Op)
	}
	if err != nil || c.Value == nil || string(c.Value) == "null" {
		return fmt.Errorf("bad value for %s on %q", c.Op, c.Field)
	}
	return nil
}

// compileGlob: * is any run (newlines too), ? is one character, everything else is literal. Not path.Match: no [...]
// classes, and * crosses slashes, so every engine agrees.
func compileGlob(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString(`(?s)^`)
	for _, ch := range pattern {
		switch ch {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	b.WriteString(`$`)
	return regexp.MustCompile(b.String())
}

func (w *Window) compile() error {
	if w.TZ == "Local" {
		return fmt.Errorf("tz must be an IANA name")
	}
	loc, err := time.LoadLocation(w.TZ) // "" is UTC
	if err != nil {
		return fmt.Errorf("bad tz %q", w.TZ)
	}
	w.loc, w.start, w.end = loc, 0, 24*60
	for _, d := range w.Days {
		if !slices.Contains(weekdays, d) {
			return fmt.Errorf("bad day %q", d)
		}
	}
	for _, t := range []struct {
		s   string
		dst *int
	}{{w.From, &w.start}, {w.To, &w.end}} {
		if t.s == "" {
			continue
		}
		if !hhmm.MatchString(t.s) {
			return fmt.Errorf("bad time %q", t.s)
		}
		h, _ := strconv.Atoi(t.s[:2])
		m, _ := strconv.Atoi(t.s[3:])
		*t.dst = h*60 + m
	}
	return nil
}

func (w *Window) contains(at float64) bool {
	sec, frac := math.Modf(at)
	t := time.Unix(int64(sec), int64(frac*1e9)).In(w.loc)
	minute := t.Hour()*60 + t.Minute()
	day := (int(t.Weekday()) + 6) % 7 // Monday is 0
	var inside bool
	switch {
	case w.end > w.start:
		inside = w.start <= minute && minute < w.end
	case minute >= w.start: // overnight, before midnight: the window started today
		inside = true
	default: // overnight, after midnight: it started yesterday
		inside, day = minute < w.end, (day+6)%7
	}
	return inside && (w.Days == nil || slices.Contains(w.Days, weekdays[day]))
}

// Decide applies the policy to req (spec/rules/README.md, Deciding). history is the requests already allowed, which
// limits count against; approved says whether req has a granted approval.
func (p *Policy) Decide(req Request, history []Request, approved bool) Decision {
	agent, _ := req["agent"].(string)
	if slices.Contains(p.Quarantined, agent) {
		return Decision{Decision: Deny, Reason: agent + " is quarantined"}
	}
	var hits []*Rule
	for i := range p.Rules {
		if r := &p.Rules[i]; r.matches(req) {
			hits = append(hits, r)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].ID < hits[j].ID })
	out := func(decision string, r *Rule) Decision { return Decision{decision, r.ID, r.Reason} }

	var passing []*Rule
	for _, r := range hits {
		if r.Effect == Deny {
			return out(Deny, r)
		}
		passing = append(passing, r)
	}
	for _, r := range passing {
		if r.Limit != nil && r.overLimit(req, history) {
			return out(RateLimited, r)
		}
	}
	if !approved {
		for _, r := range passing {
			if r.Effect == "approve" {
				return out(RequireApproval, r)
			}
		}
	}
	if len(passing) > 0 {
		return out(Allow, passing[0])
	}
	if p.Default == Allow {
		return Decision{Decision: Allow}
	}
	return Decision{Decision: Deny}
}

func (r *Rule) matches(req Request) bool {
	if on, _ := req["on"].(string); on != r.On {
		return false
	}
	if agent, _ := req["agent"].(string); len(r.Agents) > 0 && !slices.Contains(r.Agents, agent) {
		return false
	}
	if !r.where(req) {
		return false
	}
	if r.When != nil {
		at, ok := number(req["at"])
		return ok && r.When.contains(at)
	}
	return true
}

func (r *Rule) where(req Request) bool {
	for i := range r.Where {
		if !r.Where[i].holds(req) {
			return false
		}
	}
	return true
}

func (c *Condition) holds(req Request) bool {
	v := lookup(req, c.Field)
	switch c.Op {
	case "gt", "gte", "lt", "lte", "eq":
		n, ok := number(v)
		if !ok {
			return false
		}
		switch c.Op {
		case "gt":
			return n > c.num
		case "gte":
			return n >= c.num
		case "lt":
			return n < c.num
		case "lte":
			return n <= c.num
		}
		return n == c.num
	}
	s, isStr := v.(string)
	switch c.Op {
	case "is":
		return isStr && s == c.text
	case "is_not":
		return !(isStr && s == c.text)
	case "contains":
		return isStr && strings.Contains(s, c.text)
	case "not_contains":
		return !(isStr && strings.Contains(s, c.text))
	case "matches":
		return isStr && c.glob.MatchString(s)
	}
	return isStr && slices.Contains(c.list, s) // in
}

func (r *Rule) overLimit(req Request, history []Request) bool {
	l := r.Limit
	agent, _ := req["agent"].(string)
	now, _ := number(req["at"])
	key := make([]any, len(l.Per))
	for i, f := range l.Per {
		key[i] = lookup(req, f)
	}
	count, total := 0, 0.0
	for _, h := range history {
		hAt, ok := number(h["at"])
		hAgent, _ := h["agent"].(string)
		hOn, _ := h["on"].(string)
		if !ok || hAgent != agent || hOn != r.On || hAt <= now-float64(l.WindowS) || !r.where(h) {
			continue
		}
		same := true
		for i, f := range l.Per {
			same = same && reflect.DeepEqual(lookup(h, f), key[i])
		}
		if !same {
			continue
		}
		count++
		if l.Sum != "" {
			n, _ := number(lookup(h, l.Sum))
			total += n
		}
	}
	if l.Sum == "" {
		return float64(count+1) > *l.Max
	}
	amount, ok := number(lookup(req, l.Sum))
	return !ok || total+amount > *l.Max // an amount that can't be checked is over the limit
}

func lookup(req Request, path string) any {
	var v any = req
	for _, part := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return missing
		}
		if v, ok = m[part]; !ok {
			return missing
		}
	}
	return v
}

// number is a JSON number, or a string that is a plain decimal, so "1000" can't dodge `amount gt 100`.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		if decimal.MatchString(n) {
			f, err := strconv.ParseFloat(n, 64)
			return f, err == nil
		}
	}
	return 0, false
}
