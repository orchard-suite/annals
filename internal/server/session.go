package server

import (
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"
	"github.com/orchard-suite/platformkit/v3/door"
	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/annals/internal/people"
	"github.com/orchard-suite/annals/internal/selfaudit"
	"github.com/orchard-suite/annals/internal/store"
)

//go:embed templates
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

var pages = map[string]*template.Template{}

func init() {
	funcs := template.FuncMap{
		"ago":   ago,
		"clock": func(t time.Time) string { return t.In(zone).Format("2006-01-02 15:04:05 MST") },
		"day":   func(t time.Time) string { return t.In(zone).Format("Mon Jan 2") },
		"hm":    hm,
		"zone":  zoneName,
		"query": url.QueryEscape,
		"path":  url.PathEscape,
		"pct":   pct,
		"targetURL": func(t annals.Target) string {
			return "/targets/" + url.PathEscape(t.System) + "/" + url.PathEscape(t.Kind) + "/" + url.PathEscape(t.ID)
		},
		"actorURL": func(a annals.Actor) string { return "/actors/" + url.PathEscape(a.ID) },
		// who names an actor for people: its name, else its email, else its id.
		// A reader's view has no names or emails, so a person is their ref.
		"who": func(a annals.Actor) string {
			switch {
			case a.Name != "":
				return a.Name
			case a.Email != "":
				return a.Email
			}
			return a.ID
		},
		"partyURL":    func(p annals.Party) string { return "/records?party=" + url.QueryEscape(p.ID) + "&since=" },
		"plain":       plainVerb,
		"targetName":  targetName,
		"busyOf":      func(b busy, max int) busyRow { return busyRow{B: b, Max: max} },
		"kindWords":   kindWords,
		"verbFor":     verbFor,
		"firstDay":    func(g []dayGroup) []dayGroup { return g[:min(1, len(g))] },
		"verbWords":   verbWords,
		"targetSaid":  targetSaid,
		"targetsSaid": targetsSaid,
		"plural":      plural,
		"count":       thousands,
		"spark":       spark,
		"when":        when,
		"days":        days,
		"pulseOf":     func(d digest, small bool) pulseView { return pulseView{D: d, Small: small} },
		"stepName":    stepName,
		"add":         func(a, b int) int { return a + b },
		"sub":         func(a, b int) int { return a - b },
		"lower":       strings.ToLower,
		"ratio":       func(n, of int) int { return n / max(1, of) },
		"isRef":       people.IsRef,
	}
	for _, name := range []string{"today", "records", "event", "actor", "target", "approvals", "tokens", "token", "message"} {
		pages[name] = template.Must(template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/parts.html", "templates/"+name+".html"))
	}
}

// Session is the board: today, records, actors, targets, approvals, tokens,
// and the browser telemetry relay.
func (s *Server) Session() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(http.FileServerFS(static))))
	// A device that adds the board to a Home Screen or installs it on a
	// desktop fetches these without a session, so no role gates them.
	for path, kind := range map[string]string{
		"/apple-touch-icon.png": "image/png",
		"/icon-192.png":         "image/png",
		"/icon-512.png":         "image/png",
		"/manifest.webmanifest": "application/manifest+json",
	} {
		mux.Handle("GET "+path, cacheFor(installFile(static, path[1:], kind)))
	}
	mux.Handle("GET /{$}", s.readers.Func(s.pageToday))
	mux.Handle("GET /records", s.readers.Func(s.pageRecords))
	mux.Handle("GET /events/{id}", s.readers.Func(s.pageEvent))
	mux.Handle("GET /actors/{id}", s.readers.Func(s.pageActor))
	mux.Handle("GET /targets/{system}/{kind}/{id}", s.readers.Func(s.pageTarget))
	mux.Handle("GET /approvals", s.readers.Func(s.pageApprovals))
	// Tokens are issued and revoked here, so only the admin role opens them.
	mux.Handle("GET /tokens", s.admins.Func(s.pageTokens))
	mux.Handle("POST /tokens", s.admins.Wrap(door.SameOrigin(s.publicOrigin, s.log, http.HandlerFunc(s.formMintToken))))
	mux.Handle("POST /tokens/{id}/revoke", s.admins.Wrap(door.SameOrigin(s.publicOrigin, s.log, http.HandlerFunc(s.formRevokeToken))))
	// Tying an id to a person changes whom every record names, so only
	// the admin role does it.
	mux.Handle("POST /actors/{id}/link", s.admins.Wrap(door.SameOrigin(s.publicOrigin, s.log, http.HandlerFunc(s.formLink))))
	mux.Handle("POST /actors/{id}/unlink", s.admins.Wrap(door.SameOrigin(s.publicOrigin, s.log, http.HandlerFunc(s.formUnlink))))
	mux.Handle("POST /api/telemetry", s.readers.Wrap(s.relay))
	return telemetry.Handler(mux, "session")
}

