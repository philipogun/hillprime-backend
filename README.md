# HillPrime Backend — v2 (portable Postgres edition)

Production-ready Go API with a hard-drawn seam between the application and
its database host. Starts on Supabase; swaps to Neon / Fly / RDS / self-hosted
with nothing more than a `DATABASE_URL` change.

**Read in this order:**
1. `docs/PORTABILITY.md` — the key architectural doc: how the seam works,
   host-by-host migration procedure, what the tradeoffs are
2. `README.md` (this file) — orientation
3. `.env.example` — configuration surface

---

## What this is

A single Go binary that:

- **Ingests leads** from the public website (via HMAC-signed Next.js proxy)
- **Scores them 0–100** and flags "hot" leads above a threshold
- **Notifies in real time** — concurrent Telegram push + Resend email
- **Streams live updates** to an admin dashboard via Server-Sent Events
  (LISTEN/NOTIFY → SSE, portable across every Postgres host)
- **Authenticates admins** via magic-link email (no third-party IdP required;
  your own `users` table; session cookies; argon2id for passwords if you ever
  add them)
- **Auto-runs migrations** on boot — no separate migrate command, just deploy

## Why v2 ≠ v1

v1 coupled to Supabase in three ways:
- Foreign keys to `auth.users` (Supabase-managed table)
- RLS policies calling `auth.uid()`
- `supabase_realtime` publication
- Frontend used `@supabase/supabase-js` for auth

**v2 removes all four couplings** while keeping Supabase as a fine default:

| v1 | v2 |
|---|---|
| `references auth.users(id)` | Own `users` table |
| Row-Level Security | Go-app-layer role check |
| Supabase Realtime (WebSockets) | LISTEN/NOTIFY → SSE |
| `supabase-js` magic-link | `/auth/magic-link` → Resend |
| Supabase JWT HS256 verifier | Session cookies (server-side rotation) |
| External identity tied to provider | `TokenVerifier` interface, swappable |

The code never imports any Supabase SDK. `grep -r supabase backend/` is empty.

## Project layout

```
backend/
├── cmd/api/main.go              Wires everything; auto-migrates; starts SSE hub
├── migrations/001_init.sql      Portable schema (Postgres 14+)
├── internal/
│   ├── config/                  Env loader
│   ├── store/
│   │   ├── store.go             INTERFACE — everything depends on this
│   │   └── postgres/            The single Postgres implementation
│   ├── auth/                    Session cookies, magic links, argon2, HMAC, TokenVerifier
│   ├── realtime/                LISTEN/NOTIFY hub + SSE fan-out
│   ├── notify/                  Telegram + Resend fan-out
│   ├── handlers/
│   │   ├── auth.go              /auth/magic-link, /auth/callback, /auth/logout, /auth/me
│   │   ├── public.go            /v1/leads, /v1/events, /v1/subscribe
│   │   └── admin.go             /v1/admin/leads, /stats, /stream (SSE)
│   └── models/                  HTTP DTOs
├── Dockerfile                   Distroless, ~15 MB
├── go.mod
└── .env.example
```

## HTTP surface

```
GET  /healthz                         public
POST /auth/magic-link                 public     {email} → 200 always
GET  /auth/callback?token=…           public     → redirect to admin
POST /auth/logout                     public     revokes current session
GET  /auth/me                         cookie     current user

POST /v1/leads                        HMAC       website intake
POST /v1/events                       HMAC       analytics
POST /v1/subscribe                    HMAC       newsletter

GET  /v1/admin/leads                  cookie+admin
GET  /v1/admin/leads/{id}             cookie+admin
PATCH /v1/admin/leads/{id}            cookie+admin
GET  /v1/admin/stats                  cookie+admin
GET  /v1/admin/stream                 cookie+admin   SSE live feed
```

## Get running in 60 seconds (local dev)

```bash
# Postgres in Docker
docker run -d --name hp-db -p 5432:5432 \
  -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=hillprime postgres:16

# Env
cp .env.example .env
# Edit .env:
#   DATABASE_URL=postgres://postgres:postgres@localhost:5432/hillprime?sslmode=disable
#   INTERNAL_HMAC_KEY=$(openssl rand -hex 32)

# Run
go mod tidy
go run ./cmd/api
# → migrations auto-apply on first boot
# → "api listening" on :8080

# Seed a user (any Postgres client):
psql $DATABASE_URL -c "insert into users (email, role, email_verified_at)
  values ('you@example.com', 'admin', now());"

# Sign in:
curl -X POST http://localhost:8080/auth/magic-link \
  -H "Content-Type: application/json" \
  -d '{"email":"you@example.com"}'
# → check your RESEND_API_KEY inbox (or the server log in dev — it prints the link)
```

## Deploying

See `docs/PORTABILITY.md` for host-by-host instructions. The short version:

- **Railway / Fly:** `Dockerfile` is ready; set env vars in the dashboard; deploy.
- **Docker Compose:** mount `.env` as `env_file`, point at an external Postgres.
- **Kubernetes:** the binary is a single static executable under `nonroot`; any base image works.

## Production checklist

- [ ] `INTERNAL_HMAC_KEY` is 64 hex chars, set identically on API and Next.js proxy
- [ ] `DATABASE_URL` uses SSL (`sslmode=require` on most hosts, `verify-full` if you control certs)
- [ ] `ALLOWED_ORIGINS` does not include `*` or dev origins
- [ ] `APP_ENV=production` so the session cookie is `Secure` and debug logs are off
- [ ] `RESEND_FROM` uses a verified domain
- [ ] First admin user inserted with `role='admin'`
- [ ] `BASE_URL` and `ADMIN_APP_URL` point at the real domains (magic-link URL building depends on them)
- [ ] Monitoring: hit `/healthz` every 30 s from Uptime Robot or equivalent

## What's deliberately NOT here

- **No ORM.** pgx + hand-written SQL. Portable, auditable, fast.
- **No migration framework** (goose / atlas / sqlmigrate). An 80-line `runMigrations` in `main.go` handles it. Upgrade later if you ever have > 20 migrations.
- **No queue.** Notification fan-out is a goroutine. Move to [River](https://riverqueue.com/) if volume exceeds ~10 req/s.
- **No metrics lib.** If you need it, add `promhttp.Handler()` on `/metrics` and a Prometheus scrape.
- **No config struct validation lib.** `config.Load` does targeted checks for the 2–3 required vars.

Simplicity is a feature. Add machinery only when a measured problem demands it.
