# Annals

This repository is public and estate-neutral. Every name in a test, an
example, or a doc comes from the fixture world:
https://github.com/orchard-suite/hedge/blob/main/docs/fixtures.md.

Annals: one record of what every actor (person, agent, or system)
did across every product. Products post events to it; people read the board; agents ask it
over MCP. Read `README.md` and `docs/event.md` first.

## Shape

- `cmd/annalsd` is the server. One Go module, one static binary, no cgo.
- `docs/event.md` is the event contract in words and `schema/event.json`
  is it as JSON Schema. Its Go form is the `annals` package of
  `github.com/orchard-suite/platformkit/v3`, which this server validates with and
  products post with. The three change together;
  `internal/contract/contract_test.go` checks the schema and the Go
  validator agree on every example in `schema/examples/`.
- `internal/store` is ClickHouse (`clickhouse-go`, native protocol):
  records as append-only rows in `records` with the full JSON beside the
  indexed columns, sender tokens as hashes in `tokens`, and the key that
  makes people refs in `settings`. ClickHouse has no unique keys, so the
  store keeps "one record per source and event_key" itself: it is the only
  writer, claims each key in memory while writing it, and looks it up
  first. Tests run on a real ClickHouse that `make check` starts in Docker
  (`internal/store/storetest`).
- `internal/people` decides what a caller sees of people: a reader sees
  each person as a ref, a PII reader and an admin see the person. Every
  read on both ports goes through it, and a reader's filters match refs
  only. Its `Directory` matches every id a person was posted under to
  one person (their Entra object id), at read time: records keep the ids
  they were posted with, the `*_ref` columns hold each id's own filed
  ref, and a read turns them into the person's shown ref. The store keeps
  the directory in `people` and `identities`.
- `internal/directory` fills the directory from the tenant's Entra users
  (Graph's user delta query) when the pod signs in by workload identity.
  `internal/graph` is the Graph client it and the collector share.
- `internal/server` answers on two ports, one per kind of caller:
  - `API` (8081): `/v1/events`, `/v1/stats`, `/mcp`, `/healthz`. Trusts
    only a bearer token Annals issued; `/healthz` is open.
  - `Session` (8080): the board and token administration. Trusts the front
    door's `X-Forwarded-Email` and `X-Forwarded-Groups`; only Traefik may
    reach it. The kit's `door` gates each route. The door strips the
    `annals.` prefix from the Entra roles, so `Reader` opens the board,
    `PiiReader` also shows people, and `Admin` adds the tokens; a prefixed
    role stops the server at start.
- `orrery.yaml` says what this repository builds: the service, its APIs,
  and its logo, `logo.svg`. redgreen reads it from the repository and
  writes it to Orrery; the app never describes itself.
- `openapi.yaml` is the API port's REST routes as OpenAPI 3.1, named by
  `spec` in `orrery.yaml`. A route added to or removed from
  `internal/server/apiport.go` changes it in the same commit;
  `internal/server/openapi_test.go` fails while the two differ.
- `internal/annalsmcp` is the MCP server (official Go SDK, streamable HTTP,
  stateless) with `annals_search`, `annals_actor_summary`, and
  `annals_target_history`.
- `internal/retention` deletes records by age per source, hourly.
- `internal/selfaudit` writes Annals' own records under source
  `annals` (`token.issue`, `token.revoke`, `people.view`,
  `people.link`, `people.unlink`, `retention.prune`), validated like
  any event and put straight into the
  store. A write never blocks or fails the action it records: it runs on
  its own goroutine, and a failure is logged and counted in
  `annals.self.records`. A new self record goes through it, and the
  README's "Annals' own records" lists it.
- `cmd/annals-m365` and `internal/m365` are the Microsoft 365 collector,
  in the same image: Graph's audit log query API, signed in with a
  federated workload token, mapped to events and posted in batches. It is
  a sender like any product; it holds an Annals token and nothing more.
  `docs/adapters/m365.md` is its contract.
- The kit's `telemetry` sets up JSON logs, OTLP, `annals.build_info`, and
  the process metrics, and its `faro` relays the board's browser
  telemetry. `internal/metrics` is Annals' own metrics, the ones an
  estate's dashboards and alerts read. Their names are a contract: rename
  one only as a breaking change, and say so in the commit.
- `docs/adapters/` is how each product posts.

## Rules

- A record names who did what, to what, with whom; never content or a
  secret. People go only in person fields, which every read masks by the
  caller's level; never add a read path that skips `internal/people`. Keep
  the validator strict: a new field is a contract change and lands in all
  three forms of the contract plus an example.
- Records are append-only. The only delete is retention. A correction is a
  new record with `corrects`.
- Never let a header set identity on the API port, never accept a bearer
  token on the session port, and never add a route that skips either check.
- Every change on the session port checks `Origin` against `PUBLIC_URL`.
- A token's secret is shown once and only its hash is stored. A token posts
  as one `source` and reads at one level, `reader` or `pii`. Tokens never
  administer.
- Logs are JSON lines on stdout with `trace_id`. Log both branches of every
  decision about who is let in and every refused event with its reason.
- Metric attributes are low-cardinality: a source, an outcome, a reason.
  Never an actor, a target, or a key.
- No continuously repainting CSS animations. The board is phone-first and
  works in light and dark.
- No em dashes or en dashes in prose. No real names or emails in fixtures
  or docs.

## Delivery

Agents push to main.

Commit on `main` with conventional commits and land with `make land`
(`make land BRANCH=<branch>` from a worktree). It runs `make check`, then
`release-damnit land`. CI runs the same check (`.github/workflows/test.yml`)
on every pull request and before every release, then versions every `feat`
and `fix`, tags it, and builds and signs
`ghcr.io/orchard-suite/annals:<version>` for `linux/amd64`. Never edit
`VERSION`, the manifest, or `CHANGELOG.md` by hand.

## Checks

```bash
make check
```

Run it before every commit; `make land` runs it again before it pushes. It
needs Docker and Go (see `go.mod`).