// installFile serves one file from the static files with the content type
// a device expects of it.
func installFile(static fs.FS, name, kind string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", kind)
		http.ServeFileFS(w, r, static, name)
	})
}

func cacheFor(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

// level is how much of a person the signed-in caller may see.
func (s *Server) level(r *http.Request) people.Level {
	p := door.From(r.Context())
	switch {
	case p.Has(s.cfg.AdminRole):
		return people.Admin
	case p.Has(s.cfg.PIIRole):
		return people.PII
	}
	return people.Reader
}

// sessionView is the signed-in caller's view. A view that shows people is
// logged and kept as a people.view record, so who looked at people is on
// record.
func (s *Server) sessionView(r *http.Request) view {
	v := s.viewAt(s.level(r))
	s.peopleShown(r, v.level)
	return v
}

func (s *Server) peopleShown(r *http.Request, level people.Level) {
	if !level.SeesPeople() {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "people shown", r, "level", level.String(), "path", r.URL.Path)
	s.audit.PeopleViewed(r.Context(), s.audit.PersonViewer(door.From(r.Context()).Email), level, selfaudit.DoorBoard)
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data map[string]any) {
	p := door.From(r.Context())
	level := s.level(r)
	data["Email"] = p.Email
	data["Admin"] = level == people.Admin
	data["SeesPeople"] = level.SeesPeople()
	data["Level"] = level.String()
	data["Nav"] = page
	data["Version"] = s.cfg.Version
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)
	if err := pages[page].Execute(w, data); err != nil {
		s.logRequest(r.Context(), slog.LevelError, "page render failed", r, "page", page, "error", err)
	}
}

func (s *Server) message(w http.ResponseWriter, r *http.Request, status int, title, text string) {
	s.render(w, r, status, "message", map[string]any{"Title": title, "Text": text})
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.logRequest(r.Context(), slog.LevelError, what+" failed", r, "error", err)
	s.message(w, r, http.StatusInternalServerError, "Something broke", "The "+what+" could not be read.")
}

// counts turns outcome buckets into a map with every outcome present.
func counts(bs []store.Bucket) map[string]int {
	m := map[string]int{"ok": 0, "refused": 0, "failed": 0, "total": 0}
	for _, b := range bs {
		m[b.Key] = b.Count
		m["total"] += b.Count
	}
	return m
}

func nextURL(u *url.URL, cursor string) string {
	if cursor == "" {
		return ""
	}
	q := u.Query()
	q.Set("cursor", cursor)
	return u.Path + "?" + q.Encode()
}

