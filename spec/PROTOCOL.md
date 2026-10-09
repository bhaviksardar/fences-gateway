# Fences SDK ↔ server protocol

What the `agentfences` SDKs (Python in `sdk/`, TypeScript in `sdk-js/`) send to a Fences server, and what they expect back. The SDKs in this repo and the server in `fences-platform` all implement this file; when they disagree, fix one of them or change this file first.

**Status markers**

| | Meaning |
|---|---|
| ✅ | Live: today's SDK and server both do this |
| 🆕 | SDK 0.3 (unreleased) sends or expects this; the server has to build it |

SDK 0.3 and the server changes ship together, after the server passes `tests/test_sdk_e2e.py` against the unreleased SDK.

### Server work for SDK 0.3

All done in `fences-platform` (c09c70f..7629a96); its `tests/test_sdk_e2e.py` passes against SDK 0.3.

- [x] Run start: store `context`; answer `423` for a quarantined agent
- [x] Run end: store `exception`
- [x] `POST /api/runs/{run_id}/spans`: store tool calls, model calls and approvals (OpenTelemetry-shaped); show them on the run
- [x] `POST /api/heartbeat`: record last heartbeat per run; return pending commands
- [x] Send `stop`, `pause`, `resume`, `limits` commands from the dashboard actions (in heartbeat and checkpoint responses)
- [x] `POST /api/runs/{run_id}/approvals`: store, notify, deliver the answer as an `approval` command, raise `budget_usd` by a granted amount
- [x] Quarantine an agent from the dashboard
- [x] Run start: accept null limits and fill them from the agent's dashboard limits, then defaults; a run with no budget anywhere starts unlimited (see "Limits owned by the dashboard")
- [x] Read `X-Fences-SDK`: store the version per key, show it, add an `old_sdk` notice for outdated SDKs, send no commands to versions before 0.3
- [x] Agents with no budget: "No budget" badge, the Set a budget / Keep unlimited prompt on first unlimited run, an owner notification, and a `no_budget` notice in the run-start reply
- [x] `tests/test_sdk_e2e.py`: stop using `raise_on_breach` and `BudgetExceeded` (removed in SDK 0.3); it doesn't import against the new SDK
- [x] `dashboard/docs.html`: drop the exception classes and `raise_on_breach`

---

## Conventions ✅

- **Transport:** HTTPS, `POST` with a JSON body, JSON responses. The SDK waits 3 seconds per request.
- **SDK version** 🆕: every request also carries `X-Fences-SDK: python/0.3.0` (or `js/0.3.0` from the TypeScript SDK). Store the latest version per key and show it on the agent's page. When it's older than the server's current SDK, say so in a run-start `notices` entry (code `old_sdk`). Never send commands to a version before 0.3, which has none.
- **Auth:** every request carries `X-API-Key: fc_...`. A key belongs to one account and one named agent; runs a key starts are reported under that agent's name, whatever `agent_name` the SDK sends.
- **Ownership:** a key only sees and touches runs of its own account. Another account's run, or one that doesn't exist, is `404 {"detail": "Run not found"}`.
- **Run IDs** are UUIDs chosen by the SDK.

### How the SDK reads responses

| Response | Core endpoints (start, checkpoint, decisions, end) | Optional endpoints (spans, heartbeat, approvals) |
|---|---|---|
| 2xx with JSON | Use the body | Use the body |
| 401 / 403 | Raises `PermissionError` in the agent: a bad key must be noticed | `{"ok": false}`, never raises |
| 409 | Read as "this run is already fenced" | `{"ok": false}` |
| 423 | Run start only: the agent is quarantined (see below) | – |
| 404 / 405 with `{"detail": "Not Found"}` or `{"detail": "Method Not Allowed"}` | Network error | **The endpoint doesn't exist on this server.** The SDK warns once and stops using it |
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

