# Annals

Annals: one record of what every actor did across every product.
People, agents, and systems: who acted, for whom, with what authority.

Today each product audits itself. Beacon keeps an audit log, Graft
records jobs and approvals, Orrery and Vantage keep their own histories, and
Ion runs the agents. Nothing joins them. Annals is the join: an ingest
API every product posts to, one event shape, storage, a board, a query API,
and an MCP server so an agent can ask what happened.

A product posts an event; Annals keeps it as a record. A record says
who did what, to what, with whom, for whom, and with what authority:
who shared a site with which guest's email, which tools a Copilot prompt
set off and which files they touched. It never holds content: not the
prompt, the message, or the file.

## The event

One shape, `annals.Event`: who acted (`actor`), as what agent (`agent`),
with what authority and approval (`authority`), did what to what and how it
went (`action`), with whom (`parties`), touching what else (`items`), set
off by which earlier record, from any sender (`cause`, `cause_source`),
what a change did to each live thing by kind (`effects`), the rest of a stated change's
statement (`statement`: its key, writer, every thing, artifact, and unknown parts), what changed by hash (`change`), in
which trace (`trace_id`), sent by which product (`source`). Names and ids,
never content or a secret; people only in person fields; records are
append-only and a correction is a new record. The contract is
[docs/event.md](docs/event.md), the schema `schema/event.json`, the
examples `schema/examples/`.

## Roles

| Role | Front-door role | Sees |
|---|---|---|
| Reader | `Reader` (Entra `annals.Reader`) | Every record, with each person as a ref such as `p-3f9a1c2b7e4d` and names and emails removed. Finds people only by ref. |
| PII reader | `PiiReader` (Entra `annals.PiiReader`) | People too: ids, names, emails. Keep this list short; HR approves it. Every page shown to one is logged as `people shown` and kept as a `people.view` record. |
| Admin | `Admin` (Entra `annals.Admin`) | Everything, issues and revokes tokens, and ties an id to a person. |

A token reads as `reader` or `pii`, chosen when an admin issues it.

## People

Products should post a person by their Entra object id, with their email
in `email`; see [docs/event.md](docs/event.md). Older records, and
products not yet moved, name people by email or by their own user id.

Annals' **people directory** matches every id to one person, so a
person posted under three ids is one actor on the board, one bucket in a
breakdown, and one ref to a reader. Records keep the ids they were posted
with; a read asks the directory, so a match made today also covers last
year's records.

- **The tenant's users.** When the pod signs in to Entra (workload
  identity), the server reads the tenant's users from Graph every
  `DIRECTORY_INTERVAL` with the user delta query: each user's object id,
  display name, mail, sign-in name, and every email alias. An address a
  person drops stays theirs, so their older records still match; a user
  Entra deletes stays and is marked left. Its app needs Graph's
  `User.Read.All` application permission.
- **A record's own email.** After each directory pass, an id a product
  posted a person under that the directory does not know is tied to the
  person whose email the same record carries, such as a SharePoint
  login. The tie covers that id's older records too.
- **An admin's tie.** On an actor page the directory does not know, an
  admin ties the id to the person it belongs to, named by any email or id
  of theirs, and can untie it later. Each is a `people.link` or
  `people.unlink` record.

A PII reader sees the directory's name and email where a record has none,
and an actor page lists every id of the person.

## Ports

| Port | Who | Trusts |
|---|---|---|
| 8081, the API | products, agents, MCP clients | a bearer token Annals issued; `/healthz` open |
| 8080, the board | people, through the estate's front door | `X-Forwarded-Email` and `X-Forwarded-Groups` |

