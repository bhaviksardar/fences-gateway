# Fences rules

How a policy decides whether an agent's request goes ahead. Fences Gateway (MCP, HTTP, Kubernetes) and Fences Edge enforce rules from this file, using the policy in a signed bundle ([PROTOCOL-GATEWAY.md](../PROTOCOL-GATEWAY.md)). Every engine (the Go one in `pkg/policy`, and the reference one in [`conformance/reference.py`](../../conformance/reference.py)) must pass the cases in [`spec/conformance/`](../conformance). When an engine and this file disagree, fix the engine or change this file first, and add a case.

Conditions use the same shape as the dashboard's incident rules (`where: [{field, op, value}]`), so a team writes both kinds the same way.

## Policy

```json
{
  "default": "deny",
  "quarantined": ["support-bot"],
  "rules": [
    {"id": "no-delete", "on": "mcp", "agents": ["support-bot"], "effect": "deny",
     "where": [{"field": "tool", "op": "is", "value": "delete_ticket"}],
     "reason": "Tickets can't be deleted by agents"}
  ]
}
```

| Field | Meaning |
|---|---|
| `default` | `allow` or `deny`: the decision when no rule matches. **Absent means `deny`.** |
| `quarantined` | Agent names. Every request from them is denied, whatever the rules say. |
| `rules` | List of rules, below. Order doesn't matter. |

## Rules