| Field | | Notes |
|---|---|---|
| `run_id` | ✅ | 1–128 chars, unique; a repeat is `409` |
| `agent_name` | ✅ | 1–128 chars; overridden by the key's agent |
| `budget_usd`, `max_iterations`, `max_duration_ms`, `max_tokens` | ✅ / 🆕 | The limits in code. `max_tokens: 0` means no limit. The server caps each at the key's ceiling. 🆕 Each may be `null`: not set in code (see "Limits owned by the dashboard") |
| `context` | 🆕 | Optional. String → string, at most 32 keys, values up to 256 chars, already through the SDK's redact hook. Set by `init(environment=, release=)` and `agentfences.context(...)`. The server stores it on the run and lets the dashboard filter by it |

**Response** ✅ `200 {"ok": true, "limits": {"budget_usd": 0.5, "max_iterations": 100, "max_duration_ms": 300000, "max_tokens": 0}}`: the run's effective limits after capping.
🆕 The SDK adopts `limits`, so its local fallback (when the server is unreachable later) enforces the same numbers as the server. `budget_usd: null` means the run is unlimited in spend.

🆕 The reply may also carry `"notices": [{"code": "no_budget", "message": "research_agent has no budget, so its spend isn't capped.", "url": "https://.../agents/research_agent"}]`. The SDK logs each one as a warning, once per agent and code per process, with the URL appended. Use it for anything the developer should see in their logs (an outdated SDK, too).

**Quarantine** 🆕 If the agent is quarantined, answer `423 {"detail": "<why, shown to the developer>"}` and create no run. The SDK raises `AgentQuarantined` from the governed call; no agent code runs.

### `POST /api/runs/{run_id}/checkpoint`

Sent after each step (usually each model call). Spend and tokens are **this step's** amounts, never totals.

```json
{"cost_delta_usd": 0.0123, "tokens_used": 1450, "iterations": 7, "duration_ms": 8200}
```

✅ The server adds `cost_delta_usd` and `tokens_used` to the run, counts the step itself and measures time itself (`iterations` and `duration_ms` are sent but ignored), then checks limits.

**Limit rules** ✅ (the SDK applies the same ones locally): a limit trips once **exceeded**, not when reached; spend is compared after `round(spent, 9)`; when several trip at once the order is spend, steps, time, tokens; `max_tokens: 0` means no token limit.

**Response, within limits** ✅
```json
{"ok": true, "spent_usd": 0.31, "iterations": 7, "tokens_used": 9800}
```

