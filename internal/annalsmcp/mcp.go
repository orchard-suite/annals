// Package annalsmcp is Annals' MCP server: three tools an agent calls to
// ask what happened. It runs on the API port behind the same bearer token
// as /v1, stateless, so each call stands alone, and answers at the level
// the token reads at: a reader token sees each person as a ref.
package annalsmcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/orchard-suite/annals/internal/people"
	"github.com/orchard-suite/annals/internal/store"
	"github.com/orchard-suite/platformkit/v3/annals"
)

type levelKey struct{}

// WithLevel tells the tools how much of a person the caller may see. A
// call without it sees what a reader sees.
func WithLevel(ctx context.Context, l people.Level) context.Context {
	return context.WithValue(ctx, levelKey{}, l)
}

func levelFrom(ctx context.Context) people.Level {
	l, _ := ctx.Value(levelKey{}).(people.Level)
	return l
}

type shownKey struct{}

// WithShown sets what runs each time a tool answers with people shown, so
// the caller can keep a record of who saw people.
func WithShown(ctx context.Context, shown func()) context.Context {
	return context.WithValue(ctx, shownKey{}, shown)
}

func shownFrom(ctx context.Context) func() {
	f, _ := ctx.Value(shownKey{}).(func())
	return f
}

const instructions = `Annals is the record of what every actor (person, agent, or system) did across every product: who acted, for whom, with what authority, on what, with whom, and how it went. Use annals_search to find records (by actor, party, target, or the record that caused them), annals_actor_summary to see what one actor has been doing, and annals_target_history for everything that touched one thing. A record that names a cause was set off by another record from the same source, such as a tool call by the prompt behind it; search with source and cause set to a record's event_key to see what it led to. Records name people and things, never content. A token that reads as reader sees each person as a ref such as p-3f9a1c2b7e4d, and finds people only by ref.`

// Handler is the streamable HTTP endpoint. The caller mounts it at /mcp and
// checks the bearer token first.
func Handler(st *store.Store, version string) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return newServer(st, version, levelFrom(r.Context()), shownFrom(r.Context()))
	}, &mcp.StreamableHTTPOptions{Stateless: true})
}

// NewServer builds the MCP server with the three tools, answering at level.
func NewServer(st *store.Store, version string, level people.Level) *mcp.Server {
	return newServer(st, version, level, nil)
}

func newServer(st *store.Store, version string, level people.Level, shown func()) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "annals", Title: "Annals", Version: version}, &mcp.ServerOptions{Instructions: instructions})
	t := &tools{store: st, level: level, masker: st.Masker(), shown: shown}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "annals_search",
		Description: "Search Annals' events, newest first. Every filter is optional; since and until take RFC 3339 times or spans back from now such as 24h or 7d.",
	}, t.search)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "annals_actor_summary",
		Description: "What one actor (an agent, person, or system, by its id) did in a window: counts by outcome, verb, system, and source, and its latest events.",
	}, t.actorSummary)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "annals_target_history",
		Description: "Everything that touched one target, newest first: the system it lives in, its kind, and its id, as the sending product names them.",
	}, t.targetHistory)
	return s
}

type tools struct {
	store  *store.Store
	level  people.Level
	masker people.Masker
	// shown runs when an answer shows people; nil when nobody listens.
	shown func()
}

func (t *tools) filter(f store.Filter) store.Filter {
	f.Masked = !t.level.SeesPeople()
	return f
}

func (t *tools) events(evs []annals.Event) []annals.Event {
	if t.level.SeesPeople() && t.shown != nil {
		t.shown()
	}
	out := make([]annals.Event, len(evs))
	for i, ev := range evs {
		out[i] = t.masker.For(t.level, ev)
	}
	return out
}

type SearchIn struct {
	Since    string `json:"since,omitempty" jsonschema:"RFC 3339 time or a span back from now such as 24h or 7d"`
	Until    string `json:"until,omitempty" jsonschema:"RFC 3339 time or a span back from now"`
	Actor    string `json:"actor,omitempty" jsonschema:"actor.id, or a person's ref"`
	Party    string `json:"party,omitempty" jsonschema:"a party's id, email, or ref: who a thing was shared with or who was added"`
	Cause    string `json:"cause,omitempty" jsonschema:"an event_key; finds the records it caused, such as the tool calls behind a prompt. Pair with source."`
	Source   string `json:"source,omitempty" jsonschema:"the product that sent the event"`
	System   string `json:"system,omitempty" jsonschema:"action.target.system"`
	Outcome  string `json:"outcome,omitempty" jsonschema:"ok, refused, or failed"`
	Verb     string `json:"verb,omitempty" jsonschema:"action.verb such as file.write"`
	Approval bool   `json:"approval_required,omitempty" jsonschema:"only events whose authority required approval"`
	Limit    int    `json:"limit,omitempty" jsonschema:"events per page, 50 by default, 500 at most"`
	Cursor   string `json:"cursor,omitempty" jsonschema:"the next cursor from an earlier page"`
}