API: `POST /v1/events` (one or a batch, idempotent by `source` plus
`event_key`), `GET /v1/events` with `since`, `until`, `actor`, `source`,
`system`, `outcome`, `verb`, `party`, `cause`, `cause_source`, paged by cursor, `GET /v1/events/{id}`,
`GET /v1/stats`, `GET /v1/proof` (what the watch saw for one effect kind,
blueprint, and path), `GET /v1/gate/families` and `GET /v1/gate/drafts`
(how the change gate scored, as tables; see [The gate's score](#the-gates-score)), `GET /v1/whoami`, and `/mcp` (streamable HTTP) with
`annals_search`, `annals_actor_summary`, `annals_target_history`.

Board: Today, a window (24 hours, 7 days, 30 days, or all time) read as
one sentence, a pulse of records per step, and the records split in two.
Anything an actor did 5 times or more, without a failure and not as a
person, folds into one routine line with a count and its shape. The rest is
worth a look, grouped by why: failed or refused, three times busier than
the window before, new since it, done by a person, or rare. Records is
every record by day, with filters. A record's page says who, what, where,
and with what authority, then with whom, what it touched, what set it off,
and what it led to. An actor's page and a target's page read the same way,
scoped to them. Then the Approvals view, and Tokens (issue for a source at
a read level, see the secret once, revoke). Light and dark, phone-first,
with tabs along the bottom on a phone. Times are in the server's `TZ`. Browser telemetry
through `POST /api/telemetry` as `annals-web`.

## The gate's score

The change gate learns from what changes did, never from a person's yes.
Annals scores it from the change events it keeps, one row a week (Monday
to Monday, UTC; the newest runs through now), for the weeks asked
(`weeks`, 12 unless set, at most 52).

`GET /v1/gate/families` is one row a week and effect family, for every
change the judge routed with an effect of that family:

| Column | What it counts |
|---|---|
| `changes`, `asks`, `ran` | The changes, those routed to people, and those applied. |
| `needless` | Asked of people, ran, and the watch saw the family clean. |
| `misses` | Ran and did harm, and went to no one or had an incident joined to it. |
| `unknowns` | Ran, and the watch could not see the family: blind, or never watched. |
| `coverage` | The percent of the changes that ran that the watch saw. |
| `wait_median`, `wait_hours`, `waited` | From each ask to its last answer: the median minutes, the sum in hours, and how many asks were answered. |
| `score` | `scored`; `too blind`, when more than `GATE_MAX_BLIND` percent of what ran was unknown; or `nothing ran`. A family not scored shows no needless asks and no misses. |

An incident joined to a change is harm for every family the change has.
A kind the watch cannot see, such as a grant, leaves its family unknown,
because blind is never clean.

`GET /v1/gate/drafts` is one row a week and draft gate pack, from the
route each draft gave beside the live one (`draft.<pack>` on
`change.routed`): the changes it `judged`, the `asks` among them, how many
of those it would have sent to no one (`to_no_one`), how many of those did
harm (`harmed`), how many ran with no clear watch (`unseen`), and the
`coverage`.

Both answer a table, `{columns, rows}`: each column its `id`, `title`, and
`unit` and `decimals` where it has them, and each row its values in that
order, null where a value cannot be said. They answer counts, never a
record or a person, to any token.

## Annals' own records

Annals records what it does itself, as ordinary records under the
source `annals`. They pass the same validator as any posted event, and a
person in them is masked by the caller's level like any other.

| Verb | Actor | Target | When |
|---|---|---|---|
| `token.issue` | the admin | the token (kind `token`, its id, never the secret); labels `source` and `reads` | An admin issues a token. |
| `token.revoke` | the admin | the token, as above | An admin revokes a live token. |
| `people.view` | the person, or the token (system, with `authority.token_id`) | `annals` `view` `people`; labels `level` and `door` (`board`, `api`, `mcp`) | A board page, an API read, or an MCP tool shows people. At most one per caller, level, and UTC day. |
| `people.link` | the admin | the person (kind `person`, their directory id); the id tied to them is a party with role `alias` | An admin ties an id to a person. |
| `people.unlink` | the admin | the person, as above | An admin unties an id. |
| `retention.prune` | the system `annals` | the source (kind `source`) | A retention pass deletes records of that source; `change.summary` says how many and older than when. A pass that deletes nothing records nothing. |

Writing one of these never holds up or fails the action it records. A
write that fails is logged and counted in `annals.self.records`.

## Collectors

Products post their own decisions. A collector reads a system that keeps
its own audit log and posts it the same way. `annals-m365`, in the same
image, reads the Microsoft 365 unified audit log for sharing and Copilot:
see [docs/adapters/m365.md](docs/adapters/m365.md). A collector that
finds access changes in a system's audit log posts them as `access.grant`
and `access.revoke`: see [docs/adapters/access.md](docs/adapters/access.md).

## Run it

```sh
make run   # starts a local ClickHouse in Docker the first time
# the board, as the admin:
curl -H 'X-Forwarded-Email: you@example.com' -H 'X-Forwarded-Groups: Admin' localhost:8080/tokens
```

Issue a token on `/tokens`, then post `schema/examples/curl.json` with it to
`localhost:8081/v1/events`. [docs/adapters/](docs/adapters/README.md) shows
how Graft, Ion, Microsoft 365, and a shell post.

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `PUBLIC_URL` | required | The board's URL; `Origin` on changes must match. |
| `CLICKHOUSE_URL` | required | The ClickHouse, such as `clickhouse://annals@clickhouse:9000/annals`. The database and tables are made at start. |
| `CLICKHOUSE_PASSWORD` | | The ClickHouse user's password, kept out of the URL. |
| `SESSION_ADDR`, `API_ADDR` | `:8080`, `:8081` | |
| `READER_ROLE`, `PII_ROLE`, `ADMIN_ROLE` | `Reader`, `PiiReader`, `Admin` | Front-door roles, as the door sends them: the Entra role `annals.Reader` arrives as `Reader`. Each implies the ones before it. A role with the `annals.` prefix stops the server at start. |
| `RETENTION_DEFAULT` | `365d` | Records older than this are deleted, hourly. `0` keeps forever. |
| `RETENTION_SOURCES` | | Per source, `beacon=90d,ion=30d`. |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME` | | OTLP traces and metrics. Off when unset. |
| `FARO_URL`, `FARO_API_KEY` | | Relay of browser telemetry. Without them batches are accepted and dropped. |
| `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_FEDERATED_TOKEN_FILE`, `AZURE_AUTHORITY_HOST` | | The Entra app the people directory reads users as, by workload identity. The directory sync is off unless the first three are set. |
| `DIRECTORY_INTERVAL` | `30m` | How often the directory reads the users who changed. |
| `GRAPH_URL` | `https://graph.microsoft.com/v1.0` | |
| `GATE_KINDS` | | A file of the estate's own effect kinds, in the platform kit's shape (`{"kinds": [...]}`), so the gate's score counts each in its family. The suite's base kinds are always known. |
| `GATE_MAX_BLIND` | `20` | The percent of a family's changes that ran that its watch may fail to see, and the family still be scored. |
| `TZ` | `UTC` | The board's time zone, such as `America/Chicago`. Days, hours, and times on the board are in it. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error`. `debug` logs admitted requests too. |

## Orrery

`orrery.yaml` at the repository root says what this repository builds: the
service `github/orchard-suite/annals/cmd/annalsd`, the pillars it covers, the APIs
it provides, and its logo, `logo.svg`, as the custom icon `annals`. The app
never sends it anywhere. Muster, the forge reader, reads it from the
repository and writes the service and its APIs into each estate that reads
this repository.

## Telemetry

JSON logs with `trace_id`; spans per request, per retention pass, and per
directory pass;
`http.server.request.duration`; `annals.build_info`;
`annals.events.ingested` by `source` and `outcome`; `annals.events.refused`
by `reason`; `annals.events.pruned`; `annals.self.records`, Annals' own records, by
`verb` and `outcome` (`stored`, `duplicate`, `refused`, `failed`); `annals.events`, `annals.tokens`,
`annals.db.size` gauges; `annals.directory.passes` by outcome and the
`annals.directory.people` gauge; `annals.browser.batches` by outcome (`relayed`,
`dropped`, `failed`, `refused`); and
`process.cpu.time`, `process.memory.usage`.

## Deploy

The image is `ghcr.io/orchard-suite/annals:<version>` for `linux/amd64`,
signed with cosign keyless and released from `main` by
`.github/workflows/release.yml`. It holds `/app/annalsd`, the entrypoint,
and `/app/annals-m365`, the collector, run as its own deployment. An
estate keeps its own manifests, alerts, and dashboards for it.

## License

MIT.
