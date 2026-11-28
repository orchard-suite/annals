package server

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/orchard-suite/annals/internal/gate"
	"github.com/orchard-suite/annals/internal/store"
)

// defaultWeeks is how many weeks a gate read covers when it names none.
const defaultWeeks = 12

// apiGateFamilies answers the gate's four numbers for each week and effect
// family, each with its coverage, as a table.
func (s *Server) apiGateFamilies(w http.ResponseWriter, r *http.Request, tok store.Token) {
	fams, _, ok := s.gateScore(w, r, tok)
	if ok {
		writeJSON(w, http.StatusOK, gate.FamilyTable(fams))
	}
}

// apiGateDrafts answers each draft pack's replay for each week as a table.
func (s *Server) apiGateDrafts(w http.ResponseWriter, r *http.Request, tok store.Token) {
	_, drafts, ok := s.gateScore(w, r, tok)
	if ok {
		writeJSON(w, http.StatusOK, gate.DraftTable(drafts))
	}
}

// gateScore scores the weeks the request asks for, from the change events
// alone. It answers counts, never a record, so it shows no person.
func (s *Server) gateScore(w http.ResponseWriter, r *http.Request, tok store.Token) ([]gate.Family, []gate.Draft, bool) {
	weeks := defaultWeeks
	if v := r.URL.Query().Get("weeks"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > gate.MaxWeeks {
			writeError(w, http.StatusBadRequest, "weeks must be a whole number from 1 to 52", "bad_filter")
			return nil, nil, false
		}
		weeks = n
	}
	now := s.now().UTC()
	since := gate.Since(now, weeks)
	evs, err := s.store.Changes(r.Context(), gate.Verbs, since)
	if err != nil {
		s.logRequest(r.Context(), slog.LevelError, "gate score failed", r, "error", err)
		writeError(w, http.StatusInternalServerError, "the change events could not be read", "store_error")
		return nil, nil, false
	}
	fams, drafts := gate.Score(evs, since, now, s.cfg.Gate)
	s.logRequest(r.Context(), slog.LevelInfo, "gate scored", r, "source", tok.Source, "weeks", weeks, "events", len(evs), "families", len(fams), "drafts", len(drafts))
	return fams, drafts, true
}
