# Access changes

When a system keeps its own audit log, its collector posts each access
change it finds there as one event, so Annals holds every grant and
removal between reviews. The verbs are the same for every system.

| Verb | When |
|---|---|
| `access.grant` | Someone or some group was given access to an application or a group. |
| `access.revoke` | Someone or some group lost it. |
| `access.review` | A person signed off on who holds access to an application. |

## Grant and revoke

- `action.target` is what the access opens: the application or the group,
  as the system names it, such as kind `application` or `group`.
- `parties` holds whoever was given or lost the access, with role
  `grantee`: a `person`, or a `group` when the access went through one.
- `labels.role` is the role granted or removed, as the system names it,
  such as `Admin` or `Reader`.
- `actor` is who made the change, and `event_key` is the audit record's
  own id, so reading the same window again stores nothing twice.

```json
{
  "at": "2026-11-07T09:14:02Z",
  "source": "entra",
  "event_key": "audit:00000000-0000-0000-0000-000000000003",
  "actor":   { "kind": "person", "id": "user-0001", "email": "admin@example.com", "name": "Directory Admin" },
  "action":  { "verb": "access.grant",
               "target": { "system": "entra", "kind": "application", "id": "app-0001", "title": "Expense app" },
               "outcome": "ok" },
  "parties": [ { "role": "grantee", "kind": "person", "id": "user-0002", "email": "member@example.com", "name": "Team Member" } ],
  "labels":  { "role": "Reader" }
}
```

A revoke is the same shape with `access.revoke`. Grantees are people, so a
reader sees each as a ref, as with any party.

## Review

`access.review` is not posted by a collector. A review is an Orrery action
run, and Orrery's action forwarder posts every run to Annals, so each
review and each removal it makes arrives that way, under Orrery's source.
