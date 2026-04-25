// Package store defines the persistence interface used by the API.
//
// Handlers depend on Store, not on a concrete database. In practice we only
// ship a Postgres implementation (see ./postgres), but the indirection is
// cheap and buys us:
//
//  1. Portability. Every method signature uses standard Go types. Moving
//     from Supabase-hosted Postgres to Neon, RDS, Fly, or self-hosted is
//     a DSN change.
//
//  2. Testability. Handlers can be driven against an in-memory mock without
//     spinning up a database in CI.
//
//  3. Containment. If we ever do need a Supabase-specific feature (e.g.
//     their storage buckets), it lives in a separate sub-interface, not
//     leaking into the core API.
package store

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// ---- Domain types ----------------------------------------------------
//
// These are the *persisted* shapes. Intake DTOs (what the website POSTs)
// live in ../models. We keep them separate so the HTTP layer can evolve
// without forcing schema changes.

type User struct {
	ID              string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Email           string
	EmailVerifiedAt *time.Time
	PasswordHash    *string
	Role            string // "admin" | "viewer"
	Name            *string
	LastLoginAt     *time.Time
	Disabled        bool
}

type Session struct {
	ID         string
	UserID     string
	TokenHash  string
	CreatedAt  time.Time
	LastUsedAt time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	UserAgent  *string
	IP         *string
}

type AuthToken struct {
	ID        string
	UserID    string
	Purpose   string // "magic_link" | "verify_email" | "reset_password"
	TokenHash string
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

type Lead struct {
	ID           string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Name         *string
	Email        *string
	Phone        *string
	ProjectType  *string
	Budget       string
	Painpoint    *string
	Message      *string
	Timeline     *string
	Score        int
	IsHot        bool
	UTMSource    *string
	UTMMedium    *string
	UTMCampaign  *string
	Referrer     *string
	LandingPage  *string
	PagesViewed  int
	SessionID    *string
	IP           *string
	UserAgent    *string
	Country      *string
	Timezone     *string
	Status       string
	Completeness int
	NotifiedAt   *time.Time
}

type LeadInsert struct {
	Name         *string
	Email        *string
	Phone        *string
	ProjectType  *string
	Budget       string
	Painpoint    *string
	Message      *string
	Timeline     *string
	Score        int
	IsHot        bool
	Completeness int
	UTMSource    *string
	UTMMedium    *string
	UTMCampaign  *string
	Referrer     *string
	LandingPage  *string
	PagesViewed  int
	SessionID    *string
	IP           *string
	UserAgent    *string
	Country      *string
	Timezone     *string
}

type LeadFilter struct {
	Status string
	Query  string
	Limit  int
	Offset int
}

type LeadNote struct {
	ID        int64
	CreatedAt time.Time
	LeadID    string
	AuthorID  *string
	Body      string
}

type Event struct {
	SessionID  string
	LeadID     *string
	Kind       string
	Path       *string
	Referrer   *string
	CTAID      *string
	ScrollPct  int
	DwellMS    int
	Properties []byte // JSON
	IP         *string
	UserAgent  *string
	Country    *string
}

type Subscriber struct {
	Email  string
	Source *string
}

type Stats struct {
	LeadsTotal     int
	LeadsHot       int
	Leads7d        int
	Leads30d       int
	ConversionRate float64
	ByStatus       map[string]int
	ByBudget       map[string]int
	ByCampaign     map[string]int
	TopPainpoints  []Painpoint
}

type Painpoint struct {
	Text  string
	Count int
}

// ---- Interfaces ------------------------------------------------------

// Store bundles the sub-interfaces the API needs. Keeping them split makes
// it easy to mock only what a given handler uses.
type Store interface {
	Users
	Sessions
	AuthTokens
	Leads
	Events
	Subscribers
	Notes
	Health

	// Close releases all connections. Safe to call repeatedly.
	Close()
}

type Health interface {
	Ping(ctx context.Context) error
}

type Users interface {
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	GetUserByID(ctx context.Context, id string) (*User, error)
	CreateUser(ctx context.Context, u *User) (*User, error)
	TouchUserLogin(ctx context.Context, id string) error
}

type Sessions interface {
	CreateSession(ctx context.Context, s *Session) error
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error)
	TouchSession(ctx context.Context, id string) error
	RevokeSession(ctx context.Context, id string) error
	RevokeAllUserSessions(ctx context.Context, userID string) error
	PurgeExpiredSessions(ctx context.Context) (int64, error)
}

type AuthTokens interface {
	CreateAuthToken(ctx context.Context, t *AuthToken) error
	ConsumeAuthToken(ctx context.Context, tokenHash, purpose string) (*AuthToken, error)
}

type Leads interface {
	InsertLead(ctx context.Context, in *LeadInsert) (*Lead, error)
	GetLead(ctx context.Context, id string) (*Lead, error)
	ListLeads(ctx context.Context, f LeadFilter) ([]Lead, error)
	UpdateLeadStatus(ctx context.Context, id, status string) error
	MarkLeadNotified(ctx context.Context, id string) error
	ComputeStats(ctx context.Context) (*Stats, error)
}

type Events interface {
	InsertEvent(ctx context.Context, e *Event) error
}

type Subscribers interface {
	UpsertSubscriber(ctx context.Context, s *Subscriber) error
}

type Notes interface {
	AddNote(ctx context.Context, leadID, authorID, body string) error
	ListNotes(ctx context.Context, leadID string) ([]LeadNote, error)
}

// RealtimeSubscriber is the portable LISTEN/NOTIFY plumbing. Any Postgres
// driver that supports LISTEN satisfies this; in our case pgx does.
type RealtimeSubscriber interface {
	// Subscribe blocks until ctx is canceled, calling handler for each
	// notification on the given channel. Handler is called from a dedicated
	// goroutine; it must not block.
	Subscribe(ctx context.Context, channel string, handler func(payload string)) error
}
