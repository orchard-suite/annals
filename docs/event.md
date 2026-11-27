# The event and the record

A product posts an **event**: one thing an actor did. Annals keeps it as
a **record**: the event plus Annals' own id and receipt time, never
changed again. `annals.Event` is the one shape every product posts. The JSON
Schema is `schema/event.json`; the Go type is `annals.Event` in
`github.com/orchard-suite/platformkit/v3/annals`; worked examples are
`schema/examples/*.json`. This page is the contract. A product that sends
events implements it and nothing else.

A record answers who did what, to what, with whom, for whom, with what
authority, and how it went. It names people and things. It never holds what
is inside them: not a prompt, a message, a file's body, a diff, or a field's
value.

## Shape

```json
{
  "at": "2026-10-31T14:02:11Z",
  "source": "beacon",
  "event_key": "audit:00000000-0000-0000-0000-000000000002",
  "actor":     { "kind": "agent", "id": "beacon-agent", "name": "Beacon Agent", "on_behalf_of": "user-0001" },
  "agent":     { "runtime": "ion", "model": "claude-fable-5-1", "session_id": "sess_01J9", "harness": "beacon-agent" },
  "authority": { "token_id": "mcp-door", "scope": ["app.write"], "granted_by": "user-0001",
                 "approval": { "required": true, "by": "user-0001", "at": "2026-10-31T14:01:50Z" } },
  "action":    { "verb": "app.record.update",
                 "target": { "system": "beacon", "kind": "record", "id": "tasks:42", "title": "Task 42" },
                 "outcome": "ok", "reason": "" },
  "parties":   [ { "role": "assignee", "kind": "person", "id": "user-0003", "email": "dee@example.com", "name": "Dee Example" } ],
  "items":     [ { "system": "beacon", "kind": "list", "id": "lists:7", "title": "Weekly tasks", "access": "read" } ],
  "change":    { "before_hash": "sha256:1a2b", "after_hash": "sha256:3c4d", "summary": "set status to done" },
  "trace_id":  "4bf92f3577b34da6a3ce929d0e0e4736",
  "cause":     "",
  "corrects":  "",
  "labels":    { "module": "tasks", "door": "mcp" }
}
```

