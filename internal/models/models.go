// Package models holds the HTTP-facing DTOs. These are distinct from
// store.* domain types so the API can evolve without schema changes.
package models

import "time"

// LeadIntake is POST /v1/leads.
type LeadIntake struct {
	Name        string `json:"name,omitempty"`
	Email       string `json:"email,omitempty"`
	Phone       string `json:"phone,omitempty"`
	ProjectType string `json:"projectType,omitempty"`
	Budget      string `json:"budget,omitempty"`
	Painpoint   string `json:"painpoint,omitempty"`
	Message     string `json:"message,omitempty"`
	Timeline    string `json:"timeline,omitempty"`

	// Honeypots
	Website string `json:"website,omitempty"`
	Company string `json:"company,omitempty"`

	// Attribution
	UTMSource   string `json:"utm_source,omitempty"`
	UTMMedium   string `json:"utm_medium,omitempty"`
	UTMCampaign string `json:"utm_campaign,omitempty"`
	Referrer    string `json:"referrer,omitempty"`
	LandingPage string `json:"landing_page,omitempty"`
	PagesViewed int    `json:"pages_viewed,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Timezone    string `json:"timezone,omitempty"`
}

// EventIntake is POST /v1/events.
type EventIntake struct {
	SessionID  string         `json:"session_id"`
	LeadID     string         `json:"lead_id,omitempty"`
	Kind       string         `json:"kind"`
	Path       string         `json:"path,omitempty"`
	Referrer   string         `json:"referrer,omitempty"`
	CTAID      string         `json:"cta_id,omitempty"`
	ScrollPct  int            `json:"scroll_pct,omitempty"`
	DwellMS    int            `json:"dwell_ms,omitempty"`
	Properties map[string]any `json:"properties,omitempty"`
}

// SubscribeIntake is POST /v1/subscribe.
type SubscribeIntake struct {
	Email  string `json:"email"`
	Source string `json:"source,omitempty"`
}

// MagicLinkRequest is POST /auth/magic-link.
type MagicLinkRequest struct {
	Email string `json:"email"`
}

// LeadResponse is what we expose to admins.
type LeadResponse struct {
	ID           string     `json:"id"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	Name         *string    `json:"name,omitempty"`
	Email        *string    `json:"email,omitempty"`
	Phone        *string    `json:"phone,omitempty"`
	ProjectType  *string    `json:"project_type,omitempty"`
	Budget       string     `json:"budget"`
	Painpoint    *string    `json:"painpoint,omitempty"`
	Message      *string    `json:"message,omitempty"`
	Timeline     *string    `json:"timeline,omitempty"`
	Score        int        `json:"score"`
	IsHot        bool       `json:"is_hot"`
	UTMSource    *string    `json:"utm_source,omitempty"`
	UTMCampaign  *string    `json:"utm_campaign,omitempty"`
	Referrer     *string    `json:"referrer,omitempty"`
	LandingPage  *string    `json:"landing_page,omitempty"`
	PagesViewed  int        `json:"pages_viewed"`
	Country      *string    `json:"country,omitempty"`
	Status       string     `json:"status"`
	Completeness int        `json:"completeness"`
	NotifiedAt   *time.Time `json:"notified_at,omitempty"`
}

// StatsResponse mirrors store.Stats but with JSON tags.
type StatsResponse struct {
	LeadsTotal     int             `json:"leads_total"`
	LeadsHot       int             `json:"leads_hot"`
	Leads7d        int             `json:"leads_7d"`
	Leads30d       int             `json:"leads_30d"`
	ConversionRate float64         `json:"conversion_rate"`
	ByStatus       map[string]int  `json:"by_status"`
	ByBudget       map[string]int  `json:"by_budget"`
	ByCampaign     map[string]int  `json:"by_campaign"`
	TopPainpoints  []PainpointStat `json:"top_painpoints"`
}

type PainpointStat struct {
	Text  string `json:"text"`
	Count int    `json:"count"`
}

// LeadUpdate is PATCH /v1/admin/leads/:id.
type LeadUpdate struct {
	Status *string `json:"status,omitempty"`
	Note   *string `json:"note,omitempty"`
}

// MeResponse is GET /auth/me.
type MeResponse struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
	Name  string `json:"name,omitempty"`
}
