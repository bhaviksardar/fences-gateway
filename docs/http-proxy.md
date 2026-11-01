# HTTP APIs through Fences Gateway

The gateway can sit in front of an HTTP API that an agent calls, such as a refunds or billing API. Every request is decided on its method, path, query and JSON body before it's forwarded. Requests get the same limits, approvals, quarantine and dashboard as MCP calls.

## Config

```json
"apis": {
  "refunds": {"url": "https://refunds.internal/api", "headers": {"X-Api-Key": "${REFUNDS_KEY}"}}
}
```

The agent calls `https://<gateway>/http/refunds/refunds` instead of `https://refunds.internal/api/refunds`, with its Fences key as `Authorization: Bearer <key>`. The gateway removes that header and adds the API's own headers from the config, so the agent never holds the API's credentials. In Fences, assign the agent the upstream `refunds` on the gateway.

## Rules

An API request is an `http` request (see [the rules spec](../spec/rules/README.md#requests)): `server` (the API's name), `method`, `path` (after `/http/<name>`), `query` and `body`. "At most $500 of refunds an hour per agent, and refunds over $100 need approval":

```json
{"default": "deny", "rules": [
  {"id": "refund-cap", "on": "http", "effect": "allow",
   "where": [{"field": "method", "op": "is", "value": "POST"}, {"field": "path", "op": "is", "value": "/refunds"}],
   "limit": {"window_s": 3600, "max": 500, "sum": "body.amount"}, "reason": "$500 of refunds an hour"},
  {"id": "big-refunds", "on": "http", "effect": "approve",
   "where": [{"field": "method", "op": "is", "value": "POST"}, {"field": "path", "op": "is", "value": "/refunds"},
             {"field": "body.amount", "op": "gt", "value": 100}], "reason": "Refunds over $100 need a person"}
]}
```

An amount is reserved when the request is allowed, before it's forwarded, so two requests at once can't both take the last of the cap. A request is refused with `403` when denied and `429` when rate-limited, with `{"error": "Fences: …: <reason>"}`. One that needs approval is held until a person answers in Slack or on the dashboard.

## What the gateway reads

- **Only a JSON object body is read**, whatever its Content-Type says. A form or other body has no `body` fields. A rule that sums `body.amount` counts such a request as over its limit, so a cap can't be bypassed by sending the amount in another format.
- A body that repeats a key, or a query string that repeats a parameter, is refused: the API might read a different copy from the one the policy decided on.
- The path is decoded before it's decided, and forwarded re-encoded, so the API acts on the path that was decided.
- **Prefer `default: deny` with `allow` rules.** The path is compared exactly, but an API may treat `/refunds/`, `/Refunds` or `/refunds;x` as `/refunds`. A `deny` rule on `/refunds` doesn't match those variants, while under `default: deny` they're refused because nothing allows them.
- Protocol upgrades (WebSocket) are refused: requests sent over the upgraded connection couldn't be decided.
- Bodies are at most 4 MB, and a request (response included) at most 2 minutes.
