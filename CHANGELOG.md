# Changelog

Changes to `spec/` and `pkg/` are listed here, because other repos pin them: `fences-edge` imports `pkg/` and follows `spec/`, and `fences-platform` checks against `spec/`. Entries that affect `fences-edge` are marked **(edge)**.

## Unreleased

Nothing yet.

## 0.1.0 (2026-10-31)

The first release: `pkg/` is the API `fences-edge` builds on, and `spec/` at this tag is what `fences-platform` checks against.

- `pkg/policy` **(edge)**: `DecideCounts` decides with limit counts the caller keeps. `Counts`, `HistoryCounts`, `Rule.CountsToward`, `Rule.LimitKey` and `Rule.Amount` are added. `Decide` is unchanged. `spec/rules/README.md` allows an engine that keeps counts to count a changed rule from the change.
- `cmd/fences-gateway`: on shutdown, calls held for an approval are denied at once ("approval wait ended"), and requests still open after 2 seconds are cancelled. A restart with a held call took 10 seconds and returned an error, and now takes about 2. `chaos_test.go` covers it, along with the platform being unreachable (the cached bundle keeps working, approvals fail closed, and a restart serves from disk), the upstream going down and coming back, and events kept across a restart (`docs/chaos.md`).
- Releases: one tag builds the image with an SBOM and provenance, signs it with Sigstore (keyless), and publishes the Helm chart to `oci://ghcr.io/bhaviksardar/charts`. The `pkg/` module is versioned by the same tag, which must match the chart and this file. `docs/security-review.md` reviews the threat model.
- `spec/PROTOCOL-GATEWAY.md` **(edge)**: the `header` credential type is reserved, since the platform doesn't issue one yet.
- `internal/engine` keeps running counts per limit instead of every allowed request. A decision with an hour-long limit at 1,000 requests a second went from a full scan of the history under the lock, which collapsed in about 30 seconds, to 0.024 ms p99. It holds 16 bytes per counted request. `docs/load-test.md` has the numbers.
- `spec/PROTOCOL-GATEWAY.md`, `pkg/sync` **(edge)**: on `401`, a client keeps the last good bundle and tries again once a minute, instead of stopping until restarted, so a corrected clock recovers. The default `sync_interval_s` is 2 seconds, not 15.
- `spec/rules/README.md` **(edge)**: how an admission request maps to a `k8s` request. Its `verb` is the operation, and the API server sends a patch as `update`. The agent is the one whose `k8s_service_account` is the authenticated username.
- `spec/PROTOCOL-GATEWAY.md` **(edge)**: Kubernetes reads come from the audit log as `allow` events with the reason "Seen in the cluster's audit log". Containment acts on bindings labeled `fences.dev/containment=write`, and moves the service account into the `fences.dev/removed-subjects` annotation and back.
- `spec/rules/README.md` **(edge)**: an `http` request has `server`, the API's name in the gateway's config. Its `path` is decoded, `query` values are strings, and a repeated query parameter is refused.
- `spec/rules/README.md` **(edge)**: a tool call reported through a hook (Claude Code's `PreToolUse`) is an `mcp` request with `server` set to the hook's name, `claude-code` by default.
- `spec/PROTOCOL-GATEWAY.md` **(edge)**: argument names are clipped to 300 characters, as values are. An `acked_seq` above the highest `seq` sent counts as that `seq`.
- `spec/PROTOCOL-GATEWAY.md` **(edge)**: an event for an agent the gateway doesn't serve isn't stored, and the rest of its batch is. The reply says how many in `not_served`. Any other 4xx answer to a batch drops it, so it can't hold up the queue behind it. An event's `target` is at most 1,000 characters, its `arguments` at most 50 entries, and a batch at most 512 KB.
- `pkg/queue` **(edge)**: `Batch(n, maxBytes)` takes a size limit, so this is a breaking change before the first release. `pkg/sync` drops a batch the platform rejects with a 4xx (other than 429) and adds `ErrRejected`. `pkg/decisions` clips `target` and lists at most 50 arguments.
- `spec/rules/README.md`: a gateway refuses `args` or `body` that repeat a key.
- `spec/rules/README.md`: a new section, Possible requests, says which tools `tools/list` hides.
- `pkg/policy`: the rules engine in Go **(edge)**. `Parse` rejects what `spec/rules/schema.json` rejects (apart from repeats in `quarantined` and `days`, which change nothing), plus duplicate ids, and data after the policy. `Decide` follows `spec/rules/README.md`, Deciding. It passes every policy case in `spec/conformance/`, which `conformance/conformance_test.go` checks in CI. Run limits aren't in it: the gateway doesn't enforce them.
- `pkg/policy`: `Possible` says whether a partly known request could ever be allowed, for `tools/list` filtering.
- `pkg/bundle` **(edge)**. `Verify` checks a bundle's signature over the exact payload bytes, its key, its `gateway_id` and its policy. `Store` keeps the current bundle, refuses any version that isn't newer, and keeps the last good bundle on disk, written atomically with mode `0600`. A newer bundle takes effect even if saving it fails, since it may carry a quarantine. Unknown payload fields are ignored, so the platform can add some.
- `pkg/decisions` **(edge)**. `Event` is the decision event, with unset fields sent as `null`. `Target` builds the `target` string for each subject. `Arguments` gives argument names by default, or values, with names and values clipped to 300 characters. `Summary` is the approval text, ported from the SDK's: every argument is shown, long values are cut, control, invisible and quote-like characters are escaped, and the whole is at most 2,000 characters. `RequestHash` is the approval's `request_hash`. Arguments are listed in key order, because a decoded JSON object doesn't keep the order it was sent in.
- `pkg/queue` **(edge)**: the SQLite event queue. `Push` gives each event its `seq`, which is never reused, even after a restart. `Batch` returns the oldest events with their `seq`, and `Ack` deletes up to `acked_seq`, capped at the highest `seq` sent. The database file is created with mode `0600`. Once the queue reaches its max (100,000 by default), the oldest events are dropped and counted in `Dropped`, so `Push` never blocks. A push takes about 20 µs. The first dependency is `modernc.org/sqlite`, which is pure Go so builds need no cgo. Go 1.26 is now the minimum. `decisions.Event` has no `Seq` field any more: the queue adds it.
- `pkg/sync` **(edge)**: talking to the platform, for `gateway` and `device` clients.
  - `Enroll` creates the client's Ed25519 key and saves its identity with mode `0600`. `Post` signs every request.
  - The URL must be https, or http to localhost, and redirects are never followed.
  - `Syncer` runs the heartbeat and bundle pull: ±20% jitter, every 2 s while an approval is pending, 1 s backoff doubling to 5 minutes with full jitter, and a circuit that opens after 5 failures and then tries once a minute.
  - A command is applied once and acknowledged each time the platform re-sends it. A malformed command is skipped. On `401`, syncing stops and an error is logged every minute.
  - `Ship` sends queued events every 2 s, or once 100 are waiting, up to 500 per batch. A `409` counts as acknowledged, and a `403` batch is dropped and logged. A reply without `acked_seq` keeps the batch queued.

### Gateway (`internal/`, not a public API)

- `internal/mcp`: the MCP gateway. It serves one remote MCP server per upstream, at `/mcp/{upstream}`, over streamable HTTP. The agent is identified from its Fences token on every request, and may reach only the upstreams assigned to it. `tools/list` hides what isn't possible, and `tools/call` is decided before it's forwarded. A denied call gets an MCP error result carrying the reason. Upstream credentials stay in the gateway's config. Arguments that repeat a key are refused. A reply from an upstream is capped at 16 MB, and a call at 2 minutes.
- `internal/engine`: decisions for every surface. It covers quarantine commands that hold until a newer bundle, limit history reserved before forwarding, approvals held up to `approval_timeout_s` and decided again once granted, `on_unavailable` when the platform can't be asked, and a decision event for every request. Unauthenticated events are capped at 10 a second. It also keeps the metrics for health.
- `internal/approvals`: approval requests, and answers delivered by command.
- `internal/hook`: `POST /hook/{name}` decides a tool call an agent reports before making it itself, with the same limits, approvals, quarantine and events as an MCP call. Nothing is forwarded. The agent must be assigned `{name}` as an upstream. `internal/engine` finds the agent from its token (`Agent`), for every surface.
- `internal/proxy`: the HTTP API gateway. It's a reverse proxy per API at `/http/{api}/{path...}`, configured under `apis`. Each request is decided on its method, path, query and JSON body before it's forwarded, with the same limits (including `sum` caps reserved before forwarding), approvals, quarantine and events as MCP. A body that is a JSON object is decided on whatever its Content-Type says. The agent's token is removed and the API's headers from the config are added. Protocol upgrades (WebSocket) are refused. Denied requests get `403`, rate-limited ones `429`, and an unreachable API `502`. A 5xx counts as an upstream error in health. `docs/http-proxy.md` covers it.
- `internal/k8s`:
  - `POST /k8s/admit` is the validating admission webhook for agents' service accounts.
  - Approvals that can't be held are refused at once and allowed on the retry after the grant. `internal/engine` adds `ServiceAccount` and `approveLater`, and `internal/approvals` adds `Send`.
  - Containment moves a quarantined agent out of its labeled write bindings and back on release, and its result goes in the quarantine's second ack.
  - `POST /k8s/audit` records reads from the API server's audit webhook.
  - `cmd/fences-gateway` takes `tls` and `kubernetes` settings, and the Helm chart's `kubernetes.enabled` sets up the webhook, TLS and RBAC. `docs/kubernetes.md` covers it.
- `cmd/fences-hook`: a Claude Code `PreToolUse` hook. It asks the gateway, and blocks the call with the reason Claude is shown if it isn't allowed. It fails closed. File paths are relative inside the project and absolute outside it. `docs/claude-code.md` covers setup, with an example policy in `docs/examples/claude-code.json`.
- `cmd/fences-gateway`: a config with no upstreams is valid, for a gateway that only answers hooks.
- `cmd/fences-gateway`: the gateway process.
  - It reads a JSON config, which rejects unknown keys, and header values can name environment variables, written `${CRM_TOKEN}`. Only that form is expanded, so a secret can contain a bare `$`.
  - On first start it enrolls with `FENCES_ENROLL_TOKEN`, and `pinned_signing_keys` override the keys from enrollment.
  - It keeps its identity, the last good bundle and the event queue in `data_dir`, runs sync and event shipping, and serves `/mcp/{upstream}`, `/healthz` and `/readyz` (`503` until there's a bundle). It shuts down cleanly on SIGTERM.
- `Dockerfile`: a multi-arch image (amd64 and arm64) on distroless, running as a non-root user, about 15 MB. `release.yml` builds and pushes it to GHCR on a `v*` tag, with the image tagged without the `v`.
- `deploy/helm/fences-gateway`: a chart.
  - It runs a StatefulSet, so the identity, bundle and queue survive restarts on a volume. It has one replica, because each replica is its own enrolled gateway until Phase 7.
  - The enrollment token and upstream secrets come from Secrets.
  - The pod runs on a read-only root filesystem with no capabilities, and `/readyz` is its readiness probe.
  - CI lints the chart.
- `pkg/sync` has an example program for `fences-edge` to copy, compiled with the tests. Every exported name in `pkg/` has a doc comment.

## v0.0.1 (specs only, no gateway code)

- `spec/PROTOCOL.md`: the SDK ↔ server protocol, moved from `fences` with its history, unchanged.
- `spec/PROTOCOL-GATEWAY.md`: first version **(edge)**. It covers enrollment, signed requests, sync, signed bundles, decision events, approvals, the `quarantine` and `approval` commands, and fail mode. A quarantine is released by a new bundle, never by a command.
- `spec/rules/`: the rules spec and its JSON Schema **(edge)**. It covers subjects `mcp`, `http` and `k8s`, conditions, time windows, rate and amount limits, and the decision order. It also covers run limits (checkpoint and guard), as the SDKs and the platform already enforce them.
- `spec/conformance/`: 109 cases. They run against the reference engine (`conformance/reference.py`) in CI. The run-limit cases also pass against `agentfences` 0.9.2's own checks.
