// Package postgres implements store.Store against any Postgres 14+.
// Tested against: Supabase, Neon, Fly Postgres, vanilla postgres:16 in
// Docker. No Postgres-specific-flavor features used.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hillprime/api/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps a pgxpool and satisfies store.Store.
type DB struct {
	pool *pgxpool.Pool
}

// Compile-time check: *DB satisfies store.Store. If this fails to compile,
// add or fix the missing method before debugging anything else.
var _ store.Store = (*DB)(nil)

// Open creates a pooled connection against the given URL.
//
// The URL format is standard libpq. On Supabase, use the "Transaction mode"
// pooled URL (port 6543). On Neon, use the pooled endpoint. On self-hosted,
// point directly at 5432. Nothing else in the app cares.
//
//	postgres://user:pass@host:port/dbname?sslmode=require
func Open(ctx context.Context, url string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse db url: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgxpool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db ping: %w", err)
	}
	return &DB{pool: pool}, nil
}

func (d *DB) Close() { d.pool.Close() }

func (d *DB) Ping(ctx context.Context) error { return d.pool.Ping(ctx) }

// Pool exposes the raw pool for the realtime listener. Everything else
// goes through the interface methods.
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// translateErr maps pgx errors to store sentinel errors so handlers don't
// need to import pgx.
func translateErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	// 23505 is unique_violation. We could check with &pgconn.PgError, but
	// string-matching on the SQLSTATE is less brittle across driver versions.
	if strings.Contains(err.Error(), "23505") {
		return store.ErrConflict
	}
	return err
}

// ---- Users -----------------------------------------------------------

func (d *DB) GetUserByEmail(ctx context.Context, email string) (*store.User, error) {
	var u store.User
	err := d.pool.QueryRow(ctx, `
		select id, created_at, updated_at, email, email_verified_at, password_hash,
		       role::text, name, last_login_at, disabled
		from users
		where lower(email) = lower($1)`, email).
		Scan(&u.ID, &u.CreatedAt, &u.UpdatedAt, &u.Email, &u.EmailVerifiedAt,
			&u.PasswordHash, &u.Role, &u.Name, &u.LastLoginAt, &u.Disabled)
	if err != nil {
		return nil, translateErr(err)
	}
	return &u, nil
}

func (d *DB) GetUserByID(ctx context.Context, id string) (*store.User, error) {
	var u store.User
	err := d.pool.QueryRow(ctx, `
		select id, created_at, updated_at, email, email_verified_at, password_hash,
		       role::text, name, last_login_at, disabled
		from users where id = $1`, id).
		Scan(&u.ID, &u.CreatedAt, &u.UpdatedAt, &u.Email, &u.EmailVerifiedAt,
			&u.PasswordHash, &u.Role, &u.Name, &u.LastLoginAt, &u.Disabled)
	if err != nil {
		return nil, translateErr(err)
	}
	return &u, nil
}

func (d *DB) CreateUser(ctx context.Context, u *store.User) (*store.User, error) {
	var created store.User
	err := d.pool.QueryRow(ctx, `
		insert into users (email, password_hash, role, name, email_verified_at)
		values ($1, $2, $3::user_role, $4, $5)
		returning id, created_at, updated_at, email, email_verified_at, password_hash,
		          role::text, name, last_login_at, disabled`,
		u.Email, u.PasswordHash, u.Role, u.Name, u.EmailVerifiedAt).
		Scan(&created.ID, &created.CreatedAt, &created.UpdatedAt, &created.Email,
			&created.EmailVerifiedAt, &created.PasswordHash, &created.Role,
			&created.Name, &created.LastLoginAt, &created.Disabled)
	if err != nil {
		return nil, translateErr(err)
	}
	return &created, nil
}

func (d *DB) TouchUserLogin(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx, `update users set last_login_at = now() where id = $1`, id)
	return translateErr(err)
}

// ---- Sessions --------------------------------------------------------