| Field | Required | Meaning |
|---|---|---|
| `id` | yes | Unique within the policy, 1–64 characters of `A-Z a-z 0-9 _ -`. Reported with each decision. |
| `on` | yes | The subject: `mcp`, `http` or `k8s`. A rule only matches requests of its subject. |
| `effect` | yes | `allow`, `deny` or `approve` (allowed only with a person's approval). |
| `agents` | no | Agent names it applies to. Absent or empty: every agent. |
| `where` | no | Conditions, all of which must hold. Absent or empty: every request of the subject. |
| `when` | no | A time window the rule applies in (see Time windows). Outside it, the rule doesn't match. |
| `limit` | no | A rate or amount cap, on `allow` and `approve` rules only (see Limits). Ignored on `deny`. |
| `reason` | no | Shown to the agent and the developer when this rule denies, limits or asks for approval. |
| `on_unavailable` | no | `allow` or `deny`: what the gateway does when it can't decide because the platform is unreachable. Absent means `deny`. Not part of the engine's decision; see PROTOCOL-GATEWAY.md, Fail mode. |

An engine rejects a policy with an unknown `on`, `effect` or `op`, a missing, malformed or duplicate `id`, or a `default` other than `allow`/`deny`. It never guesses.

## Requests

What the engine decides on. Every request has `agent` (the name the gateway resolved from the credential the agent presented, never something the agent said), `on`, and `at` (Unix seconds). The other fields depend on the subject:

| `on` | Fields |
|---|---|
| `mcp` | `server` (the upstream's name in the gateway's config), `tool`, `args` (the call's arguments object) |
| `http` | `method` (uppercase), `path` (without the query string), `query` (object), `body` (the parsed JSON body, if any) |
| `k8s` | `verb` (`create`, `update`, `patch`, `delete`, `get`, `list`, `watch`…), `group`, `kind` (e.g. `Pod`), `namespace` (`""` for cluster-scoped), `name`, `subresource` |

## Conditions

`{"field": "args.amount", "op": "gt", "value": 100}`

**`field`** is a dotted path into the request: `tool`, `args.order_id`, `body.refund.amount`. A segment is an object key. There are no array indexes. A path that doesn't resolve (a missing key, or a step into something that isn't an object) is **missing**.

| `op` | Holds when |
|---|---|
| `is` | the value is a string equal to `value` |
| `is_not` | not `is` (so it holds for a missing value) |
| `contains` | the value is a string containing `value` |
| `not_contains` | not `contains` (so it holds for a missing value) |
| `matches` | the value is a string matching the glob `value`: `*` is any run of characters (including `/` and none), `?` is one character, everything else is literal. The whole string must match. |
| `in` | the value is a string equal to one of `value` (a list of strings) |
| `gt`, `gte`, `lt`, `lte`, `eq` | the value is a number and compares so with `value` |

Text matching is **case-sensitive** and on strings only: a number, boolean, object, array or null never `is`, `contains`, `matches` or is `in` anything.

For the number ops, a **number** is a JSON number (not a boolean), or a string that is a plain decimal (`^-?[0-9]+(\.[0-9]+)?$`, e.g. `"240"`, `"99.50"`). That way an agent can't get past `amount gt 100` by sending `"amount": "1000"` to an API that accepts either. Anything else (missing, `"1e3"`, `"$5"`, `null`) makes every number op false, so a `deny` rule on it doesn't match. If that matters, add a rule that denies when the field isn't a number, or validate upstream.

## Time windows

`"when": {"days": ["mon", "tue", "wed", "thu", "fri"], "from": "09:00", "to": "17:00", "tz": "Europe/London"}`

- `tz`: an IANA time zone. Absent means `UTC`.
- `days`: lowercase three-letter days, in `tz`. Absent: every day.
- `from` / `to`: `HH:MM` in `tz`. The window is `from` ≤ time < `to`. If `to` ≤ `from`, it runs overnight (`22:00` to `06:00`), and `days` refers to the day it starts. Both absent: all day.

## Limits

On an `allow` or `approve` rule. It caps what that rule lets through, per agent:

```json
"limit": {"window_s": 600, "max": 3, "per": ["namespace"]}
"limit": {"window_s": 3600, "max": 500, "sum": "body.amount"}
```

| Field | Meaning |
|---|---|
| `window_s` | The window, in seconds. |
| `max` | Without `sum`: how many requests the rule lets through in the window. With `sum`: the most the field may add up to. |
| `sum` | A field path. Requests count by that field's number (see Conditions) instead of by 1. A request whose field isn't a number counts as **over the limit**, since its amount can't be checked. |
| `per` | Field paths. Requests are counted separately for each combination of these values ("3 restarts per namespace"). Absent: one count for the agent. |

**History** is the requests the gateway already let through (decision `allow`), with their `at`. A request counts against a rule's limit if it was from the same agent, of the rule's `on`, matches the rule's `where`, has the same `per` values, and is in the window: `at > now − window_s`. The request being decided counts too: it is over the limit if `count + 1 > max`, or `total + amount > max` with `sum`.

The gateway reserves before forwarding: once a request is allowed, it's in the history straight away, so two racing requests can't both take the last slot.

## Deciding

Given a policy, a request, the history, and whether this request has a granted approval:

1. The agent is in `quarantined` → **`deny`** (`rule`: none, `reason`: quarantined).
2. Find the matching rules: same `on`, the agent is in `agents` (or there are none), every `where` condition holds, and the time is inside `when`.
3. Any matching `deny` rule → **`deny`**.
4. Any matching `allow` or `approve` rule with a `limit` this request would exceed → **`rate_limited`**.
5. Any matching `approve` rule, and no granted approval for this request → **`require_approval`**.
6. Any matching `allow` or `approve` rule → **`allow`**.
7. Otherwise → `default` (`deny` when absent).

The result is `{decision, rule, reason}`. `rule` is the `id` of the rule that decided it. When several rules decide at the same step, `rule` is the one whose `id` sorts first, so all engines report the same one. `rule` is null at steps 1 and 7.

## Run limits

The limits on a governed run (budget, steps, time, tokens), as the SDKs and the platform enforce them ([PROTOCOL.md](../PROTOCOL.md)). They aren't policy rules, but every engine that enforces them must agree, so they have conformance cases too ([`run.json`](../conformance/run.json)). The gateway doesn't enforce them.

A run has `spent_usd`, `iterations`, `duration_ms` and `tokens_used`, and limits `budget_usd` (null: no budget), `max_iterations`, `max_duration_ms` and `max_tokens` (0: no limit).

**Checkpoint.** A limit trips once it's **exceeded**, not when it's reached. When several trip at once, the first in this order is reported:
1. `budget_exceeded`: `round(spent_usd, 9) > budget_usd`
2. `iteration_limit`: `iterations > max_iterations`
3. `time_limit`: `duration_ms > max_duration_ms`
4. `token_limit`: `max_tokens > 0` and `tokens_used > max_tokens`

Spend is rounded to 9 decimal places (as Python's `round(x, 9)`) before comparing, so float drift doesn't trip a budget: five $0.02 calls fit a $0.10 budget.

**Guard** (a guarded tool asking to run with `amount_usd`), in this order:
1. The run was stopped → `stopped_by_user`.
2. The run already has a breach, or a checkpoint would find one → that breach.
3. `budget_usd` is set and `round(spent_usd + reserved_usd + amount_usd, 9) > budget_usd` → `budget_exceeded`.
4. Otherwise allowed, and `amount_usd` is added to `reserved_usd` before the next check, so parallel calls can't overspend together.

## Changes to this file

- **Unreleased:** first version, for Fences Gateway: subjects `mcp`, `http`, `k8s`; conditions; time windows; limits; decision order.
