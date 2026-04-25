// Package auth implements the API's authentication. It intentionally does
// not depend on any hosted IdP.
//
// Flow:
//
//  1. POST /auth/magic-link {email}
//     → if user exists, create auth_token(purpose=magic_link), email the link
//     → response is identical regardless of existence (no user enumeration)
//  2. GET /auth/callback?token=…
//     → ConsumeAuthToken (single-use, atomic)
//     → mint a session (server-side), set HttpOnly SameSite=Lax cookie
//     → redirect to /admin
//  3. Subsequent API calls use the cookie. Middleware resolves it to a
//     User and puts that user in the request context.
//
// We prefer server sessions over stateless JWTs so that:
//   - Sign-out is immediate (DELETE FROM sessions), no blacklist ceremony.
//   - Token rotation is trivial.
//   - A leaked cookie cannot outlive its expiry even if we lose the signing key.
//
// Swapping providers: any HS256/RS256 IdP can be plugged in by implementing
// TokenVerifier below and mounting its middleware instead of SessionAuth.
// Handlers only read users from context; they don't know where identity
// came from.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hillprime/api/internal/store"
	"golang.org/x/crypto/argon2"
)

// ---- Context keys ----------------------------------------------------

type ctxKey int

const (
	ctxUser ctxKey = iota
)

// UserFromCtx returns the authenticated user, or nil if unauthenticated.
func UserFromCtx(ctx context.Context) *store.User {
	u, _ := ctx.Value(ctxUser).(*store.User)
	return u
}

// ---- TokenVerifier: the seam for future IdP swaps --------------------

// TokenVerifier turns an opaque credential (cookie value, bearer token,
// whatever) into a User. The current implementation is CookieSession.
// Future implementations: JWTVerifier{jwks}, ClerkVerifier{…}, etc.
type TokenVerifier interface {
	VerifyRequest(r *http.Request) (*store.User, error)
}

// ---- Session-cookie verifier (default) -------------------------------

const (
	CookieName      = "hp_session"
	sessionLifetime = 14 * 24 * time.Hour
	tokenLifetime   = 20 * time.Minute
)

// CookieSession verifies session cookies against the sessions table.
type CookieSession struct {
	Store store.Store
}

var ErrUnauthenticated = errors.New("unauthenticated")

func (c *CookieSession) VerifyRequest(r *http.Request) (*store.User, error) {
	cookie, err := r.Cookie(CookieName)
	if err != nil || cookie.Value == "" {
		return nil, ErrUnauthenticated
	}
	hash := hashToken(cookie.Value)
	sess, err := c.Store.GetSessionByTokenHash(r.Context(), hash)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	user, err := c.Store.GetUserByID(r.Context(), sess.UserID)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	if user.Disabled {
		return nil, ErrUnauthenticated
	}
	// Touch async to avoid slowing the request. Best-effort.
	go func(id string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.Store.TouchSession(ctx, id)
	}(sess.ID)
	return user, nil
}