func (s *Server) pageEvent(w http.ResponseWriter, r *http.Request) {
	v := s.sessionView(r)
	ev, err := s.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.message(w, r, http.StatusNotFound, "Event", "There is no event with that id.")
		return
	}
	if err != nil {
		s.fail(w, r, "event", err)
		return
	}
	corrected := []annals.Event{}
	if ev.Corrects != "" {
		if old, err := s.store.Get(r.Context(), ev.Corrects); err == nil {
			corrected = append(corrected, old)
		}
	}
	corrections, err := s.store.Query(r.Context(), store.Filter{Limit: 20, TargetID: ev.Action.Target.ID, System: ev.Action.Target.System})
	if err != nil {
		s.fail(w, r, "event", err)
		return
	}
	var later []annals.Event
	for _, c := range corrections.Events {
		if c.Corrects == ev.ID {
			later = append(later, c)
		}
	}
	// The record that caused this one, and the records this one caused.
	var causedBy []annals.Event
	if ev.Cause != "" {
		if c, err := s.store.ByKey(r.Context(), ev.CauseFrom(), ev.Cause); err == nil {
			causedBy = append(causedBy, c)
		} else if !errors.Is(err, store.ErrNotFound) {
			s.fail(w, r, "event", err)
			return
		}
	}
	var led []annals.Event
	if ev.EventKey != "" {
		p, err := s.store.Query(r.Context(), store.Filter{CauseSource: ev.Source, Cause: ev.EventKey, Limit: store.MaxLimit})
		if err != nil {
			s.fail(w, r, "event", err)
			return
		}
		led = p.Events
	}
	shown := v.event(ev)
	near, err := s.around(r.Context(), v, shown)
	if err != nil {
		s.fail(w, r, "event", err)
		return
	}
	raw, _ := jsonIndent(shown)
	s.render(w, r, http.StatusOK, "event", map[string]any{
		"Event": shown, "Corrected": v.events(corrected), "Corrections": v.events(later),
		"CausedBy": v.events(causedBy), "CauseFrom": ev.CauseFrom(), "Led": v.events(led), "Raw": raw, "Near": near,
	})
}

func (s *Server) pageActor(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	q := r.URL.Query()
	win := windowFor(q, "7d")
	v := s.sessionView(r)
	d, err := s.digest(ctx, v, store.Filter{Actor: id}, url.Values{}, win)
	if err != nil {
		s.fail(w, r, "actor", err)
		return
	}
	groups, next, err := s.stream(ctx, v, store.Filter{Actor: id, Since: d.Since, Cursor: q.Get("cursor")}, r.URL, true)
	if err != nil {
		s.fail(w, r, "actor", err)
		return
	}
	data := map[string]any{"ID": id, "D": d, "Days": groups, "Next": next, "FoldAt": foldAt,
		"Spans": spans, "Since": win.Key, "Query": q}
	data["Actor"] = annals.Actor{ID: id}
	if first, ok := firstOf(groups); ok {
		data["Actor"], data["Agent"] = first.Actor, first.Agent
	} else {
		latest, err := s.store.Query(ctx, v.filter(store.Filter{Actor: id, Limit: 1}))
		if err != nil {
			s.fail(w, r, "actor", err)
			return
		}
		if len(latest.Events) > 0 {
			shown := v.event(latest.Events[0])
			data["Actor"], data["Agent"] = shown.Actor, shown.Agent
		}
	}
	for name, col := range map[string]string{"Verbs": "verb", "Systems": "target_system"} {
		bs, err := s.store.Breakdown(ctx, v.filter(store.Filter{Actor: id, Since: d.Since}), col, 12)
		if err != nil {
			s.fail(w, r, "actor", err)
			return
		}
		data[name] = bs
	}
	if v.level.SeesPeople() {
		data["Person"], data["IDs"] = s.person(id)
	}
	s.render(w, r, http.StatusOK, "actor", data)
}

// personID is one id a person was posted under, and whether an admin tied
// it to them, so an admin may untie it.
type personID struct {
	ID     string
	ByHand bool
}

// person is the directory's person behind id, when it knows them, and every
// id it ties to them.
func (s *Server) person(id string) (*people.Person, []personID) {
	dir := s.store.Directory()
	p, ok := dir.Find(id)
	if !ok {
		return nil, nil
	}
	var ids []personID
	for _, other := range dir.IDs(id) {
		l, linked := dir.LinkOf(other)
		ids = append(ids, personID{ID: other, ByHand: linked && l.Source == people.LinkAdmin})
	}
	return &p, ids
}

