# Running replicas

Run more than one gateway behind one address for availability. Each replica is its own gateway, with its own identity, event queue and bundle. Together they form a **group** that serves the same agents and counts limits together.

1. **In Fences**, in Settings → Gateways, set **Replicas** to the number you'll run, then **Add gateway**. The token enrolls that many gateways as one group. Assign agents to any replica in the group: every replica serves them, including one that enrolls later.
2. **A shared store for limits.** Each replica counts limits on its own unless the group shares its counts, so N replicas would allow N times each limit. Run Redis or Valkey (a single instance or a primary/replica pair, not Redis Cluster), and give every replica the same `shared_limits`:

   ```json
   "shared_limits": {"url": "${FENCES_SHARED_LIMITS_URL}"}
   ```

   `url` is `redis://[user:password@]host:port[/db]`, or `rediss://` for TLS.
3. **Helm**: `--set replicas=3 --set sharedLimits.urlSecret.name=<a Secret with key url>`, with the token in `enrollToken.secretName`. The chart won't render more than one replica without shared limits. Each pod enrolls under its own name (`${HOSTNAME}`) and keeps its identity on its own volume.

## How counting works

Before deciding, a replica atomically adds the request to every count it falls under, in one Lua script, and gets the counts as they were before. It decides with those counts, and takes the request back out if it isn't allowed. So two replicas racing for the last slot can both refuse, but they can't both allow it. A count's entries expire with its window. Times come from the replicas' clocks, so keep them in sync (NTP).

Each limited decision adds one round trip to the store, which is typically well under a millisecond in the same network. A decision with no limit doesn't touch the store.

**If the store is unreachable**, each replica counts on its own and marks those decisions `degraded: true`. Their events show it, and health reports `shared_limits` with the error, which the dashboard shows on the gateway. When the store is back, counting is shared again, but what each replica counted alone in the meantime isn't in the shared count.

## Limits

- **Kubernetes approvals** go back to the replica that asked, and the API server sends the retry to any replica. A gateway used as an admission webhook should run one replica until approvals are shared too.
- **Redis Cluster** isn't supported: one request's counts can fall in different slots.

## Checked

- `internal/engine` `TestSharedLimits`: two replicas on one Redis decide 5,000 random requests, each sent to either replica, exactly as one gateway would. Then 60 requests race across both for a limit of 3, and at most 3 get through. CI runs it against a Redis service.
- `fences-platform` `tests/e2e_replicas.py`: two real gateway processes enrolled with one token. A $500-an-hour cap holds across both, with requests sent to them alternately.
