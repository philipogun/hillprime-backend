-- =====================================================================
-- HillPrime API — Portable Postgres schema
-- =====================================================================
-- Runs on any Postgres >= 14. No Supabase-specific objects.
--
-- Portability choices:
--   * Own `users` table — NOT auth.users. RLS is not used; authorization
--     is enforced in the Go application layer (simpler, portable, testable).
--   * LISTEN/NOTIFY for realtime — works on every Postgres, including Neon,
--     RDS, Fly Postgres, DigitalOcean, self-hosted.
--   * pgcrypto for UUIDs — available on every hosted Postgres.
--   * No row-level security. The Go API is the only thing that talks to
--     this DB with a real connection string; it enforces all policy.
--
-- If you want to run this on Supabase, everything works — you just won't
-- use supabase.auth. Users sign in through your own /auth endpoints.
-- =====================================================================

-- Run as the migration owner (often postgres). On Supabase, run this in
-- the SQL Editor under your project's default role.

begin;

create extension if not exists "pgcrypto";

-- ---- Enums -----------------------------------------------------------
do $$ begin
  create type lead_status as enum ('new','contacted','qualified','won','lost','spam');
exception when duplicate_object then null; end $$;

do $$ begin
  create type budget_band as enum ('undisclosed','small','medium','large','enterprise');
exception when duplicate_object then null; end $$;

do $$ begin
  create type project_type as enum ('product','consulting','partnership','general');
exception when duplicate_object then null; end $$;

do $$ begin
  create type user_role as enum ('admin','viewer');
exception when duplicate_object then null; end $$;

