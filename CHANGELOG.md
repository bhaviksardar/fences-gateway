# Changelog

Changes to `spec/` and `pkg/` are listed here, because other repos pin them: `fences-edge` imports `pkg/` and follows `spec/`, and `fences-platform` checks against `spec/`. Entries that affect `fences-edge` are marked **(edge)**.

## Unreleased

- `spec/PROTOCOL-GATEWAY.md` **(edge)**: argument names are clipped to 300 characters, as values are. An `acked_seq` above the highest `seq` sent counts as that `seq`.
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

## v0.0.1 (specs only, no gateway code)

- `spec/PROTOCOL.md`: the SDK ↔ server protocol, moved from `fences` with its history, unchanged.
- `spec/PROTOCOL-GATEWAY.md`: first version **(edge)**. It covers enrollment, signed requests, sync, signed bundles, decision events, approvals, the `quarantine` and `approval` commands, and fail mode. A quarantine is released by a new bundle, never by a command.
- `spec/rules/`: the rules spec and its JSON Schema **(edge)**. It covers subjects `mcp`, `http` and `k8s`, conditions, time windows, rate and amount limits, and the decision order. It also covers run limits (checkpoint and guard), as the SDKs and the platform already enforce them.
- `spec/conformance/`: 109 cases. They run against the reference engine (`conformance/reference.py`) in CI. The run-limit cases also pass against `agentfences` 0.9.2's own checks.