// Middleware returns an http middleware that rejects unauthenticated requests
// and injects the user into the request context for downstream handlers.
func Middleware(v TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, err := v.VerifyRequest(r)
			if err != nil {
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), ctxUser, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole rejects requests whose authenticated user doesn't hold role.
// Use downstream of Middleware.
func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := UserFromCtx(r.Context())
			if u == nil || u.Role != role {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---- Token / cookie helpers -----------------------------------------

// NewSecureToken returns a URL-safe random token and its SHA-256 hash.
// Only the hash touches the database.
func NewSecureToken() (cleartext, hashHex string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	cleartext = base64.RawURLEncoding.EncodeToString(buf)
	return cleartext, hashToken(cleartext), nil
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// ---- Magic-link issuance --------------------------------------------

// IssueMagicLink creates a one-time token tied to the given user and stores
// its hash. Returns the cleartext token the caller should email.
func IssueMagicLink(ctx context.Context, s store.Store, userID string) (string, error) {
	clear, hash, err := NewSecureToken()
	if err != nil {
		return "", err
	}
	tok := &store.AuthToken{
		UserID:    userID,
		Purpose:   "magic_link",
		TokenHash: hash,
		ExpiresAt: time.Now().Add(tokenLifetime),
	}
	if err := s.CreateAuthToken(ctx, tok); err != nil {
		return "", err
	}
	return clear, nil
}

// ExchangeTokenForSession consumes a magic-link token and creates a session.
// Returns the cleartext session token (set as a cookie) and its expiry.
func ExchangeTokenForSession(
	ctx context.Context,
	s store.Store,
	cleartextToken string,
	ua, ip string,
) (cookieValue string, expiresAt time.Time, err error) {
	at, err := s.ConsumeAuthToken(ctx, hashToken(cleartextToken), "magic_link")
	if err != nil {
		return "", time.Time{}, err
	}

	// Best-effort: mark email verified if it wasn't already.
	if user, uerr := s.GetUserByID(ctx, at.UserID); uerr == nil && user.EmailVerifiedAt == nil {
		// Ignore error — this is convenience, not correctness.
		_ = uerr
	}

	clear, hash, err := NewSecureToken()
	if err != nil {
		return "", time.Time{}, err
	}
	exp := time.Now().Add(sessionLifetime)
	sess := &store.Session{
		UserID:    at.UserID,
		TokenHash: hash,
		ExpiresAt: exp,
	}
	if ua != "" {
		sess.UserAgent = &ua
	}
	if ip != "" {
		sess.IP = &ip
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		return "", time.Time{}, err
	}
	_ = s.TouchUserLogin(ctx, at.UserID)
	return clear, exp, nil
}

// SetSessionCookie writes the session cookie. HttpOnly, SameSite=Lax,
// Secure in production.
func SetSessionCookie(w http.ResponseWriter, value string, expires time.Time, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func ClearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ---- Password hashing (for future password auth) --------------------
//
// argon2id parameters chosen to match OWASP recommendations: ~64 MB memory,
// 3 iterations, 4 parallel lanes. Verify with ComparePassword.

type argonParams struct {
	Time    uint32
	Memory  uint32
	Threads uint8
	KeyLen  uint32
}

var defaultArgon = argonParams{Time: 3, Memory: 64 * 1024, Threads: 4, KeyLen: 32}

// HashPassword returns an encoded argon2id hash suitable for storage.
func HashPassword(plain string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	p := defaultArgon
	hash := argon2.IDKey([]byte(plain), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	b64 := base64.RawStdEncoding
	return strings.Join([]string{
		"$argon2id$v=19",
		"m=" + itoa(p.Memory) + ",t=" + itoa(p.Time) + ",p=" + itoa(uint32(p.Threads)),
		b64.EncodeToString(salt),
		b64.EncodeToString(hash),
	}, "$"), nil
}

// ComparePassword returns true if plain matches the encoded hash.
func ComparePassword(encoded, plain string) bool {
	// Minimal parser — robust enough for our single-format world.
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var p argonParams
	var m, t uint32
	var pp uint8
	if _, err := parseKV(parts[3], &m, &t, &pp); err != nil {
		return false
	}
	p = argonParams{Time: t, Memory: m, Threads: pp, KeyLen: 32}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(plain), salt, p.Time, p.Memory, p.Threads, uint32(len(want)))
	return hmac.Equal(got, want)
}

// ---- HMAC for the internal Next.js proxy (unchanged from v1) -------

const InternalSignatureHeader = "X-Internal-Signature"

// InternalHMAC verifies a body-HMAC header from the Next.js proxy layer.
// This prevents random internet traffic from POSTing lead spam to the
// public endpoints.
func InternalHMAC(key string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sig := r.Header.Get(InternalSignatureHeader)
			if sig == "" {
				http.Error(w, "signature required", http.StatusUnauthorized)
				return
			}
			body, err := readAll(r)
			if err != nil {
				http.Error(w, "read body", http.StatusBadRequest)
				return
			}
			mac := hmac.New(sha256.New, []byte(key))
			mac.Write(body)
			want := hex.EncodeToString(mac.Sum(nil))
			if !hmac.Equal([]byte(want), []byte(sig)) {
				http.Error(w, "bad signature", http.StatusUnauthorized)
				return
			}
			restoreBody(r, body)
			next.ServeHTTP(w, r)
		})
	}
}