-- ---- users (own it, don't depend on auth.users) ----------------------
-- Stored as opaque: email is the primary identifier, password lives in
-- password_hash as an argon2id encoded string. Magic-link tokens live in
-- the auth_tokens table below — they expire, are single-use, and never
-- need to hit disk twice.
create table if not exists users (
  id                uuid primary key default gen_random_uuid(),
  created_at        timestamptz not null default now(),
  updated_at        timestamptz not null default now(),
  email             text not null unique,
  email_verified_at timestamptz,
  password_hash     text,                      -- nullable: magic-link-only users
  role              user_role not null default 'viewer',
  name              text,
  last_login_at     timestamptz,
  disabled          boolean not null default false
);

create index if not exists users_email_lower_idx on users (lower(email));

-- Short-lived single-use tokens for magic-link sign-in, email
-- verification, and password reset. Hash of the token is stored; the
-- cleartext goes in the link.
create table if not exists auth_tokens (
  id           uuid primary key default gen_random_uuid(),
  user_id      uuid not null references users(id) on delete cascade,
  purpose      text not null check (purpose in ('magic_link','verify_email','reset_password')),
  token_hash   text not null,
  expires_at   timestamptz not null,
  used_at      timestamptz,
  created_at   timestamptz not null default now()
);
create index if not exists auth_tokens_hash_idx on auth_tokens (token_hash);
create index if not exists auth_tokens_user_idx on auth_tokens (user_id);

-- ---- leads -----------------------------------------------------------
create table if not exists leads (
  id              uuid primary key default gen_random_uuid(),
  created_at      timestamptz not null default now(),
  updated_at      timestamptz not null default now(),

  name            text,
  email           text,
  phone           text,

  project_type    project_type,
  budget          budget_band   not null default 'undisclosed',
  painpoint       text,
  message         text,
  timeline        text,

  score           int           not null default 0,
  is_hot          boolean       not null default false,

  utm_source      text,
  utm_medium      text,
  utm_campaign    text,
  referrer        text,
  landing_page    text,
  pages_viewed    int           not null default 1,
  session_id      text,

  ip              text,
  user_agent      text,
  country         text,
  timezone        text,
  status          lead_status   not null default 'new',
  completeness    int           not null default 0,

  notified_at     timestamptz,
  owner_user_id   uuid          references users(id) on delete set null
);

create index if not exists leads_created_at_idx   on leads (created_at desc);
create index if not exists leads_status_idx       on leads (status);
create index if not exists leads_score_idx        on leads (score desc);
create index if not exists leads_email_idx        on leads (email);
create index if not exists leads_utm_campaign_idx on leads (utm_campaign);

-- ---- events (analytics) ----------------------------------------------
create table if not exists events (
  id           bigserial primary key,
  created_at   timestamptz not null default now(),
  session_id   text not null,
  lead_id      uuid references leads(id) on delete set null,
  kind         text not null,
  path         text,
  referrer     text,
  cta_id       text,
  scroll_pct   int,
  dwell_ms     int,
  properties   jsonb not null default '{}'::jsonb,
  ip           text,
  user_agent   text,
  country      text
);
create index if not exists events_session_idx on events (session_id);
create index if not exists events_kind_idx    on events (kind);
create index if not exists events_created_idx on events (created_at desc);
create index if not exists events_lead_idx    on events (lead_id);

-- ---- subscribers (newsletter) ----------------------------------------
create table if not exists subscribers (
  id             uuid primary key default gen_random_uuid(),
  created_at     timestamptz not null default now(),
  email          text not null unique,
  confirmed      boolean not null default false,
  source         text,
  unsubscribed   boolean not null default false
);

-- ---- lead_notes ------------------------------------------------------
create table if not exists lead_notes (
  id          bigserial primary key,
  created_at  timestamptz not null default now(),
  lead_id     uuid not null references leads(id) on delete cascade,
  author_id   uuid references users(id) on delete set null,
  body        text not null
);
create index if not exists lead_notes_lead_idx on lead_notes (lead_id, created_at desc);

-- ---- sessions (server-side, for API auth) ----------------------------
-- We could use stateless JWTs, but refresh rotation + revocation is simpler
-- with a server session table. ~8 bytes per active session; trivial.
create table if not exists sessions (
  id            uuid primary key default gen_random_uuid(),
  user_id       uuid not null references users(id) on delete cascade,
  token_hash    text not null unique,
  created_at    timestamptz not null default now(),
  last_used_at  timestamptz not null default now(),
  expires_at    timestamptz not null,
  user_agent    text,
  ip            text,
  revoked_at    timestamptz
);
create index if not exists sessions_user_idx    on sessions (user_id);
create index if not exists sessions_expires_idx on sessions (expires_at);

-- ---- updated_at trigger ----------------------------------------------
create or replace function touch_updated_at()
returns trigger language plpgsql as $$
begin
  new.updated_at = now();
  return new;
end;
$$;

drop trigger if exists leads_touch on leads;
create trigger leads_touch before update on leads
  for each row execute function touch_updated_at();

drop trigger if exists users_touch on users;
create trigger users_touch before update on users
  for each row execute function touch_updated_at();

-- ---- Realtime via LISTEN/NOTIFY --------------------------------------
-- Portable replacement for Supabase Realtime. The Go API LISTENs on
-- 'lead_events' and forwards notifications to connected admin dashboards
-- via Server-Sent Events.
create or replace function notify_lead_event()
returns trigger language plpgsql as $$
declare
  payload jsonb;
begin
  payload = jsonb_build_object(
    'op', tg_op,
    'id', coalesce(new.id, old.id),
    'at', extract(epoch from now())
  );
  perform pg_notify('lead_events', payload::text);
  return coalesce(new, old);
end;
$$;

drop trigger if exists leads_notify on leads;
create trigger leads_notify
  after insert or update or delete on leads
  for each row execute function notify_lead_event();

-- ---- Admin views -----------------------------------------------------
create or replace view admin_stats_daily as
select
  date_trunc('day', created_at)::date as day,
  count(*)                                     as leads_total,
  count(*) filter (where is_hot)               as leads_hot,
  count(*) filter (where status = 'qualified') as leads_qualified,
  count(*) filter (where status = 'won')       as leads_won,
  avg(score)::int                              as avg_score
from leads
group by 1
order by 1 desc;

create or replace view admin_painpoint_clusters as
select
  lower(regexp_replace(coalesce(painpoint, message, ''), '[^a-z0-9 ]', '', 'g')) as text,
  count(*) as n
from leads
where coalesce(painpoint, message) is not null
  and length(coalesce(painpoint, message)) > 10
group by 1
having count(*) > 0
order by n desc
limit 50;

-- ---- Bootstrap the first admin user ----------------------------------
-- After running this migration, insert your founder account:
--
--   insert into users (email, role, name, email_verified_at)
--   values ('founder@hillprimeinnovations.com', 'admin', 'Philip', now())
--   returning id;
--
-- Then sign in via /auth/magic-link and the API emails you a sign-in link.

commit;
