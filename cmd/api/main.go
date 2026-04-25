// HillPrime API
//
// Wire-up order (important):
//  1. Load config from env.
//  2. Open Postgres pool.
//  3. Run migrations (idempotent).
//  4. Spin up the realtime hub + LISTEN loop.
//  5. Mount HTTP routes.
//  6. Start server + block on SIGINT/SIGTERM.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/hillprime/api/internal/auth"
	"github.com/hillprime/api/internal/config"
	"github.com/hillprime/api/internal/handlers"
	"github.com/hillprime/api/internal/notify"
	"github.com/hillprime/api/internal/realtime"
	"github.com/hillprime/api/internal/store"
	"github.com/hillprime/api/internal/store/postgres"
	migrations "github.com/hillprime/api/migrations"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	_ = godotenv.Load()
	zerolog.TimeFieldFormat = time.RFC3339

	cfg, err := config.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("config load")
	}
	if cfg.Env == "production" {
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	} else {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- Storage -----------------------------------------------------
	pg, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal().Err(err).Msg("db open")
	}
	defer pg.Close()

	if err := runMigrations(ctx, pg); err != nil {
		log.Fatal().Err(err).Msg("migrations")
	}

	var s store.Store = pg

	// ---- Realtime ----------------------------------------------------
	hub := realtime.NewHub()
	go realtime.Run(ctx, pg.Pool(), "lead_events", hub)

	// ---- Services ----------------------------------------------------
	notifier := notify.New(cfg)

	pub := &handlers.Public{Store: s, Cfg: cfg, Notifier: notifier}
	adm := &handlers.Admin{Store: s, Hub: hub}
	ah := &handlers.Auth{Cfg: cfg, Store: s, Notifier: notifier}

	cookieVerifier := &auth.CookieSession{Store: s}

	// ---- Router ------------------------------------------------------
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(25 * time.Second))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   cfg.AllowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", auth.InternalSignatureHeader, "X-Request-Id"},
		ExposedHeaders:   []string{"X-Request-Id"},
		AllowCredentials: true, // needed so cookies flow cross-origin from /admin
		MaxAge:           300,
	}))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := pg.Ping(r.Context()); err != nil {
			http.Error(w, "db down", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})

	// ---- /auth (session cookies) -------------------------------------
	r.Route("/auth", func(r chi.Router) {
		r.Post("/magic-link", ah.MagicLink)
		r.Get("/callback", ah.Callback)
		r.Post("/logout", ah.Logout)

		r.Group(func(r chi.Router) {
			r.Use(auth.Middleware(cookieVerifier))
			r.Get("/me", ah.Me)
		})
	})

	// ---- /v1 (public intake — HMAC) ----------------------------------
	r.Route("/v1", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(auth.InternalHMAC(cfg.InternalHMACKey))
			r.Post("/leads", pub.CreateLead)
			r.Post("/events", pub.CreateEvent)
			r.Post("/subscribe", pub.Subscribe)
		})

		// ---- /v1/admin (session-authenticated + role=admin) ----------
		r.Route("/admin", func(r chi.Router) {
			r.Use(auth.Middleware(cookieVerifier))
			r.Use(auth.RequireRole("admin"))
			r.Get("/leads", adm.ListLeads)
			r.Get("/leads/{id}", adm.GetLead)
			r.Patch("/leads/{id}", adm.UpdateLead)
			r.Get("/stats", adm.Stats)
			r.Get("/stream", adm.Stream)
		})
	})

	// ---- HTTP server -------------------------------------------------
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0, // SSE needs an unbounded write timeout
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Info().Str("addr", srv.Addr).Str("env", cfg.Env).Msg("api listening")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal().Err(err).Msg("server")
		}
	}()

	// ---- Session GC --------------------------------------------------
	go func() {
		t := time.NewTicker(1 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n, err := s.PurgeExpiredSessions(ctx)
				if err != nil {
					log.Error().Err(err).Msg("purge sessions")
				} else if n > 0 {
					log.Info().Int64("n", n).Msg("purged expired sessions")
				}
			}
		}
	}()

	<-ctx.Done()
	log.Info().Msg("shutdown initiated")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Error().Err(err).Msg("shutdown")
	}
	log.Info().Msg("bye")
}

// runMigrations executes every SQL file in migrations/ in lexical order.
// It uses a trivial marker table; it does NOT track partial failures. If
// a migration errors mid-way, you must fix the SQL and re-run. For a
// single-service API this is exactly what you want — simpler and safer
// than tracking checksums you won't remember to update.
func runMigrations(ctx context.Context, pg *postgres.DB) error {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	pool := pg.Pool()
	if _, err := pool.Exec(ctx, `
		create table if not exists _migrations (
			name text primary key,
			applied_at timestamptz not null default now()
		)`); err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		var exists bool
		if err := pool.QueryRow(ctx,
			`select exists(select 1 from _migrations where name = $1)`, e.Name()).
			Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sql, err := migrations.FS.ReadFile(e.Name())
		if err != nil {
			return err
		}
		log.Info().Str("migration", e.Name()).Msg("applying")
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply %s: %w", e.Name(), err)
		}
		if _, err := pool.Exec(ctx,
			`insert into _migrations (name) values ($1)`, e.Name()); err != nil {
			return err
		}
	}
	return nil
}
