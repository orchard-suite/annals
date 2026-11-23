package server

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/annals"

	"github.com/orchard-suite/annals/internal/store"
)

type span struct{ Key, Label string }

// spans are the windows the records page offers; the digest pages offer
// the same less "All time".
var spans = func() []span {
	out := make([]span, 0, len(windows)+1)
	for _, w := range windows {
		out = append(out, span{w.Key, w.Label})
	}
	return append(out, span{"", "All time"})
}()

// filterParams are the query parameters that narrow the records. "/" with
// any of them is the records page under its old address.
var filterParams = []struct{ Key, Name string }{
	{"actor", "who"}, {"source", "product"}, {"system", "system"}, {"kind", "kind"}, {"target", "target"},
	{"outcome", "outcome"}, {"verb", "verb"}, {"party", "with"}, {"cause", "set off by"}, {"approval", "approval"},
}

func (s *Server) pageToday(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for _, p := range filterParams {
		if q.Get(p.Key) != "" {
			http.Redirect(w, r, "/records?"+q.Encode(), http.StatusFound)
			return
		}
	}
	ctx := r.Context()
	win := windowFor(q, "24h")
	v := s.sessionView(r)
	d, err := s.digest(ctx, v, store.Filter{}, url.Values{}, win)
	if err != nil {
		s.fail(w, r, "today", err)
		return
	}
	sources, err := s.store.Breakdown(ctx, v.filter(store.Filter{Since: d.Since}), "source", 50)
	if err != nil {
		s.fail(w, r, "today", err)
		return
	}
	s.render(w, r, http.StatusOK, "today", map[string]any{
		"D": d, "Sources": sources, "FoldAt": foldAt, "Spans": spans, "Since": win.Key, "Query": q,
	})
}

// activeFilter is one filter in force, and the address without it.
type activeFilter struct{ Name, Value, Drop string }

func (s *Server) pageRecords(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !q.Has("since") {
		q.Set("since", "24h")
	}
	f, err := parseFilter(q)
	if err != nil {
		s.message(w, r, http.StatusBadRequest, "Records", err.Error())
		return
	}
	ctx := r.Context()
	v := s.sessionView(r)
	var active []activeFilter
	for _, p := range filterParams {
		if val := q.Get(p.Key); val != "" {
			drop := url.Values{}
			for k, vs := range q {
				if k != p.Key && k != "cursor" {
					drop[k] = vs
				}
			}
			active = append(active, activeFilter{Name: p.Name, Value: val, Drop: "/records?" + drop.Encode()})
		}
	}
	// A window the digest knows gets a pulse; any other since is counted plainly.
	var d digest
	if since := q.Get("since"); since == "" || windowFor(q, "24h").Key == since {
		scope := f
		scope.Since, scope.Cursor = time.Time{}, ""
		if d, err = s.digest(ctx, v, scope, url.Values{}, windowFor(q, "24h")); err != nil {
			s.fail(w, r, "records", err)
			return
		}
		f.Since = d.Since
	} else {
		outcomes, err := s.store.Breakdown(ctx, v.filter(f), "outcome", 3)
		if err != nil {
			s.fail(w, r, "records", err)
			return
		}
		c := counts(outcomes)
		d = digest{Window: window{Key: since, Label: "window asked for"}, Since: f.Since, Until: f.Until, Total: c["total"], Failed: c["failed"], Refused: c["refused"]}
	}
	every := q.Get("every") != ""
	groups, next, err := s.stream(ctx, v, f, r.URL, !every)
	if err != nil {
		s.fail(w, r, "records", err)
		return
	}
	f = v.filter(f)
	sources, err := s.store.Breakdown(ctx, v.filter(store.Filter{}), "source", 50)
	if err != nil {
		s.fail(w, r, "records", err)
		return
	}
	systems, err := s.store.Breakdown(ctx, v.filter(store.Filter{}), "target_system", 50)
	if err != nil {
		s.fail(w, r, "records", err)
		return
	}
	s.render(w, r, http.StatusOK, "records", map[string]any{
		"D": d, "Days": groups, "Next": next, "Every": every, "Filter": f, "Active": active,
		"Query": q, "Spans": spans, "Since": q.Get("since"), "Sources": sources, "Systems": systems, "Values": annals.Outcomes,
	})
}

// plainVerb is a verb as words: its last part, with underscores as spaces.
// "vault.reveal_secret" reads "reveal secret".
func plainVerb(v string) string {
	if i := strings.LastIndexByte(v, '.'); i >= 0 {
		v = v[i+1:]
	}
	return strings.ReplaceAll(v, "_", " ")
}

