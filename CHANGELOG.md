# Changelog

Changes to `spec/` and `pkg/` are listed here, because other repos pin them: `fences-edge` imports `pkg/` and follows `spec/`, and `fences-platform` checks against `spec/`. Entries that affect `fences-edge` are marked **(edge)**.

## Unreleased

- `pkg/policy`: the rules engine in Go **(edge)**. `Parse` rejects what `spec/rules/schema.json` rejects (apart from repeats in `quarantined` and `days`, which change nothing), plus duplicate ids, and data after the policy. `Decide` follows `spec/rules/README.md`, Deciding. It passes every policy case in `spec/conformance/`, which `conformance/conformance_test.go` checks in CI. Run limits aren't in it: the gateway doesn't enforce them.
- `pkg/bundle` **(edge)**. `Verify` checks a bundle's signature over the exact payload bytes, its key, its `gateway_id` and its policy. `Store` keeps the current bundle, refuses any version that isn't newer, and keeps the last good bundle on disk, written atomically with mode `0600`. A newer bundle takes effect even if saving it fails, since it may carry a quarantine. Unknown payload fields are ignored, so the platform can add some.

## v0.0.1 (specs only, no gateway code)

- `spec/PROTOCOL.md`: the SDK ↔ server protocol, moved from `fences` with its history, unchanged.
- `spec/PROTOCOL-GATEWAY.md`: first version **(edge)**. It covers enrollment, signed requests, sync, signed bundles, decision events, approvals, the `quarantine` and `approval` commands, and fail mode. A quarantine is released by a new bundle, never by a command.
- `spec/rules/`: the rules spec and its JSON Schema **(edge)**. It covers subjects `mcp`, `http` and `k8s`, conditions, time windows, rate and amount limits, and the decision order. It also covers run limits (checkpoint and guard), as the SDKs and the platform already enforce them.
- `spec/conformance/`: 109 cases. They run against the reference engine (`conformance/reference.py`) in CI. The run-limit cases also pass against `agentfences` 0.9.2's own checks.
