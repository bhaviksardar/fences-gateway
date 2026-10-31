// Package hook decides tool calls an agent reports before making them itself, such as Claude Code's built-in tools
// through a PreToolUse hook (cmd/fences-hook). Nothing is forwarded: the gateway only decides, holds the call while
// an approval is pending, and records the decision.
package hook

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/bhaviksardar/fences-gateway/internal/engine"
	"github.com/bhaviksardar/fences-gateway/pkg/policy"
)

// Reply is the decision: Allowed, or the Message to show the agent.
type Reply struct {
	Allowed  bool   `json:"allowed"`
	Decision string `json:"decision"`
	Message  string `json:"message,omitempty"`
}

// Handler serves POST /hook/{name}: {"tool": "Bash", "args": {...}} with the agent's Fences token as a Bearer
// token. The request is decided as an mcp request to server {name}
// (spec/rules/README.md, Requests), so the agent must be assigned {name} as an upstream.
func Handler(e *engine.Engine) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		agent, ok := e.Agent(r.Header.Get("Authorization"), name)
		if !ok {
			e.Unauthenticated(policy.Request{"on": "mcp", "at": now(), "server": name, "tool": ""})
			w.Header().Set("WWW-Authenticate", `Bearer realm="fences"`)
			http.Error(w, "unknown credential, or not assigned "+name, http.StatusUnauthorized)
			return
		}
		// Repeated keys are refused: decided on one copy, acted on another
		raw, err := io.ReadAll(r.Body)
		var body map[string]any
		if err == nil {
			body, err = engine.ParseObject(raw)
		}
		tool, _ := body["tool"].(string)
		args, isObject := body["args"].(map[string]any)
		if body["args"] == nil {
			args, isObject = map[string]any{}, true
		}
		if err != nil || tool == "" || !isObject {
			http.Error(w, `want {"tool": "...", "args": {...}} without repeated keys`, http.StatusBadRequest)
			return
		}
		call := e.Decide(r.Context(), policy.Request{"agent": agent, "on": "mcp", "at": now(), "server": name, "tool": tool, "args": args})
		call.Done("", nil, 0) // not forwarded: the agent runs it, or doesn't
		reply := Reply{Allowed: call.Allowed(), Decision: string(call.Decision.Decision)}
		if !reply.Allowed {
			reply.Message = call.Message()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(reply)
	})
}

func now() float64 { return float64(time.Now().UnixMilli()) / 1000 }