// formLink ties the actor's id to the person the admin named by any id of
// theirs, such as their email.
func (s *Server) formLink(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	to := strings.TrimSpace(r.PostFormValue("person"))
	by := door.From(r.Context()).Email
	if id == "" || to == "" || people.IsRef(id) {
		s.message(w, r, http.StatusBadRequest, "Same person", "Name the person by their email or directory id.")
		return
	}
	l, p, err := s.store.Link(r.Context(), id, to, by)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.logRequest(r.Context(), slog.LevelInfo, "link refused: no such person", r)
		s.message(w, r, http.StatusBadRequest, "Same person", "The directory has no one by that email or id.")
		return
	case errors.Is(err, store.ErrConflict):
		s.logRequest(r.Context(), slog.LevelInfo, "link refused: the id is another person's own", r)
		s.message(w, r, http.StatusConflict, "Same person", "That id is another person's own id or email in the directory.")
		return
	case err != nil:
		s.logRequest(r.Context(), slog.LevelError, "link failed", r, "error", err)
		s.message(w, r, http.StatusInternalServerError, "Same person", "The link could not be made.")
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "id linked to a person", r)
	s.audit.Linked(r.Context(), by, l.ID, p.ID)
	http.Redirect(w, r, "/actors/"+url.PathEscape(id), http.StatusSeeOther)
}

// formUnlink unties an id an admin tied to a person.
func (s *Server) formUnlink(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	back := r.PostFormValue("back")
	if back == "" || !strings.HasPrefix(back, "/actors/") {
		back = "/actors/" + url.PathEscape(id)
	}
	by := door.From(r.Context()).Email
	l, err := s.store.Unlink(r.Context(), id, by)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.logRequest(r.Context(), slog.LevelInfo, "unlink skipped: no link an admin made", r)
	case err != nil:
		s.logRequest(r.Context(), slog.LevelError, "unlink failed", r, "error", err)
		s.message(w, r, http.StatusInternalServerError, "Same person", "The link could not be removed.")
		return
	default:
		s.logRequest(r.Context(), slog.LevelInfo, "id unlinked from a person", r)
		s.audit.Unlinked(r.Context(), by, l.ID, l.Person)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) pageTarget(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	win := windowFor(q, "")
	v := s.sessionView(r)
	t := annals.Target{System: r.PathValue("system"), Kind: r.PathValue("kind"), ID: r.PathValue("id")}
	all := store.Filter{System: t.System, TargetKind: t.Kind, TargetID: t.ID}
	d, err := s.digest(ctx, v, all, url.Values{"target": {t.ID}}, win)
	if err != nil {
		s.fail(w, r, "target", err)
		return
	}
	in := all
	in.Since = d.Since
	in = v.filter(in)
	groups, next, err := s.stream(ctx, v, withCursor(in, q.Get("cursor")), r.URL, true)
	if err != nil {
		s.fail(w, r, "target", err)
		return
	}
	if first, ok := firstOf(groups); ok {
		t.Title = first.Action.Target.Title
	}
	total, err := s.store.Count(ctx, v.filter(all))
	if err != nil {
		s.fail(w, r, "target", err)
		return
	}
	var first *annals.Event
	if oldest, err := s.store.Oldest(ctx, v.filter(all)); err != nil {
		s.fail(w, r, "target", err)
		return
	} else if !oldest.IsZero() {
		at := all
		at.Since, at.Until, at.Limit = oldest, oldest.Add(time.Millisecond), 1
		p, err := s.store.Query(ctx, v.filter(at))
		if err != nil {
			s.fail(w, r, "target", err)
			return
		}
		if len(p.Events) > 0 {
			ev := v.event(p.Events[len(p.Events)-1])
			first = &ev
			// A first record already listed as worth a look is not said twice.
			for i, f := range d.Look {
				if f.Count == 1 && f.Latest.ID == ev.ID {
					d.Look[i].First, first = true, nil
				}
			}
		}
	}
	verbs, err := s.store.Breakdown(ctx, in, "verb", 12)
	if err != nil {
		s.fail(w, r, "target", err)
		return
	}
	s.render(w, r, http.StatusOK, "target", map[string]any{
		"Target": t, "Days": groups, "Next": next, "Total": total, "D": d, "First": first,
		"Verbs": verbs, "Spans": spans, "Since": win.Key, "Query": q,
	})
}

func withCursor(f store.Filter, cursor string) store.Filter {
	f.Cursor = cursor
	return f
}

