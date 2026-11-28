package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/orchard-suite/platformkit/v3/annals"
	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/annals/internal/annalsmcp"
	"github.com/orchard-suite/annals/internal/people"
	"github.com/orchard-suite/annals/internal/selfaudit"
	"github.com/orchard-suite/annals/internal/store"
)

// maxBody is the largest POST /v1/events body: a batch of MaxBatch events of
// a few KB each.
const maxBody = 8 << 20

// API is the machine port. It trusts a bearer token it issued and nothing
// else; /healthz is the one open route.
func (s *Server) API() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.Handle("POST /v1/events", s.sender(s.apiPost))
	mux.Handle("GET /v1/events", s.sender(s.apiList))
	mux.Handle("GET /v1/events/{id}", s.sender(s.apiGet))
	mux.Handle("GET /v1/stats", s.sender(s.apiStats))
	mux.Handle("GET /v1/proof", s.sender(s.apiProof))
	mux.Handle("GET /v1/gate/families", s.sender(s.apiGateFamilies))
	mux.Handle("GET /v1/gate/drafts", s.sender(s.apiGateDrafts))
	mux.Handle("GET /v1/whoami", s.sender(s.apiWhoami))
	mcp := annalsmcp.Handler(s.store, s.cfg.Version)
	mux.Handle("/mcp", s.sender(func(w http.ResponseWriter, r *http.Request, tok store.Token) {
		ctx := annalsmcp.WithLevel(r.Context(), tok.Level())
		ctx = annalsmcp.WithShown(ctx, func() { s.tokenShownPeople(r, tok, selfaudit.DoorMCP) })
		mcp.ServeHTTP(w, r.WithContext(ctx))
	}))
	return telemetry.Handler(mux, "api")
}

// tokenShownPeople keeps a people.view record when a token that reads
// people was just shown them.
func (s *Server) tokenShownPeople(r *http.Request, tok store.Token, door string) {
	if !tok.Level().SeesPeople() {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "people shown", r, "level", tok.Level().String(), "token_id", tok.ID, "door", door)
	s.audit.PeopleViewed(r.Context(), selfaudit.TokenViewer(tok), tok.Level(), door)
}

// apiWhoami answers with the caller's token. Who issued it is a person, so a
// reader token sees them as a ref.
func (s *Server) apiWhoami(w http.ResponseWriter, r *http.Request, tok store.Token) {
	if tok.Level() == people.Reader {
		tok.CreatedBy = s.store.Masker().Ref(tok.CreatedBy)
	} else {
		s.tokenShownPeople(r, tok, selfaudit.DoorAPI)
	}
	writeJSON(w, http.StatusOK, tok)
}

type senderHandler func(http.ResponseWriter, *http.Request, store.Token)

// sender admits a request that carries a live token, and nothing else.
func (s *Server) sender(next senderHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			s.logRequest(r.Context(), slog.LevelWarn, "sender refused: no bearer token", r)
			writeError(w, http.StatusUnauthorized, "a bearer token is required", "no_token")
			return
		}
		tok, ok := s.store.Verify(r.Context(), strings.TrimSpace(secret))
		if !ok {
			s.logRequest(r.Context(), slog.LevelWarn, "sender refused: unknown or revoked token", r)
			writeError(w, http.StatusUnauthorized, "this token is not valid", "bad_token")
			return
		}
		s.logRequest(r.Context(), slog.LevelDebug, "sender admitted", r, "token", tok.Name, "source", tok.Source, "reads", tok.Reads)
		next(w, r, tok)
	})
}