type SearchOut struct {
	Events []annals.Event `json:"events"`
	Next   string         `json:"next,omitempty"`
	Total  int            `json:"total"`
}

func (t *tools) search(ctx context.Context, _ *mcp.CallToolRequest, in SearchIn) (*mcp.CallToolResult, SearchOut, error) {
	f, err := filterFrom(in)
	if err != nil {
		return nil, SearchOut{}, err
	}
	f = t.filter(f)
	page, err := t.store.Query(ctx, f)
	if err != nil {
		return nil, SearchOut{}, err
	}
	total, err := t.store.Count(ctx, f)
	if err != nil {
		return nil, SearchOut{}, err
	}
	out := SearchOut{Events: t.events(page.Events), Next: page.Next, Total: total}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: describeSearch(out)}}}, out, nil
}

func filterFrom(in SearchIn) (store.Filter, error) {
	f := store.Filter{Actor: in.Actor, Party: in.Party, Cause: in.Cause, Source: in.Source, System: in.System, Outcome: in.Outcome, Verb: in.Verb, ApprovalRequired: in.Approval, Limit: in.Limit, Cursor: in.Cursor}
	var err error
	if f.Since, err = parseTime(in.Since); err != nil {
		return f, fmt.Errorf("since: %w", err)
	}
	if f.Until, err = parseTime(in.Until); err != nil {
		return f, fmt.Errorf("until: %w", err)
	}
	if in.Outcome != "" && !contains(annals.Outcomes, in.Outcome) {
		return f, errors.New("outcome must be ok, refused, or failed")
	}
	return f, nil
}

func describeSearch(out SearchOut) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d events match; showing %d.\n", out.Total, len(out.Events))
	for _, ev := range out.Events {
		b.WriteString(line(ev))
	}
	if out.Next != "" {
		fmt.Fprintf(&b, "More: pass cursor %q.\n", out.Next)
	}
	return b.String()
}

func line(ev annals.Event) string {
	who := ev.Actor.ID
	if ev.Actor.Name != "" {
		who = ev.Actor.Name + " (" + ev.Actor.ID + ")"
	}
	s := fmt.Sprintf("%s  %s  %s %s  %s %s/%s %s", ev.At.UTC().Format(time.RFC3339), ev.ID, ev.Actor.Kind, who, ev.Action.Verb, ev.Action.Target.System, ev.Action.Target.Kind, ev.Action.Target.ID)
	if ev.Action.Target.Title != "" {
		s += " " + strconvQuote(ev.Action.Target.Title)
	}
	if ev.Actor.OnBehalfOf != "" {
		s += "  for " + ev.Actor.OnBehalfOf
	}
	for _, p := range ev.Parties {
		s += "  " + p.Role + " " + partyName(p)
	}
	if len(ev.Items) > 0 {
		names := make([]string, len(ev.Items))
		for i, it := range ev.Items {
			names[i] = it.Title
			if names[i] == "" {
				names[i] = it.ID
			}
		}
		s += "  touched " + strings.Join(names, ", ")
	}
	s += "  " + ev.Action.Outcome
	if ev.Action.Reason != "" {
		s += ": " + ev.Action.Reason
	}
	if ev.Authority != nil && ev.Authority.Approval != nil && ev.Authority.Approval.Required {
		if ev.Authority.Approval.By != "" {
			s += "  approved by " + ev.Authority.Approval.By
		} else {
			s += "  approval required"
		}
	}
	if ev.EventKey != "" {
		s += "  key " + ev.EventKey
	}
	if ev.Cause != "" {
		s += "  caused by " + ev.Cause
	}
	return s + "  [" + ev.Source + "]\n"
}

func partyName(p annals.Party) string {
	switch {
	case p.Email != "":
		return p.Email
	case p.Name != "":
		return p.Name
	case p.ID != "":
		return p.ID
	}
	return "anyone with the link"
}

func strconvQuote(s string) string { return fmt.Sprintf("%q", s) }

type ActorIn struct {
	Actor string `json:"actor" jsonschema:"actor.id"`
	Since string `json:"since,omitempty" jsonschema:"RFC 3339 time or a span back from now; 7d by default"`
	Limit int    `json:"limit,omitempty" jsonschema:"latest events to include, 20 by default"`
}

type ActorOut struct {
	Actor    string         `json:"actor"`
	Since    time.Time      `json:"since"`
	Total    int            `json:"total"`
	Outcomes []store.Bucket `json:"outcomes"`
	Verbs    []store.Bucket `json:"verbs"`
	Systems  []store.Bucket `json:"systems"`
	Sources  []store.Bucket `json:"sources"`
	Latest   []annals.Event `json:"latest"`
}