func (d *DB) CreateSession(ctx context.Context, s *store.Session) error {
	err := d.pool.QueryRow(ctx, `
		insert into sessions (user_id, token_hash, expires_at, user_agent, ip)
		values ($1, $2, $3, $4, $5)
		returning id, created_at, last_used_at`,
		s.UserID, s.TokenHash, s.ExpiresAt, s.UserAgent, s.IP).
		Scan(&s.ID, &s.CreatedAt, &s.LastUsedAt)
	return translateErr(err)
}

func (d *DB) GetSessionByTokenHash(ctx context.Context, tokenHash string) (*store.Session, error) {
	var s store.Session
	err := d.pool.QueryRow(ctx, `
		select id, user_id, token_hash, created_at, last_used_at, expires_at,
		       user_agent, ip, revoked_at
		from sessions
		where token_hash = $1 and revoked_at is null and expires_at > now()`,
		tokenHash).
		Scan(&s.ID, &s.UserID, &s.TokenHash, &s.CreatedAt, &s.LastUsedAt,
			&s.ExpiresAt, &s.UserAgent, &s.IP, &s.RevokedAt)
	if err != nil {
		return nil, translateErr(err)
	}
	return &s, nil
}

func (d *DB) TouchSession(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx, `update sessions set last_used_at = now() where id = $1`, id)
	return translateErr(err)
}

func (d *DB) RevokeSession(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx, `update sessions set revoked_at = now() where id = $1`, id)
	return translateErr(err)
}

func (d *DB) RevokeAllUserSessions(ctx context.Context, userID string) error {
	_, err := d.pool.Exec(ctx,
		`update sessions set revoked_at = now() where user_id = $1 and revoked_at is null`, userID)
	return translateErr(err)
}