func (s *Server) pageApprovals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	if !q.Has("since") {
		q.Set("since", "30d")
	}
	f, err := parseFilter(q)
	if err != nil {
		s.message(w, r, http.StatusBadRequest, "Approvals", err.Error())
		return
	}
	f.ApprovalRequired = true
	v := s.sessionView(r)
	groups, next, err := s.stream(ctx, v, f, r.URL, true)
	if err != nil {
		s.fail(w, r, "approvals", err)
		return
	}
	f = v.filter(f)
	approvers, err := s.store.Breakdown(ctx, f, "approval_by", 20)
	if err != nil {
		s.fail(w, r, "approvals", err)
		return
	}
	pending := 0
	var byPerson []store.Bucket
	for _, b := range approvers {
		if b.Key == "" {
			pending = b.Count
		} else {
			byPerson = append(byPerson, b)
		}
	}
	total, err := s.store.Count(ctx, f)
	if err != nil {
		s.fail(w, r, "approvals", err)
		return
	}
	s.render(w, r, http.StatusOK, "approvals", map[string]any{
		"Days": groups, "Next": next, "Approvers": byPerson, "Pending": pending, "Total": total,
		"Spans": spans, "Since": q.Get("since"), "Query": q, "Filter": f,
	})
}

func (s *Server) pageTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.store.Tokens(r.Context())
	if err != nil {
		s.fail(w, r, "tokens", err)
		return
	}
	// The list names the admin who issued each token.
	s.peopleShown(r, s.level(r))
	live, revoked := 0, 0
	for _, t := range tokens {
		if t.RevokedAt.IsZero() {
			live++
		} else {
			revoked++
		}
	}
	s.render(w, r, http.StatusOK, "tokens", map[string]any{"Tokens": tokens, "Live": live, "Revoked": revoked, "URL": s.cfg.PublicURL, "Retention": s.cfg.Retention.Describe(), "Levels": people.TokenLevels})
}

func (s *Server) formMintToken(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	source := strings.TrimSpace(r.PostFormValue("source"))
	reads := strings.TrimSpace(r.PostFormValue("reads"))
	if _, err := people.ParseTokenLevel(reads); err != nil {
		s.message(w, r, http.StatusBadRequest, "Tokens", "A token reads as reader, with people hidden, or as pii, with people shown.")
		return
	}
	if name == "" || source == "" {
		s.message(w, r, http.StatusBadRequest, "Tokens", "Give the token a name and the source it may post as.")
		return
	}
	probe := annals.Event{Source: source}
	if err := probe.Validate(source); annals.Reason(err) == "bad_slug" || !isSlug(source) {
		s.message(w, r, http.StatusBadRequest, "Tokens", "The source must be a lowercase slug, such as billing or ion.")
		return
	}
	by := door.From(r.Context()).Email
	secret, tok, err := s.store.Mint(r.Context(), name, source, reads, by)
	if err != nil {
		s.logRequest(r.Context(), slog.LevelError, "token mint failed", r, "error", err)
		s.message(w, r, http.StatusInternalServerError, "Tokens", "The token could not be made.")
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "token minted", r, "token", tok.Name, "source", tok.Source, "reads", tok.Reads, "token_id", tok.ID)
	s.audit.TokenIssued(r.Context(), by, tok)
	s.render(w, r, http.StatusOK, "token", map[string]any{"Token": tok, "Secret": secret, "URL": s.cfg.PublicURL})
}

func isSlug(v string) bool {
	if v == "" || len(v) > annals.MaxShort {
		return false
	}
	for i, c := range v {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case i > 0 && (c == '.' || c == '-' || c == '_'):
		default:
			return false
		}
	}
	return true
}

func (s *Server) formRevokeToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tok, err := s.store.Revoke(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.logRequest(r.Context(), slog.LevelInfo, "token revoke skipped: not live", r, "token_id", id)
	case err != nil:
		s.logRequest(r.Context(), slog.LevelError, "token revoke failed", r, "error", err)
		s.message(w, r, http.StatusInternalServerError, "Tokens", "The token could not be revoked.")
		return
	default:
		s.logRequest(r.Context(), slog.LevelInfo, "token revoked", r, "token_id", id)
		s.audit.TokenRevoked(r.Context(), door.From(r.Context()).Email, tok)
	}
	http.Redirect(w, r, "/tokens", http.StatusSeeOther)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + " min ago"
	case d < 48*time.Hour:
		return itoa(int(d.Hours())) + " h ago"
	default:
		return itoa(int(d.Hours()/24)) + " days ago"
	}
}

func pct(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return 100 * float64(part) / float64(whole)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