| Field | Who sets it | Meaning |
|---|---|---|
| `id` | Annals | A ULID. A sender that sets it is refused. |
| `at` | sender | When the action happened, RFC 3339. Required. |
| `received_at` | Annals | When Annals stored it. |
| `source` | sender or Annals | The product that sent it, a lowercase slug. A token is bound to one source; an event may omit it or must match it. |
| `event_key` | sender | The sender's own id for the event. The same `source` and `event_key` is stored once; a repeat answers with the first record's `id`. Senders that retry must set it. |
| `actor` | sender | Who acted. `kind` is `agent`, `person`, or `system`. `id` is required and stable (an agent's name, a person's Entra object id, a service's name). `name` and `email` are for people reading the board. `on_behalf_of` is the person id an agent or system acted for; a person acts for nobody. |
| `agent` | sender | Optional. `runtime` is `ion`, `claude-code`, `codex`, `copilot`, or `other`. `model`, `session_id`, and `harness` are free short strings. |
| `authority` | sender | Optional. `token_id` is the id of the credential used, never the credential. `scope` is what it allowed. `granted_by` is who issued it. `approval.required` says a person had to say yes; `by` and `at` say who did and when, and are empty while the yes is pending. |
| `action` | sender | Required. `verb` is `noun.verb`, lowercase, such as `file.write`, `site.share`, `tool.call`. `target` is the thing acted on: the `system` it lives in, its `kind`, its `id` as that system names it, and an optional `title` for people. A target of kind `person` is a person. `outcome` is `ok`, `refused`, or `failed`; the other two need a `reason`. |
| `parties` | sender | Optional, at most 50. Who else was in the action: who a site was shared with, who was added to a group. `role` is a slug such as `recipient`, `member`, or `owner`. `kind` is `person`, `group`, `agent`, `system`, or `anyone` (an open link). `id` is required unless the kind is `anyone`. `email` and `name` say who it is; `external` says they are outside the organization. |
| `items` | sender | Optional, at most 100. Things the action touched besides its target, such as the files an agent read to answer a prompt: `system`, `kind`, `id`, and an optional `title`, `access` (a slug such as `read` or `write`), sensitivity `label`, and `hash`. |
| `change` | sender | Optional. Hashes of the thing before and after, and a short summary. Never the content. |
| `trace_id` | sender | The W3C trace id the action ran in, so a record joins a trace in Tempo. |
| `cause` | sender | The `event_key` of the record that set this one off, such as the prompt behind a tool call. The board shows each record's cause and what it led to. |
| `cause_source` | sender | Optional, only with `cause`. The `source` of the record `cause` names, when another sender posted it. Empty means this event's own source. |
| `corrects` | sender | The `id` of an earlier record this one corrects. |
| `effects` | sender | Optional, at most 1000. What a change did, or was predicted to do, to each live thing, by kind. See [Effects](#effects). |
| `labels` | sender | At most 16 low-cardinality string pairs, keys lowercase slugs, values under 64 characters. Never an id, a person, or a timestamp. |

Short strings (`id`, `name`, `email`, keys, hashes) are at most 256
characters; titles, `reason`, and `summary` at most 512. No field holds a
newline.

## People

A person appears only in a person field:

- `actor.id`, `actor.name`, and `actor.email` when `actor.kind` is `person`,
  and `actor.email` on any actor;
- `actor.on_behalf_of`, `authority.granted_by`, and `authority.approval.by`;
- every party of kind `person`;
- `action.target.id` and `title` when `action.target.kind` is `person`.

A person's `id` is their **Entra object id**, the id their directory gives
them, which never changes. Their email goes in `email`, never in `id`; an
email changes when a person is renamed. Annals' people directory
matches older ids, such as an email or a product's own user id, to the
person, so one person is one actor across every product.

Annals shows those fields by the reader's role. A **reader** sees each
person as a ref, a stable stand-in such as `p-3f9a1c2b7e4d`, with names and
emails removed, and can follow one ref across the board but cannot find a
person by id or email. A **PII reader** and an **admin** see the person. An
API token reads as `reader` or `pii`, chosen when it is issued.

Free text (titles, `summary`, `reason`, label values) never holds an email
address, so a person cannot slip past the roles there; one that does is
refused with `email_in_text`.

## Rules

1. **Never content, never a secret.** A record names who did what, to which
   thing, with whom: names, emails, site and file names, tool names, ids,
   hashes. What is inside stays in the product: the prompt, the message, the
   file, the diff, the field's value. A token, a key, a password, or a
   connection string never appears, not even partly.
2. **People only in person fields.** See above.
3. **Records are append-only.** Once stored, a record never changes and is
   never deleted by a sender. Annals deletes only by age, per source,
   under its retention setting.
4. **A correction is a new record.** To amend a stored record, post a new
   event with `corrects` set to the old `id`. Both stay; the board shows them
   together.
5. **One `source` per token.** Annals issues a token for a source. An
   event that claims another source is refused with `source_mismatch`.
6. **Retries are safe when `event_key` is set.** Use the product's own row
   id, job step, or tool call id. Without a key every post is a new record.
7. **A cause is a key, not an id.** Set `cause` to the `event_key` the cause
   was posted with, so a sender can post both in one batch, or out of order.
   A cause from another sender names that sender in `cause_source`. An
   event never causes itself: the same source and key is refused with
   `bad_cause`.

## Effects

An effect is what a change does to one live thing, never the tool's verb
and never a value. A record of a stated change carries its predicted
effects; a record of what a watch saw carries each sighting with a
verdict.

| Field | Meaning |
|---|---|
| `kind` | Required. The effect kind, a slug such as `stops`, `cuts`, `opens`, `grants`, `loses`, or `costs`, or a kind the estate names. |
| `subject` | Required. The catalog entity that feels it: its `blueprint` (its class, a slug) and its `key` (short, never an email). |
| `path` | Required. How the change reached the thing, a slug such as `in-place`, `swap`, or `rolling`. |
| `size` | Optional. How much, in the kind's own unit, a whole number never below zero. |
| `undo` | Optional. Whether the old artifact puts it back: `yes`, `no`, or `partial`. |
| `claim` | Optional. Where a predicted effect came from: `writer`, `estate`, or `model`. |
| `verdict` | Optional. What the watch saw: `clean` (it looked, and nothing happened), `harm` (the thing was hurt), or `blind` (it could not look; never the same as clean). |

## Change events

A change to a live thing is told as five change events, each with a
`sender`, its own `key`, a time `at`, the statement's `digest`, and, after
the first, `of`: the `change.stated` event it belongs to, by sender and
key. Their contract is the `change` package of the platform kit. A sender
posts each to Annals as one event, mapped like this:

| Event field | From the change event |
|---|---|
| `source` | `sender` |
| `event_key` | `key` |
| `at` | `at` |
| `actor` | `change.stated`: the statement's actor (`kind`, `id`, `on_behalf_of`). `change.decided`: `by`. `change.routed` and `change.ended`: `{kind: system, id: <sender>}`. `change.observed`: `{kind: system, id: <the watch's name>}`. |
| `action.verb` | The event's name: `change.stated`, `change.routed`, `change.decided`, `change.ended`, or `change.observed`. |
| `action.target` | `{system: orrery, kind: change, id: <the key of the change.stated event>}`, the same on all five. |
| `action.outcome` | `ok`, except `change.ended`, which takes its own outcome (`ok`, `failed`, or `refused`) with a `reason` when it is not `ok`, such as which statement fields differed. |
| `cause`, `cause_source` | `of.key` and `of.sender`, on every event but `change.stated`. |
| `items` | `change.stated` only: each of the statement's things as `{system: catalog, kind: <blueprint>, id: <key>}`. |
| `effects` | `change.stated`: the statement's effects, each with `kind`, `subject`, `path`, `size`, `undo`, and `claim` (its `from`), and no verdict. `change.observed`: its sights, each with `kind`, `subject`, `path`, and `verdict`. |
| `change.after_hash` | The statement's `digest`, on every event. |
| `labels` | `change.stated`: `applier`, and `writer` (the writer's name). `change.routed`: `route` (`no_one` or `people`), `quorum` when it goes to people, `judge` and `pack` (the judge's name and pack) when a judge answered, and `draft.<pack>` set to each draft's route. `change.decided`: `answer` (`yes` or `no`). `change.ended`: `check` (`matched`, `differed`, or `unchecked`). |

A label key is a lowercase slug of at most 64 characters, so a draft's
pack name is one too. Nothing else of a change event is posted: no
grants, reasons, signals, or incidents. Worked examples are
`schema/examples/change-*.json`.

### Proof

Annals counts what the watch saw as it stores each `change.observed`
record, by effect kind, the subject's blueprint, and the path. A change
counts once for each key it was seen under, by the worst of its
sightings: `harm`, then `blind`, then `clean`. A read of one key reads
those counts, never the records, and retention never prunes them.

## Posting

`POST /v1/events` on the API port with `Authorization: Bearer <token>` and
`Content-Type: application/json`. The body is one event or an array of up
to 1000. Unknown fields are refused.

| Answer | Meaning |
|---|---|
| `201` `{"status":"stored","id":...}` | One event, stored. `Location` names it. |
| `200` `{"status":"duplicate","id":...,"duplicate":true}` | One event, already stored under that key. |
| `422` `{"status":"refused","reason":...,"error":...}` | One event, refused. `reason` is a stable slug from the list below. |
| `200` `{"results":[{"index":0,"status":...},...]}` | A batch: one receipt per event, in order. Valid events are stored even when others are refused. |
| `400` `reason: bad_json` | Not JSON, or an unknown field. |
| `401` | No token, or a revoked one. |

Refusal reasons: `source_mismatch`, `missing_at`, `id_set`, `bad_corrects`,
`bad_cause`, `bad_cause_source`, `bad_actor_kind`, `missing_actor_id`, `bad_on_behalf_of`,
`bad_runtime`, `bad_outcome`, `missing_reason`, `bad_slug`,
`missing_<field>`, `too_long`, `bad_text`, `bad_email`, `email_in_text`,
`bad_party_kind`, `too_many_parties`, `too_many_items`, `too_many_effects`,
`bad_size`, `bad_undo`, `bad_claim`, `bad_verdict`, `bad_label`,
`too_many_labels`, `too_many_scopes`, `bad_approval`, `bad_json`. Each counts
in `annals.events.refused`.

## Reading

`GET /v1/events` takes `since`, `until` (RFC 3339 or a span back from now
such as `24h` or `7d`), `actor`, `party` (an id, email, or ref), `source`,
`system`, `outcome`, `verb`, `kind`, `target`, `cause` and `cause_source`
(together, the records an event caused, from every sender; a record whose
cause names no source has its own), `approval=required`, `limit` (50, at most 500), and
`cursor` from the previous page's `next`. `GET /v1/events/{id}` returns one.
`GET /v1/stats` is Annals at a glance. `GET /v1/proof?kind=&blueprint=&path=`
answers for one key, from the counts above: `{kind, blueprint, path,
watched, clean, harm, blind, last_seen}`, with `last_seen` left out when
nothing was watched. Add `source` to count only what one sender saw. Every read answers at the
token's level. The same filters are the MCP tool `annals_search` at `/mcp`.
