# Fences SDK ↔ server protocol

What the `agentfences` SDKs (Python in `sdk/`, TypeScript in `sdk-js/`) send to a Fences server, and what they expect back. The SDKs in this repo and the server in `fences-platform` all implement this file; when they disagree, fix one of them or change this file first.

## Which SDK supports what

| | Python (`sdk/`) | TypeScript (`sdk-js/`) |
|---|---|---|
| Runs, checkpoints, limits (including dashboard-owned and no-budget), run context, notices, quarantine, decisions, spans, errors on run end, `X-Fences-SDK` | 0.3.0 | 0.3.0 |
| Live control: heartbeat, `stop` / `pause` / `resume` / `limits` / `approval` commands, `request_approval` | 0.3.0 | 0.4.0 |
| Guarded tools (`/guard`: checked and reserved before they run), `stopped` in the heartbeat, `breach` on `end` | 0.4.0 | 0.5.0 |
| Command `id`s and acknowledgements (`acks` in the heartbeat), the `quarantine` command, `on_stop` / `on_quarantine` hooks | 0.5.0 (0.5.2 for the fixes) | 0.5.0 (0.6.0 for the 429 fix) |
| Waiting at a limit (`on_breach="hold"`, `held` in the heartbeat) | 0.6.0 | 0.6.0 |
| SDK-only features, no protocol involved: automatic OpenAI/Anthropic call recording | 0.3.0 | 0.8.0 (`instrument(client)`) |
| SDK-only, no protocol involved: LangChain/LangGraph and OpenAI Agents SDK integrations | 0.3.0 | not yet |
| SDK-only, no protocol involved: tool timeouts, halting a stuck run after a stop | 0.4.0 (sync and streaming runs: 0.8.0) | 0.5.0 |
| SDK-only, no protocol involved: budget prechecks before model calls | 0.4.0 | 0.8.0 (with `instrument(client)`) |
| SDK-only, no protocol involved: recording and guarding MCP tool calls (`mcp.wrap`) | 0.7.0 | 0.7.0 |
| A 401 / 403 after run start ends the run (`fences_unreachable`) instead of raising; only numbers in replies are adopted | 0.8.0 | 0.8.0 |

The server (`fences-platform`) implements everything in this file. It decides which SDKs get live control from `X-Fences-SDK` (see "Which SDKs get live control").

---

## Conventions

- **Transport:** HTTPS, `POST` with a JSON body, JSON responses. The SDK waits 3 seconds per request.
- **SDK version**: every request also carries `X-Fences-SDK: python/0.3.0` (or `js/0.3.0` from the TypeScript SDK). Store the latest version per key and show it on the agent's page. When it's older than the server's current SDK, say so in a run-start `notices` entry (code `old_sdk`). Never send commands to a version before 0.3, which has none.
- **Auth:** every request carries `X-API-Key: fc_...`. A key belongs to one account and one named agent; runs a key starts are reported under that agent's name, whatever `agent_name` the SDK sends.
- **Ownership:** a key only sees and touches runs of its own account. Another account's run, or one that doesn't exist, is `404 {"detail": "Run not found"}`.
- **Run IDs** are UUIDs chosen by the SDK.

### How the SDK reads responses

| Response | Core endpoints (start, checkpoint, decisions, end) | Optional endpoints (spans, heartbeat, approvals, guard) |
|---|---|---|
| 2xx with JSON | Use the body | Use the body |
| 401 / 403 | At run start: raises `FencesAuthError` (a `PermissionError` in Python) before any agent code runs, so a bad key is noticed. Later in the run (a revoked key): nothing raises, and the run ends at its next checkpoint with `fences_unreachable` | `{"ok": false}`, never raises (a guarded tool is blocked) |
| 409 | Read as "this run is already fenced" | `{"ok": false}` |
| 423 | Run start only: the agent is quarantined (see below) | – |
| 404 / 405 with `{"detail": "Not Found"}` or `{"detail": "Method Not Allowed"}` | Network error | **The endpoint doesn't exist on this server.** The SDK warns once and stops using it (a guarded tool is then blocked) |
| Anything else, a non-JSON body, a timeout | Network error: limits are enforced locally (or the run stops, with `fail_closed=True`) | Ignored |

