// Package realtime bridges Postgres LISTEN/NOTIFY to Server-Sent Events
// streamed to admin browsers.
//
// Why this instead of Supabase Realtime:
//   - Works on every Postgres (Supabase, Neon, RDS, Fly, self-hosted).
//   - No second protocol to reason about (WebSockets + Phoenix channels).
//   - One code path for tests.
//
// Flow:
//  1. Go backend opens one dedicated pgx connection and LISTENs on the
//     'lead_events' channel.
//  2. A trigger on the leads table calls pg_notify('lead_events', …) on
//     every insert/update/delete (see migrations/001_init.sql).
//  3. A hub fan-outs each payload to all connected SSE clients.
//  4. Admin dashboards open GET /v1/admin/stream and receive pings; the
//     client refetches the affected lead to get the full row.
package realtime

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// Hub is a fan-out of a single Postgres LISTEN to many SSE clients.
type Hub struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
}

func NewHub() *Hub {
	return &Hub{clients: make(map[chan string]struct{})}
}

// Subscribe returns a buffered channel that receives notification payloads.
// The caller must call the returned cancel function when done.
func (h *Hub) Subscribe() (<-chan string, func()) {
	ch := make(chan string, 32)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.clients[ch]; ok {
			delete(h.clients, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// Broadcast sends payload to all subscribers. Slow subscribers are dropped
// rather than blocking — an admin dashboard that can't keep up doesn't get
// to stall the pipeline.
func (h *Hub) Broadcast(payload string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- payload:
		default:
			log.Warn().Msg("sse subscriber backpressure; dropping")
		}
	}
}

// Run drives the LISTEN loop against a dedicated connection checked out of
// the pool. It auto-reconnects on error with exponential backoff and never
// returns until ctx is canceled.
//
// Call this exactly once at startup in its own goroutine:
//
//	hub := realtime.NewHub()
//	go realtime.Run(ctx, pool, "lead_events", hub)
func Run(ctx context.Context, pool *pgxpool.Pool, channel string, hub *Hub) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}
		if err := listenLoop(ctx, pool, channel, hub); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error().Err(err).Str("channel", channel).
				Dur("backoff", backoff).Msg("listen loop failed; retrying")
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		// Clean exit (ctx done) — return.
		return
	}
}

func listenLoop(ctx context.Context, pool *pgxpool.Pool, channel string, hub *Hub) error {
	// LISTEN needs a dedicated connection held for its lifetime — can't go
	// back to the pool between WaitForNotification calls.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+quoteIdent(channel)); err != nil {
		return err
	}
	log.Info().Str("channel", channel).Msg("realtime listening")

	// Reset backoff on successful connection.
	for {
		note, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		hub.Broadcast(note.Payload)
	}
}

// quoteIdent minimally guards against LISTEN injection. Channel names are
// hardcoded in our case but belt-and-braces.
func quoteIdent(s string) string {
	out := []byte{'"'}
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			out = append(out, '"')
		}
		out = append(out, s[i])
	}
	out = append(out, '"')
	return string(out)
}