// apiPost stores one event or a batch. A batch answers 200 with a receipt
// per event; a single event answers 201, 200 when it was already stored, or
// 422 when refused.
func (s *Server) apiPost(w http.ResponseWriter, r *http.Request, tok store.Token) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "the body is too large", "too_large")
		return
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []annals.Event
		if err := decode(body, &batch); err != nil {
			s.refusedBody(w, r, tok, err)
			return
		}
		if len(batch) > annals.MaxBatch {
			writeError(w, http.StatusRequestEntityTooLarge, "a batch holds at most 1000 events", "too_many")
			return
		}
		receipts := s.Ingest(r.Context(), batch, tok.Source)
		stored := 0
		for _, rc := range receipts {
			if rc.Status == "stored" {
				stored++
			}
		}
		s.logRequest(r.Context(), slog.LevelInfo, "batch received", r, "source", tok.Source, "events", len(batch), "stored", stored)
		writeJSON(w, http.StatusOK, map[string]any{"results": receipts})
		return
	}
	var ev annals.Event
	if err := decode(body, &ev); err != nil {
		s.refusedBody(w, r, tok, err)
		return
	}
	rc := s.Ingest(r.Context(), []annals.Event{ev}, tok.Source)[0]
	rc.Index = 0
	switch rc.Status {
	case "stored":
		w.Header().Set("Location", "/v1/events/"+rc.ID)
		writeJSON(w, http.StatusCreated, rc)
	case "duplicate":
		writeJSON(w, http.StatusOK, rc)
	case "refused":
		writeJSON(w, http.StatusUnprocessableEntity, rc)
	default:
		writeJSON(w, http.StatusInternalServerError, rc)
	}
}

func decode(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after the JSON")
	}
	return nil
}

func (s *Server) refusedBody(w http.ResponseWriter, r *http.Request, tok store.Token, err error) {
	s.metrics.Refused(r.Context(), "bad_json")
	s.logRequest(r.Context(), slog.LevelWarn, "event refused: bad json", r, "source", tok.Source, "error", err.Error())
	writeError(w, http.StatusBadRequest, "the body must be one event or an array of events, JSON, with no unknown fields: "+err.Error(), "bad_json")
}

func (s *Server) apiList(w http.ResponseWriter, r *http.Request, tok store.Token) {
	f, err := parseFilter(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "bad_filter")
		return
	}
	v := s.viewAt(tok.Level())
	page, err := s.store.Query(r.Context(), v.filter(f))
	if err != nil {
		s.logRequest(r.Context(), slog.LevelError, "query failed", r, "error", err)
		writeError(w, http.StatusInternalServerError, "the events could not be read", "store_error")
		return
	}
	page.Events = v.events(page.Events)
	s.tokenShownPeople(r, tok, selfaudit.DoorAPI)
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) apiGet(w http.ResponseWriter, r *http.Request, tok store.Token) {
	ev, err := s.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such event", "not_found")
		return
	}
	if err != nil {
		s.logRequest(r.Context(), slog.LevelError, "get failed", r, "error", err)
		writeError(w, http.StatusInternalServerError, "the event could not be read", "store_error")
		return
	}
	s.tokenShownPeople(r, tok, selfaudit.DoorAPI)
	writeJSON(w, http.StatusOK, s.viewAt(tok.Level()).event(ev))
}

func (s *Server) apiStats(w http.ResponseWriter, r *http.Request, _ store.Token) {
	st, err := s.store.Stats(r.Context())
	if err != nil {
		s.logRequest(r.Context(), slog.LevelError, "stats failed", r, "error", err)
		writeError(w, http.StatusInternalServerError, "the stats could not be read", "store_error")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// proofParam is a slug: an effect kind, a blueprint, a path, or a source.
var proofParam = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,255}$`)

// apiProof answers how many changes of one key the watch saw, and what it
// saw, from the proof table alone.
func (s *Server) apiProof(w http.ResponseWriter, r *http.Request, _ store.Token) {
	q := r.URL.Query()
	k := store.ProofKey{Kind: q.Get("kind"), Blueprint: q.Get("blueprint"), Path: q.Get("path"), Source: q.Get("source")}
	for _, p := range []struct{ name, v string }{{"kind", k.Kind}, {"blueprint", k.Blueprint}, {"path", k.Path}} {
		if !proofParam.MatchString(p.v) {
			writeError(w, http.StatusBadRequest, p.name+" is required, a lowercase slug", "bad_filter")
			return
		}
	}
	if k.Source != "" && !proofParam.MatchString(k.Source) {
		writeError(w, http.StatusBadRequest, "source must be a lowercase slug", "bad_filter")
		return
	}
	p, err := s.store.Proof(r.Context(), k)
	if err != nil {
		s.logRequest(r.Context(), slog.LevelError, "proof failed", r, "error", err)
		writeError(w, http.StatusInternalServerError, "the proof could not be read", "store_error")
		return
	}
	writeJSON(w, http.StatusOK, p)
}