**Response, breached** ✅ (also on every later checkpoint of a run that's still breached)
```json
{"ok": false, "breach": "budget_exceeded",
 "spent_usd": 0.52, "budget_usd": 0.5, "iterations": 14, "max_iterations": 20,
 "tokens_used": 6300, "max_tokens": 0, "duration_ms": 18200, "max_duration_ms": 300000}
```
The SDK adopts every total and limit in the response; the server is authoritative.

`409` ✅ for a run that already ended. 🆕 Either response may include `"commands": [...]` (see Commands); the SDK applies them before deciding.

### `POST /api/runs/{run_id}/decisions` ✅

```json
{"iteration": 3, "reasoning": "Search returned nothing; retrying with a narrower query", "action": "retry_search"}
```
`reasoning` 1–2000 chars, `action` optional, up to 200. Sent in the background, in order. Response body is ignored.

### `POST /api/runs/{run_id}/spans` 🆕

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

| Field | | Notes |
|---|---|---|
| `status` | ✅ | `success`, `error` or `breached`. A run the server already marked breached stays breached |
| `error` | ✅ | `"Type: message"`, or just the type. The server truncates to 2000 chars |
| `exception` | 🆕 | Only on `error`. `type`, `message` (≤1000 chars), `stack` (last 15 frames, innermost last). Store it with the run for the incident page |

Both `error` and `exception` have been through the SDK's redact hook, and may be missing even on an error if the hook dropped them.

---

## Live control 🆕

### `POST /api/heartbeat`

A background thread in each process with live runs sends this every 15 seconds (configurable), and every 2 seconds while one of its runs is paused or waiting for an approval.

```json
{"runs": [{"run_id": "6f1c...", "iterations": 14, "spent_usd": 0.31, "tokens_used": 9800, "paused": false}]}
```

**Response:** `{"commands": [...]}` with any commands for those runs.

The server should record each run's last heartbeat: a running run with no heartbeat and no checkpoint for a while is **stuck** (or its process died), which is what the stuck detector looks for. The run's own numbers come from checkpoints; treat the ones here as a hint.

### Commands

Delivered in heartbeat responses and in checkpoint responses. Every command is safe to apply twice, so deliver at least once (for example, until the next heartbeat shows the effect). Unknown types are ignored.

How the server does it (`fences-platform`): commands aren't queued, they're derived from the run's state and re-sent on every heartbeat and checkpoint reply:
- `stop` while the run is `stopped_by_user`
- `pause` until a heartbeat reports `paused: true`
- `resume` while the SDK reports paused but no pause is requested
- `limits` always
- `approval` answers for 10 minutes after they're given

| Command | Effect in the SDK | What the server does |
|---|---|---|
| `{"run_id", "type": "stop"}` | The next `checkpoint()` returns breach `stopped_by_user`; it sticks even if a later response says ok | ✅ already marks the run breached via `POST /api/runs/{id}/stop`; 🆕 also send this command so a run between checkpoints learns sooner |
| `{"run_id", "type": "pause"}` | The next `checkpoint()` waits until resumed; heartbeats report `"paused": true`. After `pause_timeout_s` (default 1 h) with no resume, `checkpoint()` returns breach `paused` | Keep sending `pause` until the heartbeat reports `paused: true` |
| `{"run_id", "type": "resume"}` | The waiting checkpoint returns and the agent carries on | Send once after the operator resumes |
| `{"run_id", "type": "limits", "budget_usd"?, "max_iterations"?, "max_duration_ms"?, "max_tokens"?}` | Updates the run's limits locally, so the offline fallback matches | Send whenever limits change from the dashboard |
| `{"run_id", "type": "approval", "approval_id", "granted", "by", "note", "amount_usd"}` | Answers a waiting `request_approval()`; if granted with an amount, raises the run's budget by it | See Approvals |

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

### Quarantine

A quarantined agent: new runs get `423` at start (above); running runs get `pause`. Releasing the quarantine resumes all of that agent's paused runs.

### Older SDKs

A run whose key last reported an SDK before 0.3 (`X-Fences-SDK`) can't be paused: it has no heartbeat to wait on. The server refuses the pause with `409` and says why.

---

## Limits owned by the dashboard 🆕

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
| `budget_exceeded`, `iteration_limit`, `time_limit`, `token_limit` | ✅ server (or SDK locally) | A run limit was exceeded |
| `key_daily_budget`, `key_monthly_budget` | ✅ server | The agent's daily or monthly cap across all its runs |
| `stopped_by_user` | ✅ server / 🆕 `stop` command | Someone pressed Stop |
| `paused` | 🆕 SDK | Paused and not resumed within `pause_timeout_s` |
| `fences_unreachable` | ✅ SDK | Server unreachable and `init(fail_closed=True)` |
| `limit_reached` | ✅ either | Fallback when the server gave no reason |

New breach types need a message and system prompt in the SDK (`_make_breach_result`); until then the agent sees a generic "Governance limit reached."

## Run statuses ✅

`running` → `success`, `error` or `breached`. `breached` sticks: a later `end` with another status doesn't change it. Raising limits on a breached run from the dashboard can set it back to `running`, and its next checkpoint passes.

## Delivery guarantees ✅

- Start, checkpoint and end are sent inline (in a worker thread for async agents) and wait for the answer.
- Decisions and spans go from one background thread, in order. **At most once:** nothing is retried, so a network error loses them. `flush()` (also run at exit) waits for the queue to drain.
- Everything except numbers and run IDs passes through the SDK's optional `redact` hook first, which can change or drop it. Don't assume a field is present.

---

## Changes to this file

- **0.3 (unreleased):** tool and model calls as OpenTelemetry-shaped spans (`/spans`), the `X-Fences-SDK` header, optional limits and dashboard-owned limits (null at run start, no-budget runs allowed with notices), run-start `context` and the effective-limits adoption, quarantine (423), `exception` on end, heartbeat, commands, approvals, the `paused` breach. Server step counting and timing documented as authoritative. `raise_on_breach` is gone from the SDK, so breaches are only ever results.
