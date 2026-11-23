# Adapters

How each product posts to Annals. Each page is a working snippet in the
product's own language, written against that product's real record shape.
None of them is installed by Annals; the product adds it in its own
repository.

- [Graft](graft.md): posts a job's lifecycle and approvals.
- [Ion](ion.md): a hook posts every tool call.
- [Microsoft 365](m365.md): sharing and Copilot records from the unified
  audit log, with a prompt linked to the tool calls it caused.
- [curl](curl.md): anything else, from a shell.
- [Access changes](access.md): the verbs a collector posts when a system's
  own audit log shows access given or taken away.

Every adapter needs a token issued on the board's Tokens page for its own
`source`, kept in the product's secret store, and sent as
`Authorization: Bearer <token>`. The contract they implement is
[the event and the record](../event.md). A Go product posts with the client in the `annals`
package of `github.com/orchard-suite/platformkit/v3`, which validates each event
before it sends it.