So the server must never answer a missing *run* with `{"detail": "Not Found"}` exactly; that text is reserved for a missing *route* (FastAPI's default). Use `"Run not found"`.

---

## Run lifecycle

### `POST /api/runs/start`

Sent when a `@governed` function is called, before any agent code runs.

```json
{
  "run_id": "6f1c...",
  "agent_name": "research_agent",
  "budget_usd": 0.5,
  "max_iterations": 100,
  "max_duration_ms": 300000,
  "max_tokens": 0,
  "context": {"environment": "prod", "release": "v1.4.2", "user_id": "u_123"}
}
```

| Field | Notes |
|---|---|
| `run_id` | 1–128 chars, unique; a repeat is `409` |
| `agent_name` | 1–128 chars; overridden by the key's agent |
| `budget_usd`, `max_iterations`, `max_duration_ms`, `max_tokens` | The limits in code. `max_tokens: 0` means no limit. The server caps each at the key's ceiling. Each may be `null`: not set in code (see "Limits owned by the dashboard") |
| `context` | Optional. String → string, at most 32 keys, values up to 256 chars, already through the SDK's redact hook. Set by `init(environment=, release=)` and `agentfences.context(...)`. The server stores it on the run and lets the dashboard filter by it |

**Response** `200 {"ok": true, "limits": {"budget_usd": 0.5, "max_iterations": 100, "max_duration_ms": 300000, "max_tokens": 0}}`: the run's effective limits after capping.
The SDK adopts `limits`, so its local fallback (when the server is unreachable later) enforces the same numbers as the server. `budget_usd: null` means the run is unlimited in spend.

The reply may also carry `"notices": [{"code": "no_budget", "message": "research_agent has no budget, so its spend isn't capped.", "url": "https://.../agents/research_agent"}]`. The SDK logs each one as a warning, once per agent and code per process, with the URL appended. Use it for anything the developer should see in their logs (an outdated SDK, too).

**Quarantine** If the agent is quarantined, answer `423 {"detail": "<why, shown to the developer>"}` and create no run. The SDK raises `AgentQuarantined` from the governed call; no agent code runs.

### `POST /api/runs/{run_id}/checkpoint`

Sent after each step (usually each model call). Spend and tokens are **this step's** amounts, never totals.

```json
{"cost_delta_usd": 0.0123, "tokens_used": 1450, "iterations": 7, "duration_ms": 8200}
```

The server adds `cost_delta_usd` and `tokens_used` to the run, counts the step itself and measures time itself (`iterations` and `duration_ms` are sent but ignored), then checks limits.

Optional `step_id` (a string of 1 to 64 characters, unique within the run; the SDKs send 32 hex characters): a repeat of the same `(run_id, step_id)` isn't counted again. The server answers it with the run's current totals, or its breach, with commands attached as usual, and echoes `"step_id"` in every reply to a request that had one. The echo tells the SDK this server deduplicates steps, so a checkpoint that hit a connection error can safely be sent again. Without `step_id` every request counts.

**Limit rules** (the SDK applies the same ones locally): a limit trips once **exceeded**, not when reached; spend is compared after `round(spent, 9)`; when several trip at once the order is spend, steps, time, tokens; `max_tokens: 0` means no token limit.

**Response, within limits**
```json
{"ok": true, "spent_usd": 0.31, "iterations": 7, "tokens_used": 9800}
```

**Response, breached** (also on every later checkpoint of a run that's still breached)
```json
{"ok": false, "breach": "budget_exceeded",
 "spent_usd": 0.52, "budget_usd": 0.5, "iterations": 14, "max_iterations": 20,
 "tokens_used": 6300, "max_tokens": 0, "duration_ms": 18200, "max_duration_ms": 300000}
```
The SDK adopts every total and limit in the response; the server is authoritative.

`409` for a run that already ended. Either response may include `"commands": [...]` (see Commands); the SDK applies them before deciding.

### `POST /api/runs/{run_id}/decisions`

```json
{"iteration": 3, "reasoning": "Search returned nothing; retrying with a narrower query", "action": "retry_search"}
```
`reasoning` 1–2000 chars, `action` optional, up to 200. Sent in the background, in order. Response body is ignored.

### `POST /api/runs/{run_id}/spans`

Tool calls, model calls and approvals, as spans shaped like OpenTelemetry's and named by its [GenAI semantic conventions](https://github.com/open-telemetry/semantic-conventions-genai) (`gen_ai.*`), plus Fences' own `fences.*` attributes. Sent in the background, in order with decisions, batched up to 50 per request. Store them in the same model as spans ingested from OpenTelemetry later, so both show up the same way.

```json
{"spans": [
  {"name": "chat gpt-4o-2024-08-06", "start_ts": 1791161640.47, "end_ts": 1791161642.30, "status": "ok",
   "attributes": {"gen_ai.operation.name": "chat", "gen_ai.provider.name": "openai",
                  "gen_ai.request.model": "gpt-4o", "gen_ai.response.model": "gpt-4o-2024-08-06",
                  "gen_ai.usage.input_tokens": 1000, "gen_ai.usage.cache_read.input_tokens": 400,
                  "gen_ai.usage.cache_write.input_tokens": 0, "gen_ai.usage.output_tokens": 200,
                  "fences.cost_usd": 0.004, "fences.iteration": 3}},
  {"name": "execute_tool web_search", "start_ts": 1791161642.31, "end_ts": 1791161642.72, "status": "error",
   "attributes": {"gen_ai.operation.name": "execute_tool", "gen_ai.tool.name": "web_search",
                  "gen_ai.tool.call.arguments": "query='cats', limit=5", "error.type": "PermissionError",
                  "fences.error.message": "PermissionError: 403 from search API", "fences.iteration": 3}}
]}
```

Every span has `name`, `start_ts` and `end_ts` (Unix seconds), `status` (`ok` or `error`) and `attributes`. Attributes with no value are left out.

| Span `name` | `gen_ai.operation.name` | Attributes |
|---|---|---|
| `chat {model}` | `chat` | `gen_ai.provider.name` (`openai`, `anthropic`, or what LangChain reports), `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.request.stream` (streamed calls have no usage); usage: `gen_ai.usage.input_tokens` (**includes** cache reads and writes, per OpenTelemetry), `gen_ai.usage.cache_read.input_tokens`, `gen_ai.usage.cache_write.input_tokens`, `gen_ai.usage.output_tokens`, `fences.cost_usd` |
| `execute_tool {name}` | `execute_tool` | `gen_ai.tool.name`, `gen_ai.tool.call.arguments` (a summary, ≤300 chars), `gen_ai.tool.call.id` when known |
| `fences.approval` | – | One span from request to answer: `fences.approval.id`, `.reason`, `.amount_usd`, `.granted`, `.by`, `.note`, `.granted_usd` |

On every span: `fences.iteration` (the run's step count when it happened) and, if recorded through a framework, `fences.integration` (`langchain`, `openai-agents`). On failures: `error.type` (the exception class), `fences.error.message` (≤1000 chars) and, for HTTP errors, `http.response.status_code` (e.g. 429).

Unknown span names and attributes must be ignored, so new ones can be added without a server release. `fences.cost_usd` on model calls is a record only, not spend (spend arrives through checkpoints), so don't add it to the run's total. Response body is ignored.

**Mapping a run to OpenTelemetry:** a Fences run is the `invoke_agent {agent_name}` span these sit under (`gen_ai.agent.name` = the run's agent; `gen_ai.conversation.id` = its `session_id` context value, if set).

### `POST /api/runs/{run_id}/end`

Sent when the governed function returns or raises.

```json
{"status": "error", "error": "ValueError: page 7 returned 403",
 "exception": {"type": "ValueError", "message": "page 7 returned 403",
               "stack": ["/app/agent.py:41 in fetch_page", "/app/agent.py:88 in research_agent"]}}
```

| Field | Notes |
|---|---|
| `status` | `success`, `error` or `breached`. A run the server already marked breached stays breached |
| `error` | `"Type: message"`, or just the type. The server truncates to 2000 chars |
| `exception` | Only on `error`. `type`, `message` (≤1000 chars), `stack` (last 15 frames, innermost last). Store it with the run for the incident page |
| `breach` | Only on `breached` (Python 0.4+): which breach ended the run, as in "Breach types". It matters for breaches the server didn't decide: `budget_exceeded` from a model call the SDK refused before sending it (its spend never crossed the budget), `paused`, `fences_unreachable`. Store and show it like a breach the server decided |

Both `error` and `exception` have been through the SDK's redact hook, and may be missing even on an error if the hook dropped them.

---

## Live control

### `POST /api/heartbeat`

A background thread in each process with live runs sends this every 15 seconds (configurable), and every 2 seconds while one of its runs is paused or waiting for an approval.

```json
{"runs": [{"run_id": "6f1c...", "iterations": 14, "spent_usd": 0.31, "tokens_used": 9800, "paused": false, "stopped": false}],
 "acks": [{"run_id": "6f1c...", "id": "stop:6f1c...:1791937487", "type": "stop", "received_at": 1791937490.2}]}
```

**Response:** `{"commands": [...]}` with any commands for those runs.

`acks` (Python 0.5+) acknowledges commands; see Acknowledgements. `held` (0.6+, only while it applies) says a run is paused because it's waiting at a limit, not because someone paused it; see "Waiting at a limit". A heartbeat can carry only `acks`, with `runs` empty, when a hook finishes after its run has ended.

`stopped: true` means the SDK has received a `stop` for that run (Python 0.4+; older SDKs leave it out). Use it to show how far a stop got:
- **Stop sent, not received:** no heartbeat with `stopped: true` yet. The process may be dead or cut off.
- **Received, still running:** the agent hasn't reached a checkpoint. An async Python run is cancelled `stop_grace_s` (default 30) seconds after the stop arrives, so it ends soon; a synchronous one can't be interrupted.
- If it's still running a while after that (say, 2 minutes), show it as **unresponsive** and offer to quarantine the agent, so no new run starts and every guarded tool is blocked.

The server should record each run's last heartbeat: a running run with no heartbeat and no checkpoint for a while is **stuck** (or its process died), which is what the stuck detector looks for. The run's own numbers come from checkpoints; treat the ones here as a hint.

### Commands

Delivered in heartbeat responses and in checkpoint responses. Every command is safe to apply twice, so deliver at least once (for example, until the next heartbeat shows the effect). Unknown types are ignored.

The SDK applies `limits` before the other commands in a reply, so a `resume` that releases a call held for budget re-checks it against the raised limit, whatever order the reply lists them in.

Every command except `limits` carries an `id`: a string the server chooses, identical each time it re-sends the same command and new for a new one. A second pause after a resume gets a new id. A quarantine's `quarantine` command has the same id for every run it's sent to. The SDK acknowledges commands by id (see Acknowledgements); a command without one is applied but not acknowledged, which is how servers before this change work.

How the server does it (`fences-platform`): commands aren't queued, they're derived from the run's state and re-sent on every heartbeat and checkpoint reply:
- `stop` while the run is `stopped_by_user`
- `pause` until a heartbeat reports `paused: true`
- `resume` while the SDK reports paused but no pause is requested
- `limits` always
- `approval` answers for 10 minutes after they're given

| Command | Effect in the SDK | What the server does |
|---|---|---|
| `{"run_id", "type": "stop", "id", "by"?, "reason"?}` | The next `checkpoint()` returns breach `stopped_by_user`; it sticks even if a later response says ok. Runs the `on_stop` hook, once per run | Marks the run breached (`POST /api/runs/{id}/stop`) and sends this command, so a run between checkpoints learns within seconds |
| `{"run_id", "type": "pause"}` | The next `checkpoint()` waits until resumed; heartbeats report `"paused": true`. After `pause_timeout_s` (default 1 h) with no resume, `checkpoint()` returns breach `paused` | Keep sending `pause` until the heartbeat reports `paused: true` |
| `{"run_id", "type": "resume"}` | The waiting checkpoint returns and the agent carries on | Send once after the operator resumes |
| `{"run_id", "type": "limits", "budget_usd"?, "max_iterations"?, "max_duration_ms"?, "max_tokens"?}` | Updates the run's limits locally, so the offline fallback matches | Send whenever limits change from the dashboard |
| `{"run_id", "type": "approval", "approval_id", "granted", "by", "note", "amount_usd"}` | Answers a waiting `request_approval()`; if granted with an amount, raises the run's budget by it | See Approvals |
| `{"run_id", "type": "quarantine", "id", "by"?, "reason"?}` | The run pauses at its next checkpoint, as with `pause`, and every guarded tool is blocked in the SDK too. Runs the `on_quarantine` hook, once per process per `id` | Sent to every running run of a quarantined agent, instead of `pause`, to SDKs that support it (python ≥0.5.0, js ≥0.5.0); older ones still get `pause`. Releasing the quarantine sends `resume` |

### Acknowledgements

The SDK acknowledges every command it applies that has an `id`, in the `acks` of its next heartbeat:

```json
{"run_id": "6f1c...", "id": "quarantine:research_agent:1791937400", "type": "quarantine", "received_at": 1791937490.2}
{"run_id": "6f1c...", "id": "quarantine:research_agent:1791937400", "type": "quarantine", "hook": "failed", "detail": "RuntimeError: vault unreachable", "finished_at": 1791937491.0}
```

- **Receipt:** one ack per run and command `id`, as soon as the command is applied. A new ack sends the heartbeat at once, so acks arrive within a round trip, not after a 15-second wait. A command re-sent with the same `id` isn't acknowledged again.
- **Hook result:** for `stop` and `quarantine` with a hook configured, a second ack when the hook finishes: `hook` is `ok`, `failed` (with `detail`, the error, ≤300 chars) or `timeout` (it ran past 30 seconds and is still going). The hook result is reported under the `run_id` that triggered it, even if the run has ended since.
- **At least once:** an ack whose heartbeat fails (a network error, `429` or a `5xx`) is sent again with the next one. Any other `4xx` drops it, since resending won't help. Keep the first `received_at`, and the latest `hook` result.
- **Rate limits:** answer a heartbeat over its budget with `429`, never by dropping it silently. The SDK keeps that heartbeat's acks for the next one, and its checkpoints, guards and other calls aren't affected.

**On the server**, record when each command `id` was first sent and when it was acknowledged:
- **Unacknowledged:** no ack within 30 seconds of sending (heartbeats come every 15 seconds, and every 2 while a run is busy) means the process didn't get it: it's dead, cut off or stuck in synchronous code. Show "agent hasn't acknowledged", fire the agent's containment webhooks, page again and raise the incident's severity.
- **Hook results** go on the incident timeline: "on_quarantine ran in the agent: ok", or "failed: <detail>".
- Only SDKs that send acks can be held to this (python ≥0.5.0 and js ≥0.5.0, from `X-Fences-SDK`). For older ones, keep using `stopped` and `paused` in the heartbeat.

### Waiting at a limit

An agent can opt in to wait at a limit instead of ending: `init(on_breach="hold")` or `governed(on_breach="hold")` in Python, `onBreach: "hold"` in TypeScript. Whoever is on call raises the limit, and the agent carries on where it was.

**In the SDK.** With hold on:
- A breach the server decides (`budget_exceeded`, `iteration_limit`, `time_limit`, `token_limit`, `key_daily_budget`, `key_monthly_budget`, `limit_reached`) makes `checkpoint()` wait, like a pause. Heartbeats report the run with `"paused": true, "held": "breach"`.
- A call refused for budget before it's made also waits, with `"held": "budget"`: a model call refused by the budget precheck (Python), or a guarded tool denied with a budget breach.
- **Released** by a `resume` command. A held breach then returns ok from `checkpoint()`, and a held call is checked again with the new limits (and waits again if it still doesn't fit).
- **Ends** on a `stop` (the breach is `stopped_by_user`), or after `pause_timeout_s` (default 1 h) with the original breach, e.g. `budget_exceeded`, not `paused`, so the agent wraps up as it would have.
- **Never holds** a breach the SDK decided because the server couldn't be reached, or anything in local mode: there's no one to raise the limit.

**On the server:**
- Store `held` on the run and show it: "Held at a limit: raise it to carry on" (`breach`), or "Held: the next call needs more budget" (`budget`), with the time it waits until.
- **Never send the automatic `resume`** (the one for "the SDK reports paused but no pause is requested") to a run reporting `held`. Only an operator releases it: "Raise limits & resume" raises the limits, sets a breached run back to `running`, and sends `resume`.
- **Keep sending `stop`, `quarantine` and `limits` to a held run,** even though a `breach` hold leaves the run `breached`. Otherwise a stop never reaches it and it waits out its timeout.
- A held run is alive and waiting on a person. Treat its heartbeats as normal, and don't call it stuck. Open the incident for its breach as usual, so someone is paged to raise the limit.

### Containment hooks in the SDK

`init(on_stop=..., on_quarantine=...)` runs a function in the agent's process when a `stop` or `quarantine` command arrives, to contain what the server can't reach, for example revoking a credential the agent holds:

```python
def revoke(event):  # {"type": "quarantine", "run_id", "agent_name", "id", "by", "reason"}
    vault.revoke(f"agent/{event['agent_name']}")

agentfences.init(api_key="fc_...", on_quarantine=revoke)
```

Each hook runs in its own background thread, never on the agent's thread or the heartbeat's. `on_stop` runs once per run, and `on_quarantine` once per process per quarantine (`id`), however many runs receive it. An exception is caught and reported as `failed`; a hook still running after 30 seconds is reported as `timeout` and left to finish. At exit, and in `flush()`, the SDK waits for running hooks (up to 30 seconds) and sends the acknowledgements still waiting, in a heartbeat with `runs: []`, so a process that ends right after a stop still contains it and reports it.

### Approvals: `POST /api/runs/{run_id}/approvals`

Sent by `request_approval(reason, amount_usd=None, timeout_s=600)`; the agent then waits for the answer.

```json
{"approval_id": "0c3e...", "reason": "Refund $240 to order 1182?", "amount_usd": 240, "timeout_s": 600}
```

- `approval_id` is a UUID chosen by the SDK; treat a repeat as the same request.
- **Response:** `{"ok": true}` once the request is stored and someone is notified.
- Show it to the on-call person with approve/deny. Their answer goes back as an `approval` command, with `by` (who answered, e.g. their email) and an optional `note`.
- **If an amount is granted, add it to the run's `budget_usd` on the server too.** The SDK raises its local budget by the same amount; if only one side does, the next checkpoint breaches.
- The SDK stops waiting after `timeout_s` (sent with the request; 600 if absent). Expire the request on the server at the same moment, and show a late answer as expired, not applied.

### Guarded tools: `POST /api/runs/{run_id}/guard`

A tool marked `@agentfences.tool(guard=True)` (or with `cost_usd=` or `approve=True`) asks the server before it runs, and again after, to settle. The tool runs only on `"allow": true`. This is the enforcement point for actions with consequences (refunds, payments, writes to production): the model can't talk its way past it, because the check happens in the tool's own code before it does anything.

**Check**, before the tool runs:
```json
{"guard_id": "9b2e...", "phase": "check", "tool": "issue_refund", "arguments": "order_id='1182', amount=240",
 "amount_usd": 240, "approval_id": "0c3e..."}
```
- `guard_id`: a UUID chosen by the SDK. A repeat of an allowed check returns the stored answer and is never reserved twice. A repeat of a denied one is checked again, as nothing was reserved and the cause may have changed (a resume, an approval).
- `arguments`: a summary (≤300 chars), already through the redact hook.
- `amount_usd`: what the call may cost the run (`0` if unknown). On allow, **reserve** it: it counts against the run's budget, and the key's daily and monthly caps, until it's settled. Check and reserve atomically, so two calls racing for the last of a budget can't both pass.
- `approval_id`: present when the SDK got an approval for this call (`approve=True`). Allow only if that approval belongs to this run, was granted and hasn't been used for another guard.

**Response:** `{"allow": true}`, or `{"allow": false, "reason": "<why, shown to the developer and the model>", "breach": "budget_exceeded"}`. Either may carry `commands`. Deny when:
- the run is stopped, breached or paused, or the agent is quarantined
- `spent + reserved + amount_usd` would exceed the run's budget or a key cap (`breach: budget_exceeded`, or the key cap's breach type)
- the dashboard requires approval for this tool and there's no valid `approval_id`. Answer `{"allow": false, "needs_approval": true, "reason": "..."}`: the SDK asks for approval (see Approvals), then checks again with its `approval_id`.

**Settle**, after the tool finishes:
```json
{"guard_id": "9b2e...", "phase": "settle", "outcome": "ok", "cost_usd": 240}
```
Release the reservation and add `cost_usd` to the run's spend (it's not a step, so `iterations` stays the same).
- `outcome`: `ok` (it ran), `error` (it raised, `cost_usd: 0`), or `unknown` (it timed out or was cancelled, so it may have happened; the SDK charges the full amount to be safe).
- A reservation never settled (the SDK lost its connection, or the process died) is charged in full when the run ends.

The response body is ignored.

**In the SDK:** a denied call raises `ToolBlocked` (with `reason` and `breach_type`) instead of running, and is recorded as a failed `execute_tool` span. With no answer (unreachable, timeout, any error) the tool is blocked: guards fail closed even without `fail_closed=True`. A server without this endpoint blocks guarded tools, with one warning. In local mode the SDK applies the same rules itself (stop, breach, budget including reservations); `approve=True` is always denied there, as there's no one to ask.

### Quarantine

A quarantined agent: new runs get `423` at start (above); running runs get `quarantine` (python ≥0.5.0) or `pause` (older SDKs), and every guard check is denied. Releasing the quarantine resumes all of that agent's paused runs.

### Which SDKs get live control

`X-Fences-SDK` is read as `language/version`. Live control (heartbeat commands, pause, approvals) is only for SDKs that implement it: **`python` 0.3 and later, `js` 0.4 and later**. Older ones (including `js/0.3.0`) get no commands, and pausing their runs is refused with `409` and a reason. When another SDK gains heartbeat, pause and approvals, add its language and version here and to the server's list. The `old_sdk` notice compares versions per language.

---

## Limits owned by the dashboard

Under an on-call model, limits belong to whoever runs the agent, so every limit in `@governed(...)` is optional; a bare `@governed()` works.

**SDK**
- A limit left out in code is sent as `null` at run start.
- Cloud mode: the SDK uses whatever the server returns in `limits`.
- Local mode (no server): steps default to 100, time to 5 minutes, tokens to no limit, and **budget to none**.
- With no budget the run still starts, unlimited in spend, and the SDK logs a warning once per agent: `research_agent has no budget, so its spend isn't capped.`, followed by how to set one (`@governed(budget_usd=...)` locally, or the server's `url`).

**Server**
1. Fill each `null` limit from the agent's limits in the dashboard (its key's settings); a limit in code is still capped by them as today.
2. Anything still unset: 100 steps, 300000 ms, `max_tokens: 0`, and **no budget** (`budget_usd: null`).
3. Never refuse a run for having no budget. Instead:
   - Return a `no_budget` notice in the run-start reply, with the agent page's `url`.
   - Show a **No budget** badge on the agent's row and on its runs, so it stands out.
   - On the agent's first run with no budget, prompt the owner in the dashboard: **Set a budget** (opens the agent's limits) or **Keep unlimited**.
   - "Keep unlimited" is remembered per agent and stops the prompt, but the badge stays.
   - Notify the owner when an agent first runs without a budget: in the dashboard now, and over Slack/PagerDuty once alerting exists.
4. A key-level daily or monthly cap still applies to an agent with no per-run budget.

## Breach types

What `checkpoint()` can return as `breach_type`, each with its own `message` and `system_prompt` in the SDK. The server returns the ones it decides in the `breach` field.

| `breach` | Decided by | Meaning |
|---|---|---|
| `budget_exceeded`, `iteration_limit`, `time_limit`, `token_limit` | server (or SDK locally) | A run limit was exceeded |
| `key_daily_budget`, `key_monthly_budget` | server | The agent's daily or monthly cap across all its runs |
| `stopped_by_user` | server, or a `stop` command | Someone pressed Stop |
| `budget_exceeded` (prechecked) | SDK | A model call refused before it was sent because its worst case could have passed the budget; reported in `end`'s `breach`, as spend never crossed the budget on the server |
| `paused` | SDK | Paused and not resumed within `pause_timeout_s` |
| `fences_unreachable` | SDK | Server unreachable and `init(fail_closed=True)`, or the API key was rejected during the run |
| `limit_reached` | either | Fallback when the server gave no reason |

New breach types need a message and system prompt in the SDK (`_make_breach_result`); until then the agent sees a generic "Governance limit reached."

## Run statuses

`running` → `success`, `error` or `breached`. `breached` sticks: a later `end` with another status doesn't change it. Raising limits on a breached run from the dashboard can set it back to `running`, and its next checkpoint passes.

## Delivery guarantees

- Start, checkpoint and end are sent inline (in a worker thread for async agents) and wait for the answer.
- Guard checks and settles are sent inline too. Nothing is retried, so a check is never applied twice; `guard_id` makes a repeat harmless anyway.
- Decisions and spans go from one background thread, in order. **At most once:** nothing is retried, so a network error loses them. `flush()` (also run at exit) waits for the queue to drain.
- Everything except numbers and run IDs passes through the SDK's optional `redact` hook first, which can change or drop it. Don't assume a field is present.

---

## Changes to this file

- **Next SDK release (unreleased):** checkpoints carry an optional `step_id` (32 hex characters, unique within the run); a server that supports it counts a repeated `(run_id, step_id)` once, returns the run's current totals (or its breach), and echoes `step_id` in the reply. The SDK retries a checkpoint once after a connection error only after seeing that echo. Guard calls are retried once after a connection error under their `guard_id`. SDK-only, alongside: a circuit breaker on calls to Fences, and heartbeat backoff with jitter.
- **Python 0.8.0, TypeScript 0.8.0 (released 2026-10-17):** how the SDKs read replies, with no change to the wire format. A 401 / 403 after run start (a key revoked mid-run) no longer raises in the agent: the run ends at its next checkpoint with `fences_unreachable`. Only numbers are adopted from `limits`, checkpoint replies and `limits` commands; a null `budget_usd` still means unlimited. A JSON reply that isn't an object counts as not JSON. An approval request answered with an error is denied at once. SDK-only, alongside: TypeScript records OpenAI and Anthropic calls and prechecks them against the budget (`instrument(client)`); Python halts a sync or streaming run stuck after a stop.
- **Python 0.6.0, TypeScript 0.6.0 (released 2026-10-16):** waiting at a limit: `on_breach="hold"`, `held` (`breach` or `budget`) in heartbeat runs, released only by an operator's `resume`.
- **Python 0.5.2, TypeScript 0.6.0:** a heartbeat answered `429` keeps its acks for the next one (Python 0.5.1 dropped them, so the server could flag commands the agent had received as unacknowledged).
- **Python 0.5.1:** fixes 0.5.0 sending heartbeats back to back while its agent was quarantined (the server re-sends `quarantine` on every reply). No protocol change.
- **TypeScript 0.5.0 (released 2026-10-15, before the 429 fix: a heartbeat answered `429` loses its acks, as in Python 0.5.0/0.5.1; fixed in TypeScript 0.6.0):** guarded tools, `stopped` in the heartbeat, `breach` on `end`, command acknowledgements, the `quarantine` command and `onStop` / `onQuarantine` hooks: the protocol side of Python 0.4.0 and 0.5.0. Not budget prechecks, which need model-call instrumentation.

- **Python 0.5.0 (released 2026-10-13):** command `id`s and `acks` in the heartbeat, the `quarantine` command, `on_stop` / `on_quarantine` hooks with their results acknowledged. Server side: `fences-platform` 0a30ecf.
- **Python 0.4.0 (released 2026-10-13):** `breach` on `end` for breaches the SDK decided, guarded tools (`/guard`: check and reserve before, settle after), `stopped` in heartbeat runs, unacknowledged and unresponsive stops. Budget prechecks before model calls, tool timeouts and cancelling a stuck async run need no server change. See ENFORCEMENT.md for what each part guarantees. Server side: `fences-platform` 8ad1be9.
- **TypeScript 0.4.0 (released 2026-10-11):** live control in the TypeScript SDK.
- **0.3 (released 2026-10-09, Python 0.3.0 and TypeScript 0.3.0):** tool and model calls as OpenTelemetry-shaped spans (`/spans`), the `X-Fences-SDK` header, optional limits and dashboard-owned limits (null at run start, no-budget runs allowed with notices), run-start `context` and the effective-limits adoption, quarantine (423), `exception` on end, heartbeat, commands, approvals, the `paused` breach. Server step counting and timing documented as authoritative. `raise_on_breach` is gone from the SDK, so breaches are only ever results. Server side: `fences-platform` c09c70f..212487a.
