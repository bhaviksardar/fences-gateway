# Govern Claude Code with Fences

Claude Code can be governed in two ways, and you can use both:

- **Its MCP servers, through the gateway.** Claude Code connects to the Fences gateway instead of each MCP server. Tools it may never call are hidden, and every call is decided before it reaches the server.
- **Its built-in tools, through a hook.** `fences-hook` runs as a `PreToolUse` hook. Before Claude runs a shell command, writes a file or uses any other tool, the hook asks the gateway. If the call isn't allowed, Claude Code blocks it, and Claude is told why.

Either way, each developer is one agent in Fences. Their calls show up on the agent's page, approvals go to Slack, and a quarantine stops them at once.

## 1. In Fences

1. **Add an agent** for each developer, and choose **Third-party agent (Fences Gateway)** as its kind. Copy its key: it's that developer's `FENCES_TOKEN`.
2. On the agent's page, paste its **gateway rules**. [`examples/claude-code.json`](examples/claude-code.json) is a starting point (see [Example policy](#example-policy)).
3. In **Settings → Gateways**, make an enrollment token, start the gateway with it (step 2), then assign the agent the upstream **`claude-code`**, plus any MCP upstreams it should reach.
4. For approvals in Slack, in **Settings → Slack**, save the incoming webhook URL and your Slack app's signing secret. Set the app's Interactivity request URL to the one shown there.

## 2. The gateway

A developer can run one on their own machine, or a team can share one. To govern only the built-in tools, a gateway needs no upstreams:

```json
{"platform": "https://fences.example.com", "name": "alice-laptop", "data_dir": "fences-data", "listen": "127.0.0.1:8080"}
```

The first time, start it with the enrollment token:

```sh
FENCES_ENROLL_TOKEN=fge_… fences-gateway -config gateway.json
```

## 3. The hook

```sh
go install github.com/bhaviksardar/fences-gateway/cmd/fences-hook@latest
```

In Claude Code's settings (`~/.claude/settings.json`, or managed settings so it can't be turned off; see [Limits](#limits)):

```json
{
  "hooks": {
    "PreToolUse": [
      {"matcher": "*", "hooks": [{"type": "command", "command": "fences-hook", "timeout": 600}]}
    ]
  }
}
```

The hook reads these environment variables, from the environment Claude Code starts in:

| Variable | |
|---|---|
| `FENCES_GATEWAY_URL` | The gateway, e.g. `http://127.0.0.1:8080`. |
| `FENCES_TOKEN` | The developer's agent key. |
| `FENCES_HOOK_NAME` | The upstream name rules see as `server`. Default `claude-code`. |
| `FENCES_HOOK_TIMEOUT_S` | How long to wait for a decision, approvals included. Default 590. Keep it below the hook's `timeout` (600 above): Claude Code doesn't block a call whose hook timed out. |

**It fails closed.** If the gateway can't be reached, the token is unknown or the answer can't be read, the call is blocked and Claude is told why. An allowed call isn't approved by the hook: it still goes through Claude Code's own permission prompts.

## 4. MCP servers through the gateway

Add each upstream to the gateway's config (`"upstreams": {"crm": {"url": "https://crm.example.com/mcp", "headers": {"Authorization": "Bearer ${CRM_TOKEN}"}}}`), assign it to the agent in Fences, and point Claude Code at the gateway instead of the server:

```sh
claude mcp add --transport http crm http://127.0.0.1:8080/mcp/crm --header "Authorization: Bearer $FENCES_TOKEN"
```

The server's own credentials stay in the gateway's config: the developer never holds them.

## How rules see a Claude Code call

A hooked call is an `mcp` request (see [the rules spec](../spec/rules/README.md#requests)):

| Field | Value |
|---|---|
| `server` | `claude-code` (`FENCES_HOOK_NAME`) |
| `tool` | Claude Code's tool name: `Bash`, `Write`, `Edit`, `Read`, `WebFetch`… |
| `args` | The tool's input: `args.command` for `Bash`, `args.file_path` for `Write`, and so on |

File paths (`file_path`, `notebook_path`, `path`) are rewritten before the check: **relative if inside the project, absolute if outside it**. The project is `CLAUDE_PROJECT_DIR`, or the session's working directory. So a rule can match any write outside the project with `"args.file_path" matches "/*"`.

## Example policy

[`examples/claude-code.json`](examples/claude-code.json), with `default: allow`:

| Rule | Effect |
|---|---|
| `no-push-main` | Denies `Bash` commands matching `*git push*main*`. |
| `no-rm-rf-root`, `no-rm-rf-home`, `no-rm-rf-parent` | Denies `rm -rf` on an absolute path, the home directory, or above the project. |
| `no-writes-outside` | Denies `Write`, `Edit` and `MultiEdit` outside the project. |
| `prod-credentials` | Commands with `--profile prod` wait for a person's approval, in Slack or on the dashboard. |
| `daily-cap` | At most 1,000 shell commands a day per developer. Past that, they're refused until the window moves on. |

When a rule blocks a call, Claude sees `Fences: deny: Pushing to main is blocked: open a pull request instead` and can change course.

## Limits

- **Command rules are pattern matches, not a sandbox.** They can block too much as well as too little: `*git push*main*` also blocks `git push origin maintenance-fix`. They stop the mistakes you name. `git push` with no arguments while on `main`, `rm -r -f`, or a script that does the same thing won't match. Use them as guardrails alongside Claude Code's own permissions.
- **Paths are compared as text.** A symlink inside the project that points outside it counts as inside.
- **The hook is only as fixed as the settings file.** Claude could edit `~/.claude/settings.json` through a shell command. For developers who mustn't turn it off, deploy the hook in Claude Code's managed settings, which users can't override.
- A file of more than 4 MB written in one call is refused by the gateway, so the hook blocks it.
