# Fences SDK ↔ server protocol

What the `agentfences` SDK sends to a Fences server, and what it expects back. The SDK in this repo and the server in `fences-platform` both implement this file; when they disagree, fix one of them or change this file first.

**Status markers**

| | Meaning |
|---|---|
| ✅ | Live: today's SDK and server both do this |
| 🆕 | SDK 0.3 (unreleased) sends or expects this; the server has to build it |
| 📝 | Designed here, built on neither side yet |

SDK 0.3 and the server changes ship together, after the server passes `tests/test_sdk_e2e.py` against the unreleased SDK.

### Server work for SDK 0.3

- [ ] Run start: store `context`; answer `423` for a quarantined agent
- [ ] Run end: store `exception`
- [ ] `POST /api/runs/{run_id}/events`: store tool and model calls; show them on the run
- [ ] `POST /api/heartbeat`: record last heartbeat per run; return pending commands
- [ ] Send `stop`, `pause`, `resume`, `limits` commands from the dashboard actions (in heartbeat and checkpoint responses)
- [ ] `POST /api/runs/{run_id}/approvals`: store, notify, deliver the answer as an `approval` command, raise `budget_usd` by a granted amount
- [ ] Quarantine an agent from the dashboard
- [ ] `tests/test_sdk_e2e.py`: stop using `raise_on_breach` and `BudgetExceeded` (removed in SDK 0.3); it doesn't import against the new SDK
- [ ] `dashboard/docs.html`: drop the exception classes and `raise_on_breach`

---

## Conventions ✅

- **Transport:** HTTPS, `POST` with a JSON body, JSON responses. The SDK waits 3 seconds per request.
- **Auth:** every request carries `X-API-Key: fc_...`. A key belongs to one account and one named agent; runs a key starts are reported under that agent's name, whatever `agent_name` the SDK sends.
- **Ownership:** a key only sees and touches runs of its own account. Another account's run, or one that doesn't exist, is `404 {"detail": "Run not found"}`.
- **Run IDs** are UUIDs chosen by the SDK.

### How the SDK reads responses

| Response | Core endpoints (start, checkpoint, decisions, end) | Optional endpoints (events, heartbeat, approvals) |
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
| `budget_usd`, `max_iterations`, `max_duration_ms`, `max_tokens` | ✅ | The limits in code. `max_tokens: 0` means no limit. The server caps each at the key's ceiling |
| `context` | 🆕 | Optional. String → string, at most 32 keys, values up to 256 chars, already through the SDK's redact hook. Set by `init(environment=, release=)` and `agentfences.context(...)`. The server stores it on the run and lets the dashboard filter by it |

**Response** ✅ `200 {"ok": true, "limits": {"budget_usd": 0.5, "max_iterations": 100, "max_duration_ms": 300000, "max_tokens": 0}}`: the run's effective limits after capping.
🆕 The SDK adopts `limits`, so its local fallback (when the server is unreachable later) enforces the same numbers as the server.

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

### `POST /api/runs/{run_id}/events` 🆕

Tool calls and model calls, sent in the background, in order with decisions, batched up to 50 per request.

```json
{"events": [
  {"type": "tool_call", "ts": 1791161641.07, "iteration": 3, "name": "web_search",
   "args": "query='cats', limit=5", "ok": false, "error": "PermissionError: 403 from search API", "latency_ms": 412},
  {"type": "llm_call", "ts": 1791161642.30, "iteration": 3, "provider": "openai", "model": "gpt-4o-2024-08-06", "ok": true,
   "latency_ms": 1830, "input_tokens": 600, "cache_read_tokens": 400, "cache_write_tokens": 0, "output_tokens": 200, "cost_usd": 0.004}
]}
```

| Type | Fields |
|---|---|
| `tool_call` | `name`, `args` (summary, ≤300 chars), `ok`, `error` (≤1000 chars, null if ok), `latency_ms` |
| `llm_call` | `provider` (`openai`, `anthropic`), `model`, `ok`, `latency_ms`; with usage: `input_tokens` (uncached), `cache_read_tokens`, `cache_write_tokens`, `output_tokens`, `cost_usd`; streamed calls: `stream: true` and no usage; failures: `error`, `status` (HTTP status, e.g. 429) |
| `approval_requested` | `approval_id`, `reason`, `amount_usd` |
| `approval_answered` | `approval_id`, `granted`, `by`, `note`, `amount_usd` |

Every event has `type`, `ts` (Unix seconds) and `iteration` (the run's step count when it happened). Unknown types and fields must be ignored, so new ones can be added without a server release. `llm_call` events are a record only: their cost is **not** spend (spend arrives through checkpoints), so don't add them to the run's total. Response body is ignored.

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
{"approval_id": "0c3e...", "reason": "Refund $240 to order 1182?", "amount_usd": 240}
```

- `approval_id` is a UUID chosen by the SDK; treat a repeat as the same request.
- **Response:** `{"ok": true}` once the request is stored and someone is notified.
- Show it to the on-call person with approve/deny. Their answer goes back as an `approval` command, with `by` (who answered, e.g. their email) and an optional `note`.
- **If an amount is granted, add it to the run's `budget_usd` on the server too.** The SDK raises its local budget by the same amount; if only one side does, the next checkpoint breaches.
- The SDK stops waiting after `timeout_s`. A late answer should be shown as expired, not applied.

### Quarantine

A quarantined agent: new runs get `423` at start (above); running runs get `pause`.

---

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
- Decisions and events go from one background thread, in order. **At most once:** nothing is retried, so a network error loses them. `flush()` (also run at exit) waits for the queue to drain.
- Everything except numbers and run IDs passes through the SDK's optional `redact` hook first, which can change or drop it. Don't assume a field is present.

---

## Designed, not built 📝

### SDK version on every request

The SDK will send `X-Fences-SDK: python/0.3.0`. The server stores the latest version per key, shows it on the agent's page, and warns about old ones. It should not send commands a version doesn't understand (anything before 0.3 has no commands at all).

### Limits owned by the dashboard

Under an on-call model, limits belong to whoever runs the agent. The SDK will allow a bare `@governed()`:

- At start it sends `"budget_usd": null` (and null for any limit not set in code).
- The server fills each null from the agent's limits in the dashboard, and returns the effective set in `limits` as today.
- **Open question:** what if neither code nor dashboard sets a budget? Proposal: reject the start with `422 {"detail": "Set a budget for <agent> in the dashboard, or pass budget_usd"}`, which the SDK turns into a clear exception, rather than running unlimited.
- Local mode has no dashboard, so there a bare `@governed()` will need limits in code or use conservative defaults.

---

## Changes to this file

- **0.3 (unreleased):** run-start `context` and the effective-limits adoption, quarantine (423), `exception` on end, the events endpoint, heartbeat, commands, approvals, the `paused` breach. Server step counting and timing documented as authoritative. `raise_on_breach` is gone from the SDK, so breaches are only ever results.
