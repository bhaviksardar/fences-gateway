// Package conformance runs the cases in spec/conformance/ against pkg/policy. conformance/reference.py runs the same
// cases against the reference engine.
package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bhaviksardar/fences-gateway/pkg/policy"
)

type suite struct {
	Kind   string          `json:"kind"`
	Policy json.RawMessage `json:"policy"`
	Cases  []struct {
		Name     string           `json:"name"`
		Policy   json.RawMessage  `json:"policy"`
		Request  policy.Request   `json:"request"`
		History  []policy.Request `json:"history"`
		Approved bool             `json:"approved"`
		Expect   map[string]any   `json:"expect"`
	} `json:"cases"`
}

func TestConformance(t *testing.T) {
	files, _ := filepath.Glob("../spec/conformance/*.json")
	total := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var s suite
		if err := json.Unmarshal(data, &s); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if s.Kind == "run" {
			continue // run limits are enforced by the SDKs and the platform, not the gateway (spec/rules, Run limits)
		}
		for _, c := range s.Cases {
			total++
			raw := c.Policy
			if raw == nil {
				raw = s.Policy
			}
			got := map[string]any{}
			if p, err := policy.Parse(raw); err != nil {
				got["error"] = "invalid_policy"
			} else {
				d := p.Decide(c.Request, c.History, c.Approved)
				got["decision"], got["rule"], got["reason"] = d.Decision, nullable(d.Rule), nullable(d.Reason)
			}
			for k, want := range c.Expect {
				if got[k] != want {
					t.Errorf("%s: %s\n  want %v\n  got  %v", filepath.Base(f), c.Name, c.Expect, got)
					break
				}
			}
		}
	}
	if total == 0 {
		t.Fatal("no conformance cases found")
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
