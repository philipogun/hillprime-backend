package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/hillprime/api/internal/config"
	"github.com/hillprime/api/internal/models"
	"github.com/hillprime/api/internal/notify"
	"github.com/hillprime/api/internal/store"
	"github.com/rs/zerolog/log"
)

type Public struct {
	Store    store.Store
	Cfg      *config.Config
	Notifier *notify.Notifier
}

// POST /v1/leads (signed by the Next.js proxy).
func (h *Public) CreateLead(w http.ResponseWriter, r *http.Request) {
	var in models.LeadIntake
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}

	// Honeypot — accept then silently drop.
	if in.Website != "" || in.Company != "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	if err := validateLead(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in = sanitizeLead(in)

	score := scoreLead(&in)
	hot := score >= h.Cfg.HotLeadThreshold
	completeness := completenessPct(&in)

	ip := clientIP(r)
	ua := r.Header.Get("User-Agent")
	country := r.Header.Get("CF-IPCountry") // Cloudflare header

	ins := &store.LeadInsert{
		Name:         nz(in.Name),
		Email:        nz(in.Email),
		Phone:        nz(in.Phone),
		ProjectType:  nz(in.ProjectType),
		Budget:       in.Budget,
		Painpoint:    nz(in.Painpoint),
		Message:      nz(in.Message),
		Timeline:     nz(in.Timeline),
		Score:        score,
		IsHot:        hot,
		Completeness: completeness,
		UTMSource:    nz(in.UTMSource),
		UTMMedium:    nz(in.UTMMedium),
		UTMCampaign:  nz(in.UTMCampaign),
		Referrer:     nz(in.Referrer),
		LandingPage:  nz(in.LandingPage),
		PagesViewed:  in.PagesViewed,
		SessionID:    nz(in.SessionID),
		IP:           nz(ip),
		UserAgent:    nz(ua),
		Country:      nz(country),
		Timezone:     nz(in.Timezone),
	}

	lead, err := h.Store.InsertLead(r.Context(), ins)
	if err != nil {
		log.Error().Err(err).Msg("insert lead")
		writeErr(w, http.StatusInternalServerError, "could not save lead")
		return
	}

	// Notify in background. We never block the website's POST on email/
	// Telegram — if both providers are down, the lead is still saved.
	go func(lead *store.Lead) {
		bg, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		h.Notifier.Notify(bg, lead)
		_ = h.Store.MarkLeadNotified(bg, lead.ID)
	}(lead)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"id":     lead.ID,
		"score":  lead.Score,
		"is_hot": lead.IsHot,
	})
}

