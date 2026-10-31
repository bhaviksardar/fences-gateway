# Fences Gateway ↔ platform protocol

What a Fences Gateway (or a Fences Edge device) sends to the Fences platform, and what it expects back. The gateway in this repo and the platform in `fences-platform` both implement this file. When they disagree, fix one of them, or change this file first.

The gateway decides every request locally, from a signed policy bundle it pulls from the platform ([rules/README.md](rules/README.md)). The platform is never on the hot path. It issues bundles, receives decision events, answers approvals and sends quarantines.

The SDK's protocol is a separate file, [PROTOCOL.md](PROTOCOL.md). This one reuses its ideas (commands with `id`s, acknowledgements, approvals), adapted to a long-lived process that serves many agents.

---

## Conventions

- **Transport:** HTTPS, `POST` with a JSON body, JSON responses. The gateway waits 5 seconds per request.
- **Version:** every request carries `X-Fences-Client: gateway/0.1.0` (`device/0.1.0` from Fences Edge).
- **Auth:** enrollment uses a one-time token. Every request after that is signed with the gateway's own key (see Signing requests).
- **Ownership:** a gateway only receives bundles, commands and approvals for the agents its team assigned to it, and can only report for those agents. An event or approval request for any other agent is rejected (`403`), and nothing in the batch is stored.

| Response | What the gateway does |
|---|---|
| 2xx with JSON | Uses the body |
| 401 | Its key was revoked or never enrolled. It keeps enforcing the last good bundle, stops syncing, and logs an error every minute. It never fails open because of this. |
| 403 | The batch named an agent this gateway doesn't serve. It drops that batch and logs it. |
| 409 | A repeat the platform already has. Treated as success. |
| 429, 5xx, timeout, non-JSON | Backs off and retries (see Sync) |

---

## Enrollment: `POST /api/gateways/enroll`

Sent once, at first start. The gateway creates an Ed25519 key pair and keeps the private key on disk (mode `0600`). The private key never leaves the gateway.

```json
{"token": "fge_...", "kind": "gateway", "name": "prod-mcp-1", "public_key": "<base64, 32 bytes>", "version": "0.1.0"}
```

- `token`: created in the dashboard. Single use, expires after 24 hours.
- `kind`: `gateway` or `device`.

**Response:**
```json
{"gateway_id": "gw_8f2c...", "signing_keys": [{"key_id": "k1", "public_key": "<base64>"}], "sync_interval_s": 15}
```

`signing_keys` are the platform's public keys for verifying bundles. A gateway configured with `pinned_signing_keys` uses only those and ignores the list here, so a platform that swaps keys can't push a bundle (threat model, [decisions.md](../docs/decisions.md)).

A used, expired or unknown token gets `401`.

## Signing requests

Every request after enrollment carries:

| Header | Value |
|---|---|
| `X-Fences-Gateway-Id` | `gateway_id` |
| `X-Fences-Timestamp` | Unix seconds |
| `X-Fences-Signature` | base64 Ed25519 signature, with the gateway's key, of `POST\n<path>\n<timestamp>\n<hex sha256 of the body>` |

The platform rejects a request with `401` if its timestamp is more than 300 seconds from the platform's clock, if the signature is bad, or if the gateway is revoked. A request replayed within those 300 seconds does no harm: syncs are reads, events are deduplicated by `seq`, and approval requests by `approval_id`.

---

## Sync: `POST /api/gateways/sync`

This is the gateway's heartbeat and bundle pull in one request. It's sent every `sync_interval_s` (default 15 s) with ±20% jitter, and every 2 s while an approval is pending.

```json
{"bundle_version": 41,
 "acks": [{"id": "quarantine:support-bot:1793610000", "type": "quarantine", "received_at": 1793610001.2}],
 "health": {"version": "0.1.0", "uptime_s": 86400, "queue_depth": 12, "dropped_events": 0, "bundle_error": null,
            "decisions_per_s": 140.5, "p99_added_ms": 1.8, "upstream_errors": {"crm": 0, "mail": 3}}}
```

**Response:**
```json
{"bundle": {...}, "commands": [...]}
```
- `bundle` is present only when the platform has a newer version than `bundle_version`.
- `commands` works as in PROTOCOL.md: commands are derived from state and re-sent until the gateway acknowledges them.

**When it fails** (network error, `429`, `5xx`), the gateway retries with exponential backoff: 1 s, doubling, up to 5 minutes, with full jitter. After 5 failures in a row, the circuit opens and the gateway tries once a minute until a sync succeeds. Throughout, it keeps enforcing its last good bundle (see Fail mode).

