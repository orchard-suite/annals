// Package directory keeps Annals' people directory in step with the
// tenant's Entra users. It reads Graph's user delta query, so after the
// first pass each pass reads only who changed, and writes each user as a
// person with every email they hold as a link to them. A user Entra
// deletes is kept and marked left, since their records outlive them.
package directory

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/orchard-suite/annals/internal/graph"
	"github.com/orchard-suite/annals/internal/people"
)

// deltaSetting is the setting that keeps the delta link of the last pass.
const deltaSetting = "directory_delta"

// firstPage is where a pass with no delta link starts: every user.
const firstPage = "/users/delta?$select=id,displayName,mail,userPrincipalName,proxyAddresses,otherMails"

// Store is where the directory lives.
type Store interface {
	Setting(ctx context.Context, name string) (string, error)
	SetSetting(ctx context.Context, name, value string) error
	Directory() *people.Directory
	WriteDirectory(ctx context.Context, ps []people.Person, links []people.Link) error
	LinkPosted(ctx context.Context, until time.Time) (int, error)
}

// Counter counts each pass by outcome.
type Counter interface {
	DirectoryPass(ctx context.Context, outcome string)
}

// Sync reads the tenant's users into the directory.
type Sync struct {
	Graph   *graph.Client
	Store   Store
	Counter Counter
	Log     *slog.Logger
}

// user is the part of an Entra user the directory keeps. A field Graph left
// out of a delta page is nil and keeps what the directory had.
type user struct {
	ID             string    `json:"id"`
	DisplayName    *string   `json:"displayName"`
	Mail           *string   `json:"mail"`
	UPN            *string   `json:"userPrincipalName"`
	ProxyAddresses *[]string `json:"proxyAddresses"`
	OtherMails     *[]string `json:"otherMails"`
	Removed        *struct {
		Reason string `json:"reason"`
	} `json:"@removed"`
}

// postedSettle is how long a pass leaves the newest records for their
// inserts to land before it ties the ids they were posted under.
const postedSettle = time.Minute

// Pass reads every user changed since the last pass, or every user on the
// first, and writes them. A delta link Graph no longer honors starts over.
// Then it ties the ids products posted people under to the people the
// same records name by email.
func (s *Sync) Pass(ctx context.Context) error {
	start := time.Now()
	n, err := s.pass(ctx)
	var gerr *graph.Error
	if errors.As(err, &gerr) && gerr.Status == http.StatusGone {
		s.Log.InfoContext(ctx, "directory delta expired; reading every user", "task", "directory")
		if err = s.Store.SetSetting(ctx, deltaSetting, ""); err == nil {
			n, err = s.pass(ctx)
		}
	}
	var tied int
	if err == nil {
		tied, err = s.Store.LinkPosted(ctx, time.Now().Add(-postedSettle))
	}
	outcome := "ok"
	if err != nil {
		outcome = "failed"
	}
	s.Counter.DirectoryPass(ctx, outcome)
	if err != nil {
		s.Log.ErrorContext(ctx, "directory pass failed", "task", "directory", "error", err)
		return err
	}
	s.Log.InfoContext(ctx, "directory pass done", "task", "directory", "users", n, "posted_ids_tied", tied, "people", s.Store.Directory().People(), "seconds", int(time.Since(start).Seconds()))
	return nil
}

func (s *Sync) pass(ctx context.Context) (int, error) {
	next, err := s.Store.Setting(ctx, deltaSetting)
	if err != nil {
		return 0, err
	}
	if next == "" {
		next = firstPage
	}
	read := 0
	for {
		var page struct {
			Value []user `json:"value"`
			Next  string `json:"@odata.nextLink"`
			Delta string `json:"@odata.deltaLink"`
		}
		if err := s.Graph.Do(ctx, http.MethodGet, next, nil, &page); err != nil {
			return read, err
		}
		read += len(page.Value)
		ps, links := s.changes(page.Value)
		if err := s.Store.WriteDirectory(ctx, ps, links); err != nil {
			return read, err
		}
		if page.Next != "" {
			next = page.Next
			continue
		}
		if page.Delta == "" {
			return read, errors.New("graph ended the user delta without a delta link")
		}
		return read, s.Store.SetSetting(ctx, deltaSetting, page.Delta)
	}
}

// changes turns one page of users into people and the links to them. An
// email a person held stays linked to them after they drop it, so their
// older records still find them; if the address goes to someone else, the
// directory's next link for it replaces the old one.
func (s *Sync) changes(users []user) (ps []people.Person, links []people.Link) {
	dir := s.Store.Directory()
	for _, u := range users {
		id := people.Key(u.ID)
		if id == "" {
			continue
		}
		known, _ := dir.Find(id)
		p := people.Person{ID: id, Name: known.Name, Email: known.Email}
		if u.Removed != nil {
			p.Left = true
			ps = append(ps, p)
			continue
		}
		if u.DisplayName != nil {
			p.Name = *u.DisplayName
		}
		mail, upn := deref(u.Mail), deref(u.UPN)
		if u.Mail != nil || u.UPN != nil {
			p.Email = people.Key(firstEmail(mail, upn))
		}
		ps = append(ps, p)
		emails := map[string]bool{}
		for _, e := range append([]string{mail, upn, known.Email}, append(addresses(u.ProxyAddresses), derefs(u.OtherMails)...)...) {
			if e = people.Key(e); strings.Contains(e, "@") && e != p.Email && !emails[e] {
				emails[e] = true
				links = append(links, people.Link{ID: e, Person: id, Source: people.LinkDirectory})
			}
		}
	}
	return ps, links
}

// addresses are the email addresses among a user's proxy addresses, such as
// SMTP:a@example.com; other kinds, such as SIP, are left out.
func addresses(proxy *[]string) []string {
	var out []string
	for _, a := range derefs(proxy) {
		if kind, addr, ok := strings.Cut(a, ":"); ok && strings.EqualFold(kind, "smtp") {
			out = append(out, addr)
		}
	}
	return out
}

func firstEmail(vs ...string) string {
	for _, v := range vs {
		if strings.Contains(v, "@") {
			return v
		}
	}
	return ""
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefs(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

// Run passes every interval until ctx ends.
func (s *Sync) Run(ctx context.Context, interval time.Duration) {
	tracer := otel.Tracer("annals")
	pass := func() {
		spanCtx, span := tracer.Start(ctx, "directory")
		defer span.End()
		_ = s.Pass(spanCtx)
	}
	pass()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass()
		}
	}
}
