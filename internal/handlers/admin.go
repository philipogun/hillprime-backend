package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hillprime/api/internal/auth"
	"github.com/hillprime/api/internal/models"
	"github.com/hillprime/api/internal/realtime"
	"github.com/hillprime/api/internal/store"
	"github.com/rs/zerolog/log"
)

type Admin struct {
	Store store.Store
	Hub   *realtime.Hub
}

// GET /v1/admin/leads?status=&q=&limit=&offset=
func (h *Admin) ListLeads(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	leads, err := h.Store.ListLeads(r.Context(), store.LeadFilter{
		Status: q.Get("status"),
		Query:  strings.TrimSpace(q.Get("q")),
		Limit:  qInt(q.Get("limit"), 50),
		Offset: qInt(q.Get("offset"), 0),
	})
	if err != nil {
		log.Error().Err(err).Msg("list leads")
		writeErr(w, http.StatusInternalServerError, "query failed")
		return
	}
	resp := make([]models.LeadResponse, 0, len(leads))
	for i := range leads {
		resp = append(resp, toLeadResponse(&leads[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"leads": resp})
}

// GET /v1/admin/leads/:id
func (h *Admin) GetLead(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	lead, err := h.Store.GetLead(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed")
		return
	}
	notes, _ := h.Store.ListNotes(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{
		"lead":  toLeadResponse(lead),
		"notes": notes,
	})
}

// PATCH /v1/admin/leads/:id
func (h *Admin) UpdateLead(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var in models.LeadUpdate
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.Status != nil {
		if !oneOf(*in.Status, "new", "contacted", "qualified", "won", "lost", "spam") {
			writeErr(w, http.StatusBadRequest, "invalid status")
			return
		}
		if err := h.Store.UpdateLeadStatus(r.Context(), id, *in.Status); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "lead not found")
				return
			}
			writeErr(w, http.StatusInternalServerError, "update failed")
			return
		}
	}
	if in.Note != nil && strings.TrimSpace(*in.Note) != "" {
		authorID := ""
		if u := auth.UserFromCtx(r.Context()); u != nil {
			authorID = u.ID
		}
		if err := h.Store.AddNote(r.Context(), id, authorID, strings.TrimSpace(*in.Note)); err != nil {
			writeErr(w, http.StatusInternalServerError, "note failed")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// GET /v1/admin/stats
func (h *Admin) Stats(w http.ResponseWriter, r *http.Request) {
	s, err := h.Store.ComputeStats(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("stats")
		writeErr(w, http.StatusInternalServerError, "stats failed")
		return
	}
	resp := models.StatsResponse{
		LeadsTotal:     s.LeadsTotal,
		LeadsHot:       s.LeadsHot,
		Leads7d:        s.Leads7d,
		Leads30d:       s.Leads30d,
		ConversionRate: s.ConversionRate,
		ByStatus:       s.ByStatus,
		ByBudget:       s.ByBudget,
		ByCampaign:     s.ByCampaign,
	}
	for _, p := range s.TopPainpoints {
		resp.TopPainpoints = append(resp.TopPainpoints, models.PainpointStat{
			Text: p.Text, Count: p.Count,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// GET /v1/admin/stream — Server-Sent Events.
// Replaces Supabase Realtime. Broadcasts every NOTIFY payload on
// 'lead_events' to connected admin clients. EventSource API on the browser
// reconnects automatically on drops.
func (h *Admin) Stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx

	sub, cancel := h.Hub.Subscribe()
	defer cancel()

	// Send a hello so the client knows the connection is live.
	fmt.Fprint(w, "event: hello\ndata: {\"ok\":true}\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			// Comments are ignored by EventSource but keep the socket warm
			// through proxies that drop idle connections.
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case payload, ok := <-sub:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: lead\ndata: %s\n\n", payload)
			flusher.Flush()
		}
	}
}

// ---- mappers ---------------------------------------------------------

func toLeadResponse(l *store.Lead) models.LeadResponse {
	return models.LeadResponse{
		ID:           l.ID,
		CreatedAt:    l.CreatedAt,
		UpdatedAt:    l.UpdatedAt,
		Name:         l.Name,
		Email:        l.Email,
		Phone:        l.Phone,
		ProjectType:  l.ProjectType,
		Budget:       l.Budget,
		Painpoint:    l.Painpoint,
		Message:      l.Message,
		Timeline:     l.Timeline,
		Score:        l.Score,
		IsHot:        l.IsHot,
		UTMSource:    l.UTMSource,
		UTMCampaign:  l.UTMCampaign,
		Referrer:     l.Referrer,
		LandingPage:  l.LandingPage,
		PagesViewed:  l.PagesViewed,
		Country:      l.Country,
		Status:       l.Status,
		Completeness: l.Completeness,
		NotifiedAt:   l.NotifiedAt,
	}
}