## Bundles

```json
{"key_id": "k1", "payload": "<base64 of the payload's JSON bytes>", "signature": "<base64 Ed25519 signature of those bytes>"}
```

The gateway verifies the exact bytes it received, then parses them. It never re-serializes before checking. The payload:

```json
{"version": 42, "gateway_id": "gw_8f2c...", "issued_at": 1793610000,
 "agents": {
   "support-bot": {"credentials": [{"type": "token", "sha256": "9c1e..."}], "upstreams": ["crm", "mail"]},
   "sre-bot": {"credentials": [{"type": "k8s_service_account", "name": "system:serviceaccount:ops:sre-bot"}]},
   "refund-vendor": {"credentials": [{"type": "header", "header": "X-Api-Key", "sha256": "71ab..."}]}
 },
 "policy": {"default": "deny", "quarantined": [], "rules": [...]},
 "approval_timeout_s": 600}
```

| Field | Meaning |
|---|---|
| `version` | Increases by at least 1 with every new bundle for this gateway. |
| `agents` | Every agent this gateway serves, and how to recognize it. A **token** is one Fences issued, sent by an MCP client as `Authorization: Bearer`. A **header** is an API key the vendor sends in a named header. Only SHA-256 hashes are in the bundle, never the secrets. |
| `upstreams` | The upstream MCP servers (names from the gateway's config) the agent may reach. Upstream credentials stay in the gateway's config and are never in a bundle. |
| `policy` | A policy, per [rules/README.md](rules/README.md). |
| `approval_timeout_s` | How long a call waits for an approval before it's denied. |

**Accepting a bundle.** The gateway accepts a bundle only if all of these hold:
- `key_id` is a signing key it trusts
- the signature verifies
- `gateway_id` is its own
- `version` is greater than the version it has
- the policy is valid per the [rules spec](rules/README.md) and [schema](rules/schema.json)

Otherwise it keeps the bundle it has and reports why in `health.bundle_error`. An older version is always refused, so a replayed bundle can't roll the policy back.

An accepted bundle is written to disk atomically (a temp file, then a rename) as the **last good bundle**, and applied to the next request. Requests already in flight finish under the bundle they started with.

**Identity.** Each request's agent comes from the credential it presents, matched against `agents`, never from anything the agent says about itself. If the credential matches no agent, the request is refused (`401` over HTTP, an MCP auth error, a denied admission). It's recorded as a decision with `agent: null` and `reason: "unknown credential"`.

---

## Decision events: `POST /api/gateways/decisions`

One event for every request the gateway decides. Events go into a local SQLite queue before the request is answered. They're sent in batches every 2 s, or as soon as 100 are waiting, up to 500 per batch.

```json
{"events": [
  {"seq": 1048, "at": 1793610000.12, "agent": "support-bot", "on": "mcp", "target": "crm/get_ticket",
   "arguments": ["id"], "decision": "allow", "rule": "crm-reads", "reason": null, "bundle_version": 42,
   "degraded": false, "approval_id": null, "latency_ms": 0.8, "upstream_status": "ok", "upstream_ms": 212}
]}
```

| Field | Meaning |
|---|---|
| `seq` | Per gateway: starts at 1, increases by 1, is stored on disk and never reused. |
| `on`, `target` | `mcp`: `server/tool`. `http`: `METHOD /path`. `k8s`: `verb kind namespace/name`. |
| `arguments` | Argument (or body field) **names only**, by default. With `report_argument_values: true` in the gateway's config, it's an object of the values instead. Names and values are each clipped to 300 characters. |
| `decision`, `rule`, `reason` | From the rules spec, plus `unauthenticated` (unknown credential). |
| `degraded` | `true` if the fail mode decided this, not the policy (see Fail mode). |
| `latency_ms` | Time the gateway added: policy, queue and bookkeeping. It doesn't include the upstream. |
| `upstream_status` | `ok` or `error` for MCP, the status code for HTTP, `null` for k8s or when the call wasn't forwarded. |

**Response:** `{"acked_seq": 1048}`, the highest `seq` up to which the platform has stored every event. The gateway deletes events up to it and sends the rest again. An `acked_seq` above the highest `seq` the gateway has sent is taken as that `seq`, so a wrong answer can't delete events that were never sent. Delivery is **at least once**, and the platform stores each `(gateway_id, seq)` once.

If the platform is unreachable for a long time, the queue keeps up to 100,000 events (configurable). Past that it drops the oldest and counts them in `health.dropped_events`. It never blocks requests because the queue is full.

**On the platform:** each event becomes a span on a synthetic run for that agent, one run per agent per hour. That way incidents, detectors and replay work as they do for SDK runs.

---

## Approvals: `POST /api/gateways/approvals`

Sent when the policy says `require_approval`.

```json
{"approval_id": "0c3e...", "agent": "refund-vendor", "on": "http", "target": "POST /refunds", "rule": "big-refunds",
 "reason": "Refunds over $100 need approval", "summary": "POST /refunds {\"order_id\": \"1182\", \"amount\": 240}",
 "request_hash": "5d41...", "timeout_s": 600}
```

- `approval_id`: a UUID the gateway chooses. A repeat is the same request.
- `summary`: the full request, with values, so the person approving sees what they're approving. Values are clipped, control characters escaped, and the whole summary is at most 2,000 characters, as in PROTOCOL.md.
- `request_hash`: SHA-256 of the request as the rules spec describes it, minus `at`, serialized as JSON with sorted keys and no whitespace. An approval applies only to a request with this hash.

**Response:** `{"ok": true}` once the request is stored and someone has been notified (Slack, PagerDuty, dashboard).

The answer comes back as a command:

```json
{"type": "approval", "id": "approval:0c3e...", "approval_id": "0c3e...", "granted": true, "by": "dana@example.com", "note": ""}
```

**How each surface waits:**
- **MCP and HTTP:** the call is held open until the answer arrives or `timeout_s` passes. A grant re-decides the call with `approved: true`, because the limits may have changed while it waited. A denial or a timeout returns the deny to the agent.
- **Kubernetes:** an admission webhook can't hold a request open. The gateway denies it at once with a message naming the approval (`Waiting for approval 0c3e...: Credential rotation needs approval. Retry once it's approved.`). If a request with the same `request_hash` comes back from the same agent within 10 minutes of the grant, it's decided with `approved: true`.

Each approval can be used once. The platform expires it at `timeout_s` and shows a late answer as expired.

---

## Commands

Commands arrive in sync responses and are applied in the order listed. Unknown types are ignored. Every command has an `id` and is acknowledged once, in the next sync's `acks`.

| Command | Effect in the gateway |
|---|---|
| `{"type": "quarantine", "id", "agent", "bundle_version", "by"?, "reason"?}` | Every request from `agent` is denied at once, and its open MCP sessions and held calls are closed. On Kubernetes, the gateway runs its containment action: it removes the agent's write role binding and keeps read access. This holds until the gateway applies a bundle whose `version` is at least `bundle_version`. From then on, the bundle's `quarantined` list decides. |
| `{"type": "approval", "id", "approval_id", "granted", "by", "note"?}` | Answers a pending approval (see Approvals). |

**There's no release command.** A quarantine is released by a new bundle that no longer lists the agent. Bundles are signed and commands aren't, so the only thing a command can do is make the gateway stricter, apart from answering an approval the gateway itself asked for. Someone who can send commands but can't sign bundles can't open anything up.

**Acknowledgements**, as in PROTOCOL.md:
```json
{"id": "quarantine:sre-bot:1793610000", "type": "quarantine", "received_at": 1793610001.2}
{"id": "quarantine:sre-bot:1793610000", "type": "quarantine", "containment": "failed", "detail": "rolebindings.rbac.authorization.k8s.io \"sre-bot-write\" is forbidden", "finished_at": 1793610002.0}
```
- The receipt ack is sent as soon as the command is applied, and triggers an immediate sync.
- A quarantine with a containment action gets a second ack when the action finishes. Its `containment` is `ok` or `failed` (with `detail`, ≤300 characters).
- **On the platform:** no receipt within 30 seconds means the gateway didn't get the quarantine. Show it on the incident, fire the agent's containment webhooks, and page again.

---

## Fail mode

The gateway keeps deciding from its last good bundle when the platform is unreachable, so most requests never notice. The fail mode applies only when the policy can't decide on its own ([decisions.md](../docs/decisions.md), section 4):

| Situation | The gateway |
|---|---|
| No bundle yet (first start, platform unreachable), or no bundle on disk passes verification | Denies every request |
| `require_approval` and the approval request can't be sent | Uses the matching rule's `on_unavailable` (default `deny`) |
| Several replicas share a rate window (Phase 7) and the shared store is unreachable | Counts locally and keeps deciding. The event carries `degraded: true` |
| Quarantine | Unaffected: it's in the bundle, or in a command already applied |

A decision made by the fail mode, not the policy, is reported with `degraded: true`.

---

## Changes to this file

- **Unreleased:** first version: enrollment, signed requests, sync, bundles, decision events, approvals, `quarantine` and `approval` commands, fail mode.
