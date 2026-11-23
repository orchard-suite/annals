# curl

Anything that can run a shell can post. Issue a token on the board's Tokens
page for the source the script posts as, then:

```sh
ANNALS_URL=https://annals.example.com
ANNALS_TOKEN=an_...   # from the Tokens page, shown once

curl -sS "$ANNALS_URL/v1/events" \
  -H "Authorization: Bearer $ANNALS_TOKEN" \
  -H "Content-Type: application/json" \
  -d @- <<EOF2
{
  "at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "event_key": "deploy-$(date -u +%Y%m%d)-1",
  "actor": { "kind": "person", "id": "user-0001", "name": "Operator" },
  "action": {
    "verb": "workload.restart",
    "target": { "system": "kubernetes", "kind": "deployment", "id": "orrery/orrery-hub", "title": "Orrery hub" },
    "outcome": "failed",
    "reason": "rollout did not become ready in 5 minutes"
  },
  "labels": { "cluster": "cluster-a" }
}
EOF2
```

`source` may be left out; Annals fills in the token's. The answer is
`201` with the event's `id`, `200` with `duplicate: true` when the same
`event_key` was posted before, or `422` with a `reason`.

Read it back:

```sh
curl -sS "$ANNALS_URL/v1/events?since=24h&outcome=failed" -H "Authorization: Bearer $ANNALS_TOKEN"
curl -sS "$ANNALS_URL/v1/stats" -H "Authorization: Bearer $ANNALS_TOKEN"
```

Ask it from an MCP client: the endpoint is `$ANNALS_URL/mcp`, streamable
HTTP, with the same bearer token; the tools are `annals_search`,
`annals_actor_summary`, and `annals_target_history`.
