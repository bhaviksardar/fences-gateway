# Decisions

Phase 0 decisions for Fences Gateway. Change one by editing this file in the same PR as the behavior change.

## 1. First design partner and surface

- **Design partner:** _open, needs a name._
- **First surface:** MCP. Switch to Kubernetes (Phase 6 first) only if the partner's agent is an AI SRE agent working on a cluster.

## 2. Language and libraries

- Go for `fences-gateway` and `fences-edge`.
- MCP handling uses `github.com/modelcontextprotocol/go-sdk`.
- Module path: `github.com/bhaviksardar/fences-gateway`.

## 2a. Where the gateway specs live

In this repo, not in `fences`. `fences` is the SDK repo, and none of the SDKs evaluate these rules.
- [`docs/PROTOCOL-GATEWAY.md`](PROTOCOL-GATEWAY.md): gateway ↔ platform.
- [`docs/RULES.md`](RULES.md): the rules.
- [`conformance/cases/`](../conformance/cases): the conformance cases.

`fences-edge` gets all of them along with `pkg/`. If an SDK ever enforces these rules locally, it copies the cases at a tagged version, the same way `fences-edge` takes `pkg/`.

This differs from the roadmap, which put them in `fences`.

## 3. Stability of `pkg/`

- `pkg/` (`policy`, `bundle`, `sync`, `queue`, `decisions`) is a public API that `fences-edge` imports at a tagged version.
- Semantic versioning. Below 1.0, breaking changes go only in a new minor version (`v0.1.x` → `v0.2.0`), never in a patch.
- Every `pkg/` change gets a `CHANGELOG.md` entry, and the entry says whether it affects `fences-edge`.
- `pkg/` never imports `internal/`. Gateway-only code lives in `internal/`.

## 4. Fail mode

When the gateway can't reach the platform, it keeps deciding from the last good bundle on disk. Most calls are decided that way and never need the platform. The fail mode covers what happens when a decision can't be made, for example:
- there's no bundle yet
- the bundle can't be verified
- the decision needs the platform (an approval, or a rate window shared across replicas) and the platform is down

**Default: fail closed for every call, reads included.** This matches the SDK, where guarded tools fail closed ([ENFORCEMENT.md](https://github.com/bhaviksardar/fences/blob/main/ENFORCEMENT.md)).

| Situation | Behavior |
|---|---|
| No valid bundle yet (new gateway, or the signature check failed) | Deny everything |
| Decision needs an approval and the platform is down | Deny (no answer means no approval) |
| Rate window shared across replicas and the platform is down | Count locally per replica, don't deny. It can overshoot by up to (replicas − 1) × limit and is logged as degraded |
| Quarantined agent | Deny (quarantine is in the cached bundle) |

- A rule can opt in to failing open with `on_unavailable: allow`. This is meant for reads the policy author knows are safe.
- What counts as a read is set by the policy, never by the upstream. MCP annotations such as `readOnlyHint` come from the upstream server, which may be malicious, so they never affect a decision.
- Weakening a fail-closed default needs a test that shows why.

Why not fail open on reads by default: with a cached bundle, the gateway keeps deciding locally even when the platform is down, so this table only applies when nothing has said what's allowed. A read can also be the harm (data exfiltration).

## 5. Latency budget

- Policy is decided locally from the cached bundle. A local decision adds at most **5 ms at p99** (measured in Phase 7: 1,000 decisions/s per replica).
- Nothing on the hot path waits on a network call to the platform. Decision events go into the local SQLite queue (`pkg/queue`) and are sent in batches.
- Approvals are the one exception. Waiting for the platform is asynchronous: the call is held (MCP, HTTP) up to the approval timeout, or denied and retried (Kubernetes). It never blocks other calls.
- Rate windows are kept in memory per replica until Phase 7, where they become shared across replicas, falling back to local counts when the platform is down (see section 4).

## 6. Threat model

**What we protect:** the customer's systems behind the gateway (MCP servers, APIs, clusters), the integrity of policy, and an honest record of what each agent did.

| Threat | What an attacker gets | Mitigations |
|---|---|---|
| **Compromised gateway** (host or process) | It sees all traffic through it and can skip policy for the agents it serves. It also holds the upstream credentials. | Least-privilege upstream credentials, one gateway per trust zone. The gateway's key can report only for the agents assigned to it (enforced by the platform). The platform can revoke the gateway's key. Decision events carry sequence numbers, so gaps are detectable. Signed releases and an SBOM (Phase 7). Not solved: a compromised gateway can lie about what it allowed. Audit connectors (k8s audit log, later GitHub and the major clouds) give an independent record. |
| **Compromised platform** | It can push any policy and any command. | Bundles are signed with Ed25519, and the gateway refuses older versions (no rollback). Commands can only make things stricter (quarantine) or answer approvals: a command can never widen access. Option: pin the signing key in the gateway's config so that a platform with a swapped key is rejected. Not solved: a platform holding the real signing key can sign a permissive bundle. Keep the signing key in a KMS/HSM, separate from the web app. |
| **Malicious agent** | It's an authenticated client trying to get more access. | Identity is the credential the agent presents (a Fences-issued token, a service account, an API key header), never something the agent says about itself. Tools the agent may never call are hidden from `tools/list`, and `tools/call` checks policy again anyway. Argument matchers work on parsed values, never on raw strings. A rate window is counted before the call is forwarded. Amounts are reserved before forwarding (Phase 5). Quarantine applies right away. |
| **Malicious upstream tool** | It controls the responses and tool descriptions, and may try to inject instructions or exfiltrate data. | The gateway never takes policy from upstream data: annotations such as `readOnlyHint` are ignored for decisions and fail mode. Upstream responses are passed through without being run. Response sizes are capped and timeouts enforced. Each upstream gets its own credentials, never the agent's Fences token. Out of scope for now: content inspection of responses (prompt injection). |

**Out of scope:** a compromised customer host running both the agent and its upstreams. The agent can't be stopped from going around the gateway by talking to upstreams directly. The fix is network policy, plus brokered credentials (see "Later" in the roadmap).
