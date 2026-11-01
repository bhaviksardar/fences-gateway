// Command fences-hook is a Claude Code PreToolUse hook: it asks the local Fences gateway whether a tool call may go
// ahead and blocks it, with the reason shown to Claude, if not. Allowed calls carry on to Claude Code's own permission
// checks: the hook never approves anything by itself.
//
//	FENCES_GATEWAY_URL   the gateway, e.g. http://127.0.0.1:8080
//	FENCES_TOKEN         this developer's Fences agent token
//	FENCES_HOOK_NAME     the hook's name in rules (server), default claude-code
//	FENCES_HOOK_TIMEOUT_S  how long to wait for a decision, approvals included, default 590 (keep it under the
//	                     hook's timeout in Claude Code's settings, 600 by default: a hook that times out doesn't block)
//
// It fails closed: if the gateway can't be asked, the call is blocked. See docs/claude-code.md.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	in, err := io.ReadAll(os.Stdin)
	reason := ""
	if err != nil {
		reason = "Fences couldn't read the hook input: blocked."
	} else {
		reason = decide(in, os.Getenv)
	}
	if reason != "" {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason}})
	}
}

// decide asks the gateway about one PreToolUse input, and returns why the call is blocked, or "" if it may go ahead.
func decide(in []byte, getenv func(string) string) string {
	var hook struct {
		ToolName  string         `json:"tool_name"`
		ToolInput map[string]any `json:"tool_input"`
		Cwd       string         `json:"cwd"`
	}
	if err := json.Unmarshal(in, &hook); err != nil || hook.ToolName == "" {
		return "Fences couldn't read the hook input: blocked."
	}
	base, token := strings.TrimRight(getenv("FENCES_GATEWAY_URL"), "/"), getenv("FENCES_TOKEN")
	if base == "" || token == "" {
		return "Fences isn't set up for this session (FENCES_GATEWAY_URL and FENCES_TOKEN): blocked."
	}
	name := getenv("FENCES_HOOK_NAME")
	if name == "" {
		name = "claude-code"
	}
	timeout := 590 * time.Second
	if s, err := strconv.Atoi(getenv("FENCES_HOOK_TIMEOUT_S")); err == nil && s > 0 {
		timeout = time.Duration(s) * time.Second
	}
	project := getenv("CLAUDE_PROJECT_DIR")
	if project == "" {
		project = hook.Cwd
	}
	args := hook.ToolInput
	if args == nil {
		args = map[string]any{}
	}
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		if p, ok := args[k].(string); ok {
			args[k] = inProject(p, hook.Cwd, project)
		}
	}
	body, _ := json.Marshal(map[string]any{"tool": hook.ToolName, "args": args})
	req, err := http.NewRequest("POST", base+"/hook/"+url.PathEscape(name), bytes.NewReader(body))
	if err != nil {
		return fmt.Sprintf("Fences isn't set up right (FENCES_GATEWAY_URL: %v): blocked.", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return fmt.Sprintf("Fences couldn't be asked (%v): blocked.", err)
	}
	defer resp.Body.Close()
	var reply struct {
		Allowed bool   `json:"allowed"`
		Message string `json:"message"`
	}
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Sprintf("Fences refused the check (%d %s): blocked.", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return "Fences gave an answer that couldn't be read: blocked."
	}
	if reply.Allowed {
		return ""
	}
	if reply.Message == "" { // anything but an explicit allow blocks, with or without a reason
		return "Fences didn't allow this call: blocked."
	}
	return reply.Message // "Fences: deny: <the rule's reason>"
}

// inProject is a path as rules see it: relative if it's inside the project, absolute if it's outside, so a rule can
// match a write outside the project with "/*".
// ponytail: lexical only, so a symlink inside the project that points out of it still counts as inside.
func inProject(p, cwd, project string) string {
	if strings.HasPrefix(p, "~/") || p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	p = filepath.Clean(p)
	if project == "" {
		return p
	}
	if rel, err := filepath.Rel(filepath.Clean(project), p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		return rel
	}
	return p
}
