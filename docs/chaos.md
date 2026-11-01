# Chaos tests

```sh
go test -race -run TestChaos -v ./cmd/fences-gateway/     # CI runs it with every go test
```

`TestChaos` (`cmd/fences-gateway/chaos_test.go`) runs the whole gateway process against a fake platform and a real MCP upstream. Either one can be made unreachable, dropping every connection. The results below are from 2026-10-31.

| Situation | What the gateway does | Result |
|---|---|---|
| Platform unreachable | Keeps deciding from the cached bundle: reads go through. | pass |
| Platform unreachable, call needs approval | Fails closed: "approval needed, and Fences can't be reached". A rule with `on_unavailable: allow` goes ahead, marked degraded. | pass |
| Upstream MCP server down | The call fails with an error result, and health counts an upstream error. | pass |
| Upstream back | The next calls work: the gateway drops the broken session and opens a new one. | pass |
| Restart while a call waits on an approval | The held call is denied at once ("approval wait ended"), and the process stops in about 2 seconds. Before this test, the restart took 10 seconds and returned an error. | pass (fixed) |
| Restart while the platform is unreachable | Serves at once from the identity and last good bundle on disk. | pass |
| Events made while the platform was down, across the restart | Kept in the on-disk queue and shipped when the platform returns. None were lost. | pass |
| The platform answers an approval the old process asked | The new process isn't waiting on it, so the answer is ignored, without an error. The agent asks again. | pass |

Agents' MCP sessions don't survive a restart. An MCP client initializes again, as the protocol expects when its session is gone.