// genericHeads are verb prefixes that name a product's area, not a thing,
// so the words of the verb leave them out. An estate adds its products'
// own areas with Config.AreaWords.
var genericHeads = map[string]bool{"entity": true, "record": true, "item": true}

// addAreaWords adds an estate's area prefixes to genericHeads. New calls it
// once, before the server answers.
func addAreaWords(words []string) {
	for _, w := range words {
		if w = strings.TrimSpace(w); w != "" {
			genericHeads[w] = true
		}
	}
}

// irregular is the past tense of verbs that do not take -ed.
var irregular = map[string]string{
	"run": "ran", "send": "sent", "read": "read", "write": "wrote", "make": "made", "get": "got", "put": "put",
	"set": "set", "give": "gave", "take": "took", "find": "found", "begin": "began", "shut": "shut", "do": "did",
	"see": "saw", "go": "went", "build": "built", "hold": "held", "keep": "kept", "leave": "left", "sell": "sold",
	"draw": "drew", "forget": "forgot", "split": "split", "done": "done", "intake": "took in", "cut": "cut", "tracker": "tracker", "held": "held", "hidden": "hidden", "standby": "put on standby",
	"ingredient": "ingredient", "mass": "mass", "hass": "hass", "ops": "ops",
}

// doubles are verbs that double their last letter before -ed.
var doubles = map[string]bool{"drop": true, "stop": true, "ship": true, "plan": true, "flag": true, "skip": true,
	"snap": true, "trim": true, "wrap": true, "shop": true, "log": true, "tag": true, "pin": true, "map": true, "chat": true}

// past is a verb in the past tense. A word that is not a plain verb (one
// already in -ed or -ing, or a known noun) is left as it is.
func past(w string) string {
	if p, ok := irregular[w]; ok {
		return p
	}
	n := len(w)
	for _, end := range []string{"ed", "ing"} {
		if strings.HasSuffix(w, end) {
			return w
		}
	}
	switch {
	case n == 0:
		return w
	case doubles[w]:
		return w + w[n-1:] + "ed"
	case strings.HasSuffix(w, "e"):
		return w + "d"
	case n > 1 && w[n-1] == 'y' && !strings.ContainsRune("aeiou", rune(w[n-2])):
		return w[:n-1] + "ied"
	}
	return w + "ed"
}

// pastPhrase puts the first word of a verb phrase in the past tense.
func pastPhrase(p string) string {
	first, rest, _ := strings.Cut(p, " ")
	if rest == "" {
		return past(first)
	}
	return past(first) + " " + rest
}

// verbWords is an action's verb as words in its sentence, in the past
// tense: what the actor did. The verb's own
// noun stays when it is not the target's kind: "rule.changed" on a
// blueprint reads "changed rule on", "blueprint.changed" reads "changed".
func verbWords(a annals.Action) string {
	head, tail, ok := strings.Cut(a.Verb, ".")
	if !ok {
		return pastPhrase(plainVerb(a.Verb))
	}
	if i := strings.LastIndexByte(tail, '.'); i >= 0 {
		head, tail = tail[:i], tail[i+1:]
	}
	words := pastPhrase(strings.ReplaceAll(tail, "_", " "))
	if head == "tool" {
		// An agent's tool call reads as running the tool.
		return "ran " + strings.ReplaceAll(tail, "_", " ")
	}
	kind := strings.ReplaceAll(a.Target.Kind, "_", " ")
	h := strings.ReplaceAll(head, "_", " ")
	switch {
	case genericHeads[head], h == kind, h+"s" == kind, h == strings.TrimSuffix(kind, "s"), strings.HasSuffix(kind, " "+h):
		return words
	case strings.HasSuffix(tail, "ed"):
		return words + " " + h + " on"
	}
	return words
}

// targetName names a target for people: its title, else its kind and id.
func targetName(t annals.Target) string {
	if t.Title != "" {
		return t.Title
	}
	return strings.ReplaceAll(t.Kind, "_", " ") + " " + t.ID
}

// targetSaid names an action's target where it follows the verb in a
// sentence, without saying a word twice: a kind the verb already names is
// left out, and an id that only repeats the verb gives way to the kind.
func targetSaid(a annals.Action) string {
	t := a.Target
	if t.Title != "" {
		return t.Title
	}
	verb, kind := verbWords(a), strings.ReplaceAll(t.Kind, "_", " ")
	switch {
	case t.ID == a.Verb || strings.HasSuffix(verb, " "+t.ID) || verb == t.ID:
		return kind
	case strings.HasSuffix(verb, " "+kind) || strings.HasSuffix(verb, " "+strings.TrimSuffix(kind, "s")):
		return t.ID
	}
	return kind + " " + t.ID
}