func (t *tools) actorSummary(ctx context.Context, _ *mcp.CallToolRequest, in ActorIn) (*mcp.CallToolResult, ActorOut, error) {
	if in.Actor == "" {
		return nil, ActorOut{}, errors.New("actor is required")
	}
	since, err := parseTime(in.Since)
	if err != nil {
		return nil, ActorOut{}, fmt.Errorf("since: %w", err)
	}
	if since.IsZero() {
		since = time.Now().Add(-7 * 24 * time.Hour)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	f := t.filter(store.Filter{Actor: in.Actor, Since: since, Limit: limit})
	out := ActorOut{Actor: in.Actor, Since: since.UTC()}
	if out.Total, err = t.store.Count(ctx, f); err != nil {
		return nil, out, err
	}
	for _, g := range []struct {
		col string
		dst *[]store.Bucket
	}{{"outcome", &out.Outcomes}, {"verb", &out.Verbs}, {"target_system", &out.Systems}, {"source", &out.Sources}} {
		if *g.dst, err = t.store.Breakdown(ctx, f, g.col, 20); err != nil {
			return nil, out, err
		}
	}
	page, err := t.store.Query(ctx, f)
	if err != nil {
		return nil, out, err
	}
	out.Latest = t.events(page.Events)
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d events since %s.\n", in.Actor, out.Total, since.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Outcomes: %s\nVerbs: %s\nSystems: %s\nSources: %s\nLatest:\n", buckets(out.Outcomes), buckets(out.Verbs), buckets(out.Systems), buckets(out.Sources))
	for _, ev := range out.Latest {
		b.WriteString(line(ev))
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}}}, out, nil
}

type TargetIn struct {
	System string `json:"system" jsonschema:"action.target.system"`
	Kind   string `json:"kind,omitempty" jsonschema:"action.target.kind"`
	ID     string `json:"id" jsonschema:"action.target.id"`
	Since  string `json:"since,omitempty" jsonschema:"RFC 3339 time or a span back from now"`
	Limit  int    `json:"limit,omitempty" jsonschema:"events per page, 50 by default"`
	Cursor string `json:"cursor,omitempty"`
}

type TargetOut struct {
	Total  int            `json:"total"`
	Actors []store.Bucket `json:"actors"`
	Events []annals.Event `json:"events"`
	Next   string         `json:"next,omitempty"`
}

func (t *tools) targetHistory(ctx context.Context, _ *mcp.CallToolRequest, in TargetIn) (*mcp.CallToolResult, TargetOut, error) {
	if in.System == "" || in.ID == "" {
		return nil, TargetOut{}, errors.New("system and id are required")
	}
	since, err := parseTime(in.Since)
	if err != nil {
		return nil, TargetOut{}, fmt.Errorf("since: %w", err)
	}
	f := t.filter(store.Filter{System: in.System, TargetKind: in.Kind, TargetID: in.ID, Since: since, Limit: in.Limit, Cursor: in.Cursor})
	var out TargetOut
	if out.Total, err = t.store.Count(ctx, f); err != nil {
		return nil, out, err
	}
	if out.Actors, err = t.store.Breakdown(ctx, f, "actor_id", 20); err != nil {
		return nil, out, err
	}
	page, err := t.store.Query(ctx, f)
	if err != nil {
		return nil, out, err
	}
	out.Events, out.Next = t.events(page.Events), page.Next
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s: %d events. Actors: %s\n", in.System, in.Kind, in.ID, out.Total, buckets(out.Actors))
	for _, ev := range out.Events {
		b.WriteString(line(ev))
	}
	if out.Next != "" {
		fmt.Fprintf(&b, "More: pass cursor %q.\n", out.Next)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}}}, out, nil
}

func buckets(bs []store.Bucket) string {
	if len(bs) == 0 {
		return "none"
	}
	parts := make([]string, len(bs))
	for i, b := range bs {
		parts[i] = fmt.Sprintf("%s %d", b.Key, b.Count)
		if b.Label != "" {
			parts[i] = fmt.Sprintf("%s (%s) %d", b.Label, b.Key, b.Count)
		}
	}
	return strings.Join(parts, ", ")
}

func parseTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if n, ok := strings.CutSuffix(v, "d"); ok {
		var days int
		if _, err := fmt.Sscanf(n, "%d", &days); err != nil || days < 0 {
			return time.Time{}, fmt.Errorf("%q is not a time or a span", v)
		}
		return time.Now().Add(-time.Duration(days) * 24 * time.Hour), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a time or a span", v)
	}
	return time.Now().Add(-d), nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
