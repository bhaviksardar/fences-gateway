# Security review (Phase 7)

This review checks the threat model in [decisions.md](decisions.md#6-threat-model) against the code at 0.1.0 plus Phase 7, as of 2026-10-31. Each mitigation is checked in code, and the table names the test that would fail if it broke. Dependency scan: `govulncheck ./...` found **no vulnerabilities**. The image builds with the current Go 1.26 patch release (`golang:1.26`), on distroless `static:nonroot`.

## Compromised gateway

| Mitigation | Status | Evidence |
|---|---|---|
| Its key reports only for the agents assigned to it | Holds. Another team's or an unassigned agent's events aren't stored (`not_served`), and its approval requests get `403`. | platform `test_gateways.py` (cross-team, `not_served`) |
| The platform can revoke its key | Holds. A revoked gateway gets `401` and keeps its last bundle, so it never fails open. | `test_gateways.py` revoke; `pkg/sync` 401 test |
| Gaps in decision events are detectable | Partly. `seq` never repeats, the platform dedupes by `acked_seq`, and dropped events are counted in health (`dropped_events`), which the dashboard shows. The platform doesn't flag a `seq` gap on its own. | `pkg/queue` tests |
| Signed releases and an SBOM | Built, not yet run. A tag builds the image with an SBOM and provenance, signs it with Sigstore (keyless, bound to the release workflow), and publishes the Helm chart. Verify with `cosign verify ghcr.io/bhaviksardar/fences-gateway:<v> --certificate-identity-regexp 'github.com/bhaviksardar/fences-gateway/.github/workflows/release.yml' --certificate-oidc-issuer https://token.actions.githubusercontent.com`. | `.github/workflows/release.yml` |
| **New since the model: Kubernetes containment rights** | Bounded. A compromised gateway can update RoleBindings and ClusterRoleBindings cluster-wide, but can only **bind** the roles in `kubernetes.writeRoles`. So it can hand those roles to anyone, but nothing beyond them. | Helm `templates/kubernetes.yaml` |

## Compromised platform

| Mitigation | Status | Evidence |
|---|---|---|
| Bundles signed with Ed25519, and no rollback | Holds. The exact bytes are verified, an older or equal version is refused, and the last good bundle is kept. | `pkg/bundle` tests |
| Commands can only make things stricter | Holds. A quarantine is released only by a signed bundle, and the Kubernetes containment follows the same rule. An approval answer is used only for a request this gateway asked. A platform can still approve anything that's waiting, which is its job. | `internal/engine` (`Quarantined`), `internal/k8s` containment test, `approvals.Answer` |
| Pinning the signing key | Holds. `pinned_signing_keys` in the config replaces the keys from enrollment. | `cmd/fences-gateway` |
| **Signing key in a KMS or HSM** | **Open.** The platform reads `FENCES_BUNDLE_SIGNING_KEY` from its environment, in the web app's process. Anyone with the app's environment can sign a permissive bundle. Fix: sign through a KMS (AWS KMS and GCP KMS both support Ed25519), or a separate signing service. | `fences-platform/backend/gateways.py` `signer()` |

## Malicious agent

| Mitigation | Status | Evidence |
|---|---|---|
| Identity from the credential, never the agent's word | Holds. A token is matched by SHA-256 in constant time on every request. A Kubernetes agent is the username the API server authenticated. A hook uses the developer's token. | `mcp_test`, `k8s_test`, `fences-hook` test |
| Hidden tools, and `tools/call` checked anyway | Holds. | `mcp_test` |
| Parsed values; repeated keys refused | Holds for MCP arguments, hook input, HTTP bodies, and HTTP query parameters. An HTTP body that's a JSON object is read whatever its Content-Type says. | `mcp_test`, `fences-hook` test, `proxy_test` |
| Limits and amounts reserved before forwarding | Holds. Limits are now running counts, and a randomized test checks them against the spec's full-history definition. | `engine_test` (`TestCountsMatchHistory`), `proxy_test` |
| Quarantine at once | Holds. Held calls end too, and on Kubernetes the write bindings lose the agent. | `mcp_test`, `e2e_k8s.py` |
| **New: tunneling through the proxy** | Fixed. Protocol upgrades (WebSocket) are refused. | `proxy_test` |
| **New: path variants on HTTP** | Documented. An API may treat `/refunds/` or `/Refunds` as `/refunds`. Use `default: deny` with allow rules, so variants are refused. | `docs/http-proxy.md` |
| **New: flooding** | Bounded. Unauthenticated events are capped at 10 a second, pending approvals at 50 per run, and Slack posts at 20 a minute per team. | engine, platform tests |
| **New: Claude turning off its own hook** | Documented. Claude could edit `~/.claude/settings.json`. Deploy the hook in managed settings. | `docs/claude-code.md` |

## Malicious upstream

| Mitigation | Status | Evidence |
|---|---|---|
| No policy from upstream data | Holds. `tools/list` filtering uses only tool names, so `readOnlyHint` and other annotations are ignored. | `internal/mcp` `listTools` |
| Response caps and timeouts | Holds. MCP replies are capped at 16 MB, calls at 2 minutes. HTTP responses are streamed, not buffered, with a 2-minute cap. | `mcp_test`, `proxy.CallTimeout` |
| Separate credentials | Holds. The MCP gateway uses its own session to the upstream, and the HTTP proxy strips the agent's `Authorization`, then adds the API's. | `proxy_test` |

## New findings, open

1. **The admission webhook doesn't authenticate its caller.** `/k8s/admit` answers anyone who can reach the gateway's port. A forged AdmissionReview can't let a write through, because only the API server's own call decides that. But it can:
   - use up an agent's limits;
   - raise approval requests (capped);
   - put made-up entries on the agent's page.

   Fix: restrict ingress to the gateway's port with a NetworkPolicy (the API server's addresses are cluster-specific). Or configure the API server to present a client certificate to webhooks, and have the gateway require it. The audit endpoint is already authenticated with its bearer token.