// targetsSaid is how many targets an action touched, as the end of its
// sentence.
func targetsSaid(a annals.Action, n int) string { return thousands(n) + " " + plural(a.Target.Kind) }

// verbFor is an action's verb words before a count of its targets: a verb
// that names its kind leaves the kind to the count, so "revealed secret"
// before "3 secrets" reads "revealed".
func verbFor(a annals.Action, n int) string {
	verb, kind := verbWords(a), strings.ReplaceAll(a.Target.Kind, "_", " ")
	if n > 1 {
		for _, k := range []string{kind, strings.TrimSuffix(kind, "s")} {
			if v, ok := strings.CutSuffix(verb, " "+k); ok {
				return v
			}
		}
	}
	return verb
}

// kindWords is a target kind as words.
func kindWords(kind string) string { return strings.ReplaceAll(kind, "_", " ") }

// plural is a target kind, as words, for more than one.
func plural(kind string) string {
	k := strings.ReplaceAll(kind, "_", " ")
	switch {
	case strings.HasSuffix(k, "s"):
		return k
	case strings.HasSuffix(k, "y") && !strings.HasSuffix(k, "ey"):
		return k[:len(k)-1] + "ies"
	}
	return k + "s"
}

// thousands writes n with commas.
func thousands(n int) string {
	s := itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

// spark draws counts as a small column chart against peak, on a square
// root scale so a quiet pattern still shows its shape beside a loud one.
func spark(steps []int, peak int) template.HTML {
	const w, h = 112.0, 28.0
	peak = max(peak, 1)
	for _, n := range steps {
		peak = max(peak, n)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="spark" viewBox="0 0 %g %g" aria-hidden="true" preserveAspectRatio="none">`, w, h)
	bw := w / float64(max(1, len(steps)))
	for i, n := range steps {
		if n == 0 {
			continue
		}
		bh := max(2, h*math.Sqrt(float64(n)/float64(peak)))
		fmt.Fprintf(&b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" rx="1"/>`, float64(i)*bw, h-bh, max(1, bw-1.5), bh)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// zone is the board's time zone: the server's, set with TZ.
var zone = time.Local

// dayStart is midnight, in the board's zone, of the day t falls on.
func dayStart(t time.Time) time.Time {
	y, m, d := t.In(zone).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, zone)
}

// hm is a time of day in the board's zone.
func hm(t time.Time) string { return t.In(zone).Format("15:04") }

// zoneName is the board's zone as people write it, such as CST.
func zoneName() string { return time.Now().In(zone).Format("MST") }

// when is a time as short as it can be and still be plain: the clock today,
// the weekday this week, the date before that.
func when(t time.Time) string {
	t, now := t.In(zone), time.Now().In(zone)
	switch {
	case t.Format("2006-01-02") == now.Format("2006-01-02"):
		return t.Format("15:04")
	case now.Sub(t) < 6*24*time.Hour:
		return t.Format("Mon 15:04")
	}
	return t.Format("Jan 2")
}

// dayGroup is the runs of one day.
type dayGroup struct {
	Label string
	Runs  []run
	// Self is set when the stream is one actor's, so its lines start at
	// the verb.
	Self bool
}

// byDay groups runs, newest first, by the day their newest record
// happened in the board's zone.
func byDay(runs []run) []dayGroup {
	var out []dayGroup
	now := time.Now().In(zone)
	for _, r := range runs {
		at := r.First().At.In(zone)
		label := at.Format("Monday, January 2")
		switch at.Format("2006-01-02") {
		case now.Format("2006-01-02"):
			label = "Today, " + at.Format("January 2")
		case now.Add(-24 * time.Hour).Format("2006-01-02"):
			label = "Yesterday, " + at.Format("January 2")
		}
		if at.Year() != now.Year() {
			label += at.Format(", 2006")
		}
		if len(out) == 0 || out[len(out)-1].Label != label {
			out = append(out, dayGroup{Label: label})
		}
		out[len(out)-1].Runs = append(out[len(out)-1].Runs, r)
	}
	return out
}

// days groups a short list of records, such as a record's causes, by day,
// folding runs.
func days(evs []annals.Event) []dayGroup { return byDay(runsOf(evs, true)) }

// pulseView is a digest's pulse, full or small.
type pulseView struct {
	D     digest
	Small bool
}

// stepName names one step of a window's pulse.
func stepName(w window) string {
	switch {
	case w.Step >= 24*time.Hour:
		return "day"
	case w.Step > time.Hour:
		return itoa(int(w.Step.Hours())) + " hours"
	}
	return "hour"
}