// POST /v1/events
func (h *Public) CreateEvent(w http.ResponseWriter, r *http.Request) {
	var in models.EventIntake
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.SessionID == "" || in.Kind == "" {
		writeErr(w, http.StatusBadRequest, "session_id and kind required")
		return
	}
	if !validEventKind(in.Kind) {
		writeErr(w, http.StatusBadRequest, "invalid event kind")
		return
	}

	props, _ := json.Marshal(in.Properties)
	if props == nil || len(props) == 0 {
		props = []byte("{}")
	}

	ev := &store.Event{
		SessionID:  in.SessionID,
		Kind:       in.Kind,
		Path:       nz(in.Path),
		Referrer:   nz(in.Referrer),
		CTAID:      nz(in.CTAID),
		ScrollPct:  in.ScrollPct,
		DwellMS:    in.DwellMS,
		Properties: props,
		IP:         nz(clientIP(r)),
		UserAgent:  nz(r.Header.Get("User-Agent")),
		Country:    nz(r.Header.Get("CF-IPCountry")),
	}
	if in.LeadID != "" {
		ev.LeadID = &in.LeadID
	}
	if err := h.Store.InsertEvent(r.Context(), ev); err != nil {
		log.Error().Err(err).Msg("insert event")
		writeErr(w, http.StatusInternalServerError, "could not save event")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /v1/subscribe
func (h *Public) Subscribe(w http.ResponseWriter, r *http.Request) {
	var in models.SubscribeIntake
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if _, err := mail.ParseAddress(in.Email); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid email")
		return
	}
	if err := h.Store.UpsertSubscriber(r.Context(), &store.Subscriber{
		Email:  in.Email,
		Source: nz(in.Source),
	}); err != nil {
		log.Error().Err(err).Msg("upsert subscriber")
		writeErr(w, http.StatusInternalServerError, "could not subscribe")
		return
	}
	go func(email string) {
		bg, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := h.Notifier.ConfirmSubscriber(bg, email); err != nil {
			log.Warn().Err(err).Msg("subscriber welcome failed")
		}
	}(in.Email)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- validation / scoring -------------------------------------------

func validateLead(in *models.LeadIntake) error {
	if in.Email == "" && in.Message == "" && in.Painpoint == "" {
		return errors.New("provide at least email or message or painpoint")
	}
	if in.Email != "" {
		if _, err := mail.ParseAddress(in.Email); err != nil {
			return errors.New("invalid email")
		}
	}
	if len(in.Name) > 200 || len(in.Message) > 5000 || len(in.Painpoint) > 2000 {
		return errors.New("field too long")
	}
	if reBadTag.MatchString(in.Message + in.Painpoint + in.Name) {
		return errors.New("invalid characters")
	}
	if in.ProjectType != "" && !oneOf(in.ProjectType, "product", "consulting", "partnership", "general") {
		return errors.New("invalid projectType")
	}
	if in.Budget != "" && !oneOf(in.Budget, "undisclosed", "small", "medium", "large", "enterprise") {
		return errors.New("invalid budget")
	}
	return nil
}

var reBadTag = regexp.MustCompile(`(?i)<script|javascript:|onerror=|onload=`)

func sanitizeLead(in models.LeadIntake) models.LeadIntake {
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.Name = strings.TrimSpace(in.Name)
	in.Message = strings.TrimSpace(in.Message)
	in.Painpoint = strings.TrimSpace(in.Painpoint)
	return in
}

// scoreLead returns 0-100 based on budget / painpoint quality / engagement / source.
func scoreLead(in *models.LeadIntake) int {
	s := 0
	switch strings.ToLower(in.Budget) {
	case "enterprise":
		s += 40
	case "large":
		s += 30
	case "medium":
		s += 20
	case "small":
		s += 10
	}
	pp := strings.ToLower(in.Painpoint + " " + in.Message)
	if len(pp) > 200 {
		s += 10
	} else if len(pp) > 60 {
		s += 5
	}
	for _, kw := range []string{"urgent", "asap", "deadline", "losing", "paying", "revenue", "production", "enterprise", "scale"} {
		if strings.Contains(pp, kw) {
			s += 2
		}
		if s > 20 {
			break
		}
	}
	if in.Email != "" {
		domain := ""
		if at := strings.LastIndex(in.Email, "@"); at >= 0 {
			domain = in.Email[at+1:]
		}
		if !isFreemail(domain) && domain != "" {
			s += 10
		} else {
			s += 3
		}
	}
	if in.PagesViewed >= 5 {
		s += 15
	} else if in.PagesViewed >= 3 {
		s += 10
	} else if in.PagesViewed >= 2 {
		s += 5
	}
	if in.UTMCampaign != "" {
		s += 5
	}
	if in.UTMSource != "" && in.UTMSource != "direct" {
		s += 5
	}
	if s > 100 {
		s = 100
	}
	return s
}

func completenessPct(in *models.LeadIntake) int {
	n := 0
	fields := []string{in.Name, in.Email, in.Phone, in.ProjectType, in.Budget, in.Painpoint, in.Message, in.Timeline}
	for _, f := range fields {
		if strings.TrimSpace(f) != "" {
			n++
		}
	}
	return (n * 100) / len(fields)
}

func isFreemail(domain string) bool {
	switch strings.ToLower(domain) {
	case "gmail.com", "yahoo.com", "yahoo.co.uk", "hotmail.com", "outlook.com",
		"icloud.com", "live.com", "aol.com", "protonmail.com", "proton.me",
		"mail.com", "gmx.com":
		return true
	}
	return false
}

var validKinds = map[string]bool{
	"page_view": true, "cta_click": true, "scroll_depth": true,
	"form_start": true, "form_abandon": true, "form_submit": true,
	"exit_intent": true, "painpoint_submit": true,
	"pricing_view": true, "demo_request": true,
}

func validEventKind(k string) bool { return validKinds[k] }
