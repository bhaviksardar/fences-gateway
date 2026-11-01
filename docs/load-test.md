# Load test

The roadmap's target: **1,000 decisions a second per replica, with p99 added latency under 5 ms for decisions made locally.**

```sh
go test -run TestLoad -v ./internal/proxy/                    # 3 seconds; CI runs this
FENCES_LOAD_S=60 go test -run TestLoad -v -timeout 10m ./internal/proxy/
```

`TestLoad` (`internal/proxy/load_test.go`) sends requests open loop at 1,000 a second: one every millisecond, whether or not earlier ones have answered. They go through the HTTP proxy to a local API, as one agent. The policy has a deny rule and an allow rule with an hour-long `sum` limit, so every request is counted, and the count grows the whole run. It fails if:
- any request fails;
- the engine's p99 added latency is 5 ms or more;
- on a run with `FENCES_LOAD_S` set, the client's p99 through the gateway is 5 ms or more above its p99 going direct. On CI's short run, which shares a small machine, that comparison is only logged.

## Results

Apple M3 Pro (12 cores), Go 1.27, 2026-10-31, 60 seconds:

| | p50 | p99 |
|---|---|---|
| Direct to the API | 0.20 ms | 0.56 ms |
| Through the gateway | 0.47 ms | 1.54 ms |
| Engine added latency (policy, limits, bookkeeping) | | **0.024 ms** |

There were 60,000 requests at 996 a second, none failed, and every decision was queued as an event.

**What the test found.** Before this run, the engine kept every allowed request and rescanned them all for each limited decision, under the engine's lock. With an hour-long limit at 1,000 requests a second, latency grew with the history until requests queued behind the lock. About 30 seconds in, p99 reached minutes and 27,000 of 60,000 requests failed. The engine now keeps a running count and total per limit and key: 16 bytes per counted request, dropped as they leave the window. `internal/engine/engine_test.go` checks it decides random traffic exactly as `pkg/policy` does from the full history.

## Not measured here

- MCP adds the MCP SDK's JSON-RPC handling per call, on top of the same engine path.
- Approvals wait for a person, so they're excluded from added latency by design.
- Several replicas sharing rate windows (Phase 7) would add a round trip to the shared store per limited decision.