func (d *DB) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := d.pool.Exec(ctx,
		`delete from sessions where expires_at < now() - interval '7 days'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ---- AuthTokens ------------------------------------------------------

func (d *DB) CreateAuthToken(ctx context.Context, t *store.AuthToken) error {
	return translateErr(d.pool.QueryRow(ctx, `
		insert into auth_tokens (user_id, purpose, token_hash, expires_at)
		values ($1, $2, $3, $4)
		returning id, created_at`,
		t.UserID, t.Purpose, t.TokenHash, t.ExpiresAt).
		Scan(&t.ID, &t.CreatedAt))
}

// ConsumeAuthToken atomically marks the token used if it's unused and unexpired.
// This is the critical single-use guarantee for magic links.
func (d *DB) ConsumeAuthToken(ctx context.Context, tokenHash, purpose string) (*store.AuthToken, error) {
	var t store.AuthToken
	err := d.pool.QueryRow(ctx, `
		update auth_tokens
		set used_at = now()
		where token_hash = $1
		  and purpose = $2
		  and used_at is null
		  and expires_at > now()
		returning id, user_id, purpose, token_hash, expires_at, used_at, created_at`,
		tokenHash, purpose).
		Scan(&t.ID, &t.UserID, &t.Purpose, &t.TokenHash, &t.ExpiresAt, &t.UsedAt, &t.CreatedAt)
	if err != nil {
		return nil, translateErr(err)
	}
	return &t, nil
}

// ---- Leads -----------------------------------------------------------

func (d *DB) InsertLead(ctx context.Context, in *store.LeadInsert) (*store.Lead, error) {
	var l store.Lead
	err := d.pool.QueryRow(ctx, `
		insert into leads (
			name, email, phone, project_type, budget, painpoint, message, timeline,
			score, is_hot, utm_source, utm_medium, utm_campaign, referrer,
			landing_page, pages_viewed, session_id, ip, user_agent, country,
			timezone, completeness
		)
		values (
			$1, $2, $3,
			nullif($4::text, '')::project_type,
			coalesce(nullif($5::text,'')::budget_band, 'undisclosed'),
			$6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22
		)
		returning id, created_at, updated_at, name, email, phone,
		          project_type::text, budget::text, painpoint, message, timeline,
		          score, is_hot, utm_source, utm_medium, utm_campaign, referrer,
		          landing_page, pages_viewed, session_id, ip, user_agent, country,
		          timezone, status::text, completeness, notified_at`,
		in.Name, in.Email, in.Phone,
		strOrEmpty(in.ProjectType), in.Budget,
		in.Painpoint, in.Message, in.Timeline,
		in.Score, in.IsHot,
		in.UTMSource, in.UTMMedium, in.UTMCampaign, in.Referrer,
		in.LandingPage, maxOne(in.PagesViewed), in.SessionID,
		in.IP, in.UserAgent, in.Country, in.Timezone, in.Completeness).
		Scan(
			&l.ID, &l.CreatedAt, &l.UpdatedAt,
			&l.Name, &l.Email, &l.Phone,
			&l.ProjectType, &l.Budget,
			&l.Painpoint, &l.Message, &l.Timeline,
			&l.Score, &l.IsHot,
			&l.UTMSource, &l.UTMMedium, &l.UTMCampaign, &l.Referrer,
			&l.LandingPage, &l.PagesViewed, &l.SessionID,
			&l.IP, &l.UserAgent, &l.Country, &l.Timezone,
			&l.Status, &l.Completeness, &l.NotifiedAt,
		)
	if err != nil {
		return nil, translateErr(err)
	}
	return &l, nil
}

func (d *DB) GetLead(ctx context.Context, id string) (*store.Lead, error) {
	var l store.Lead
	err := d.pool.QueryRow(ctx, selectLeadSQL+` where id = $1`, id).Scan(leadScanArgs(&l)...)
	if err != nil {
		return nil, translateErr(err)
	}
	return &l, nil
}

func (d *DB) ListLeads(ctx context.Context, f store.LeadFilter) ([]store.Lead, error) {
	args := []any{}
	where := []string{}
	if f.Status != "" {
		args = append(args, f.Status)
		where = append(where, fmt.Sprintf("status = $%d::lead_status", len(args)))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, "%"+q+"%")
		i := len(args)
		where = append(where, fmt.Sprintf(
			"(name ilike $%d or email ilike $%d or message ilike $%d or painpoint ilike $%d)",
			i, i, i, i))
	}
	sql := selectLeadSQL
	if len(where) > 0 {
		sql += " where " + strings.Join(where, " and ")
	}
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args = append(args, limit, f.Offset)
	sql += fmt.Sprintf(" order by created_at desc limit $%d offset $%d", len(args)-1, len(args))

	rows, err := d.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.Lead, 0, limit)
	for rows.Next() {
		var l store.Lead
		if err := rows.Scan(leadScanArgs(&l)...); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (d *DB) UpdateLeadStatus(ctx context.Context, id, status string) error {
	tag, err := d.pool.Exec(ctx,
		`update leads set status = $1::lead_status where id = $2`, status, id)
	if err != nil {
		return translateErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (d *DB) MarkLeadNotified(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx,
		`update leads set notified_at = now() where id = $1`, id)
	return translateErr(err)
}

func (d *DB) ComputeStats(ctx context.Context) (*store.Stats, error) {
	s := &store.Stats{
		ByStatus:   map[string]int{},
		ByBudget:   map[string]int{},
		ByCampaign: map[string]int{},
	}

	if err := d.pool.QueryRow(ctx,
		`select count(*), count(*) filter (where is_hot) from leads`).
		Scan(&s.LeadsTotal, &s.LeadsHot); err != nil {
		return nil, err
	}
	if err := d.pool.QueryRow(ctx,
		`select count(*) from leads where created_at >= now() - interval '7 days'`).
		Scan(&s.Leads7d); err != nil {
		return nil, err
	}
	if err := d.pool.QueryRow(ctx,
		`select count(*) from leads where created_at >= now() - interval '30 days'`).
		Scan(&s.Leads30d); err != nil {
		return nil, err
	}

	var won, total int
	if err := d.pool.QueryRow(ctx,
		`select count(*) filter (where status='won'), count(*) from leads`).
		Scan(&won, &total); err == nil && total > 0 {
		s.ConversionRate = float64(won) / float64(total)
	}

	if err := fillMap(ctx, d.pool,
		`select status::text, count(*) from leads group by 1`, s.ByStatus); err != nil {
		return nil, err
	}
	if err := fillMap(ctx, d.pool,
		`select budget::text, count(*) from leads group by 1`, s.ByBudget); err != nil {
		return nil, err
	}
	if err := fillMap(ctx, d.pool,
		`select coalesce(utm_campaign,'direct'), count(*) from leads
		 group by 1 order by 2 desc limit 10`, s.ByCampaign); err != nil {
		return nil, err
	}

	rows, err := d.pool.Query(ctx, `select text, n from admin_painpoint_clusters limit 15`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p store.Painpoint
		if err := rows.Scan(&p.Text, &p.Count); err != nil {
			return nil, err
		}
		s.TopPainpoints = append(s.TopPainpoints, p)
	}
	return s, rows.Err()
}

// ---- Events ---------------------------------------------------------

func (d *DB) InsertEvent(ctx context.Context, e *store.Event) error {
	_, err := d.pool.Exec(ctx, `
		insert into events (
			session_id, lead_id, kind, path, referrer, cta_id, scroll_pct,
			dwell_ms, properties, ip, user_agent, country
		) values (
			$1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12
		)`,
		e.SessionID, e.LeadID, e.Kind, e.Path, e.Referrer, e.CTAID,
		e.ScrollPct, e.DwellMS, string(e.Properties), e.IP, e.UserAgent, e.Country,
	)
	return translateErr(err)
}

// ---- Subscribers ----------------------------------------------------

func (d *DB) UpsertSubscriber(ctx context.Context, s *store.Subscriber) error {
	_, err := d.pool.Exec(ctx, `
		insert into subscribers (email, source) values ($1, $2)
		on conflict (email) do update set unsubscribed = false`,
		s.Email, s.Source)
	return translateErr(err)
}

// ---- Notes ----------------------------------------------------------

func (d *DB) AddNote(ctx context.Context, leadID, authorID, body string) error {
	var auth any
	if authorID != "" {
		auth = authorID
	}
	_, err := d.pool.Exec(ctx,
		`insert into lead_notes (lead_id, author_id, body) values ($1, $2, $3)`,
		leadID, auth, body)
	return translateErr(err)
}

func (d *DB) ListNotes(ctx context.Context, leadID string) ([]store.LeadNote, error) {
	rows, err := d.pool.Query(ctx,
		`select id, created_at, lead_id, author_id, body
		 from lead_notes where lead_id = $1 order by created_at desc`, leadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.LeadNote, 0)
	for rows.Next() {
		var n store.LeadNote
		if err := rows.Scan(&n.ID, &n.CreatedAt, &n.LeadID, &n.AuthorID, &n.Body); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---- helpers ---------------------------------------------------------

const selectLeadSQL = `
select id, created_at, updated_at, name, email, phone,
       project_type::text, budget::text, painpoint, message, timeline,
       score, is_hot, utm_source, utm_medium, utm_campaign, referrer,
       landing_page, pages_viewed, session_id, ip, user_agent, country,
       timezone, status::text, completeness, notified_at
from leads`

func leadScanArgs(l *store.Lead) []any {
	return []any{
		&l.ID, &l.CreatedAt, &l.UpdatedAt,
		&l.Name, &l.Email, &l.Phone,
		&l.ProjectType, &l.Budget,
		&l.Painpoint, &l.Message, &l.Timeline,
		&l.Score, &l.IsHot,
		&l.UTMSource, &l.UTMMedium, &l.UTMCampaign, &l.Referrer,
		&l.LandingPage, &l.PagesViewed, &l.SessionID,
		&l.IP, &l.UserAgent, &l.Country, &l.Timezone,
		&l.Status, &l.Completeness, &l.NotifiedAt,
	}
}

func fillMap(ctx context.Context, pool *pgxpool.Pool, sql string, out map[string]int) error {
	rows, err := pool.Query(ctx, sql)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return err
		}
		out[k] = n
	}
	return rows.Err()
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func maxOne(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
