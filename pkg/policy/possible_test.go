package policy

import "testing"

func TestPossible(t *testing.T) {
	p, err := Parse([]byte(`{"quarantined": ["bad"], "rules": [
		{"id": "reads", "on": "mcp", "effect": "allow", "where": [{"field": "tool", "op": "matches", "value": "get_*"}]},
		{"id": "no-drop", "on": "mcp", "effect": "deny", "where": [{"field": "tool", "op": "is", "value": "get_secret"}]},
		{"id": "small-refunds", "on": "mcp", "effect": "allow", "where": [{"field": "tool", "op": "is", "value": "refund"}, {"field": "args.amount", "op": "lte", "value": 100}]},
		{"id": "no-big-deletes", "on": "mcp", "effect": "deny", "where": [{"field": "tool", "op": "is", "value": "get_users"}, {"field": "args.all", "op": "is", "value": "yes"}]},
		{"id": "night", "on": "mcp", "effect": "deny", "where": [{"field": "tool", "op": "is", "value": "get_logs"}], "when": {"from": "22:00", "to": "06:00"}},
		{"id": "ops-only", "on": "mcp", "effect": "allow", "agents": ["ops"], "where": [{"field": "tool", "op": "is", "value": "restart"}]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	for tool, want := range map[string]bool{
		"get_ticket": true,  // allowed
		"get_secret": false, // a deny that always matches
		"refund":     true,  // allowed for some arguments
		"get_users":  true,  // denied only for some arguments
		"get_logs":   true,  // denied only at night
		"restart":    false, // allowed for another agent only
		"delete":     false, // no rule, default deny
	} {
		if got := p.Possible(Request{"agent": "bot", "on": "mcp", "server": "crm", "tool": tool}); got != want {
			t.Errorf("%s: Possible = %v, want %v", tool, got, want)
		}
	}
	if p.Possible(Request{"agent": "bad", "on": "mcp", "server": "crm", "tool": "get_ticket"}) {
		t.Error("a quarantined agent can't do anything")
	}
	open, _ := Parse([]byte(`{"default": "allow", "rules": [{"id": "d", "on": "mcp", "effect": "deny", "where": [{"field": "server", "op": "is", "value": "prod"}]}]}`))
	if !open.Possible(Request{"agent": "bot", "on": "mcp", "server": "dev", "tool": "x"}) || open.Possible(Request{"agent": "bot", "on": "mcp", "server": "prod", "tool": "x"}) {
		t.Error("default allow: possible unless a deny always matches")
	}
}