2. **The spec's `header` credential type isn't implemented.** Agents authenticate with `Authorization: Bearer`. An agent given only a header credential is refused, so this fails closed. The spec now marks the type as reserved, since the platform doesn't issue these yet.
3. **A Kubernetes dry run counts against limits.** `kubectl --dry-run=server` is decided and reserved like the real request. This hurts only the agent doing it.
4. **Signing key in the app's environment** (above).

None of these lets a request through that policy denies.

## Adversarial audit (2026-10-31, after 0.1.0)

An agent acting as an attacker read the code and proved findings with tests against the real handlers. It was stopped before it covered Kubernetes, the shared limit store and the platform side. Everything it proved is fixed in Unreleased (see `CHANGELOG.md`), each with a test that fails on the old code:

| Finding | Severity | Fix |
|---|---|---|
| `%2e%2e` and `..%2F` in an HTTP path passed `/public/*` and reached `/admin` | critical | `.` and `..` segments, backslashes and NULs refused |
| JSON keys in another case (`AMOUNT`): decided on one value, acted on another | high | case-folded repeats refused; field lookup falls back to the case-folded key |
| The platform's `429` on approval requests let `on_unavailable: allow` through | high | a 4xx from the platform denies |
| A rule the platform accepts and the gateway refuses froze all later bundles | high | reason length in characters; platform refuses non-float numbers |
| The hook let a call run on a reply without `allowed: true` | medium | only an explicit allow |
| Agent-chosen `per` values held memory for the whole window | medium | count keys hashed |
| MCP sessions were never freed | medium | 10-minute idle timeout, 60 new sessions a minute per agent |
| Approval summary: padding or many arguments hid what mattered | medium | values show their start and end; left-out arguments counted |
| HTML escaping grew queued events sixfold | low | events stored unescaped |

Still open: a policy the gateway can't parse still keeps the previous bundle, which is the safe choice for a bad bundle but also holds back revocations. The platform now refuses what the gateway would. Quarantine commands work either way.

