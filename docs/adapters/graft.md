# Graft

Graft keeps one `Job` per idea or change and appends an `Event{At, Step,
Message}` as it moves through its states (`internal/store/store.go`:
`queued`, `naming`, `creating`, `cloning`, `building`, `pushing`, `landing`,
`releasing`, `watching`, `deploying`, `ready`, `failed`,
`awaiting_approval`, `declined`). A change that touches a protected path
waits in `awaiting_approval` until a person clicks approve, which sets
`ApprovedHead`.

Graft posts one event each time a step starts, ends, or waits, with
`event_key` as `job:<id>:<step>:<round>:<phase>` so a retried post is
stored once. A person's approval or decline is an event of its own, keyed
`job:<id>:approval:<head>`. It builds them on the `annals` package of
`github.com/orchard-suite/platformkit/v3`, whose client validates each event against
this contract before it posts and returns a refusal as an `*ErrInvalid`:

```go
package annals

import (
	"context"
	"fmt"
	"os"
	"time"

	kit "github.com/orchard-suite/platformkit/v3/annals"

	"github.com/orchard-suite/graft/internal/store"
)

// New returns nil without a URL or token, and a nil client posts nothing.
var client = kit.New(os.Getenv("GRAFT_ANNALS_URL"), os.Getenv("GRAFT_ANNALS_TOKEN"), "graft")

// Step posts a job's step reaching phase. reason says why a failed phase failed.
func Step(ctx context.Context, j *store.Job, phase, outcome, reason string) error {
	ev := kit.Event{
		At:        time.Now().UTC().Truncate(time.Second),
		EventKey:  fmt.Sprintf("job:%s:%s:%d:%s", j.ID, j.Step, j.Rounds, phase),
		Actor:     kit.Actor{Kind: "agent", ID: "graft-agent", Name: "Graft"},
		Agent:     &kit.Agent{Runtime: "claude-code", Harness: "graft"},
		Authority: &kit.Authority{TokenID: "github-app:graft", Scope: []string{"contents:write"}},
		Action: kit.Action{
			Verb:    "job." + j.Step,
			Target:  kit.Target{System: "graft", Kind: "job", ID: j.ID, Title: j.Repo},
			Outcome: outcome,
			Reason:  kit.Clip(reason),
		},
		Labels: map[string]string{"mode": j.Mode, "step": j.Step, "phase": phase},
	}
	if j.Commit != "" {
		ev.Change = &kit.Change{BeforeHash: j.Base, AfterHash: j.Commit, Summary: fmt.Sprintf("round %d", j.Rounds)}
	}
	return client.Post(ctx, ev)
}
```

A refusal is logged and not retried: the event will not pass next time
either. Any other failure is logged and dropped; a post never blocks a job.
Settings: `GRAFT_ANNALS_URL` and `GRAFT_ANNALS_TOKEN` in `graft.env`,
the token from the estate's secret store.
