package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hillprime/api/internal/config"
	"github.com/hillprime/api/internal/store"
	"github.com/rs/zerolog/log"
)

// Notifier fans one lead to all channels concurrently.
type Notifier struct {
	cfg  *config.Config
	http *http.Client
}

func New(cfg *config.Config) *Notifier {
	return &Notifier{cfg: cfg, http: &http.Client{Timeout: 8 * time.Second}}
}

func (n *Notifier) Notify(ctx context.Context, lead *store.Lead) {
	var wg sync.WaitGroup
	if n.cfg.TelegramBotToken != "" && n.cfg.TelegramChatID != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := n.sendTelegram(ctx, lead); err != nil {
				log.Error().Err(err).Str("lead", lead.ID).Msg("telegram failed")
			}
		}()
	}
	if n.cfg.ResendAPIKey != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := n.sendEmail(ctx, lead); err != nil {
				log.Error().Err(err).Str("lead", lead.ID).Msg("resend failed")
			}
		}()
	}
	wg.Wait()
}

// SendMagicLinkEmail emails a sign-in link to the user.
func (n *Notifier) SendMagicLinkEmail(ctx context.Context, to, link string) error {
	if n.cfg.ResendAPIKey == "" {
		// Dev mode: log the link so you can still click through.
		log.Info().Str("to", to).Str("link", link).Msg("magic-link email (DEV — would send)")
		return nil
	}
	body := fmt.Sprintf(`
<!doctype html><html><body style="font-family:-apple-system,Segoe UI,Roboto,sans-serif;background:#f8fafc;padding:24px;">
<table width="100%%" style="max-width:480px;margin:0 auto;background:#fff;border-radius:12px;padding:32px;border:1px solid #e2e8f0;">
<tr><td>
<h1 style="font-size:18px;margin:0 0 8px;color:#0f172a;">Sign in to HillPrime</h1>
<p style="color:#475569;font-size:14px;line-height:1.5;margin:0 0 24px;">
This link expires in 20 minutes. If you didn't request it, you can ignore this email.
</p>
<a href="%s" style="display:inline-block;padding:12px 24px;background:#0B2B40;color:#fff;text-decoration:none;border-radius:8px;font-weight:600;font-size:14px;">
Sign in
</a>
<p style="color:#94a3b8;font-size:12px;margin-top:24px;">
Or copy: <br/><code style="word-break:break-all;">%s</code>
</p>
</td></tr></table></body></html>`, html.EscapeString(link), html.EscapeString(link))

	return n.resend(ctx, map[string]any{
		"from":    n.cfg.ResendFrom,
		"to":      []string{to},
		"subject": "Sign in to HillPrime",
		"html":    body,
	})
}

func (n *Notifier) ConfirmSubscriber(ctx context.Context, email string) error {
	if n.cfg.ResendAPIKey == "" {
		return nil
	}
	return n.resend(ctx, map[string]any{
		"from":    n.cfg.ResendFrom,
		"to":      []string{email},
		"subject": "Welcome to HillPrime Insights",
		"html":    `<p>You're in. Expect one email a month on AI, DevOps, and building for Africa — nothing else.</p><p>— Philip · HillPrime</p>`,
	})
}

// ---- Telegram --------------------------------------------------------

func (n *Notifier) sendTelegram(ctx context.Context, l *store.Lead) error {
	hot := ""
	if l.IsHot {
		hot = "🔥🔥🔥 *HOT LEAD* 🔥🔥🔥\n\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s*New lead — score %d/100*\n\n", hot, l.Score)
	if l.Name != nil {
		fmt.Fprintf(&b, "👤 %s\n", esc(*l.Name))
	}
	if l.Email != nil {
		fmt.Fprintf(&b, "✉️ `%s`\n", esc(*l.Email))
	}
	if l.Phone != nil {
		fmt.Fprintf(&b, "📞 %s\n", esc(*l.Phone))
	}
	if l.Budget != "" && l.Budget != "undisclosed" {
		fmt.Fprintf(&b, "💰 *%s*\n", esc(strings.ToUpper(l.Budget)))
	}
	if l.ProjectType != nil {
		fmt.Fprintf(&b, "🏷 %s\n", esc(*l.ProjectType))
	}
	if s := deref(l.Painpoint); s != "" {
		fmt.Fprintf(&b, "\n🎯 Painpoint:\n_%s_\n", esc(truncate(s, 300)))
	}
	if s := deref(l.Message); s != "" {
		fmt.Fprintf(&b, "\n💬 Message:\n_%s_\n", esc(truncate(s, 500)))
	}
	if l.UTMCampaign != nil {
		fmt.Fprintf(&b, "\n📍 Campaign: `%s`", esc(*l.UTMCampaign))
	}
	if l.Country != nil {
		fmt.Fprintf(&b, "\n🌍 %s", esc(*l.Country))
	}
	fmt.Fprintf(&b, "\n\n[Open in admin](%s/leads/%s)", n.cfg.AdminAppURL, l.ID)

	payload := map[string]any{
		"chat_id":                  n.cfg.TelegramChatID,
		"text":                     b.String(),
		"parse_mode":               "Markdown",
		"disable_web_page_preview": true,
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.cfg.TelegramBotToken)
	return n.post(ctx, url, nil, body)
}

// ---- Resend ----------------------------------------------------------

func (n *Notifier) sendEmail(ctx context.Context, l *store.Lead) error {
	subject := fmt.Sprintf("New lead — %s · score %d", derefOr(l.Email, "unknown"), l.Score)
	if l.IsHot {
		subject = "🔥 HOT " + subject
	}
	body := renderLeadEmail(l, n.cfg.AdminAppURL)
	payload := map[string]any{
		"from":     n.cfg.ResendFrom,
		"to":       []string{n.cfg.FounderEmail},
		"subject":  subject,
		"html":     body,
		"reply_to": derefOr(l.Email, ""),
	}
	return n.resend(ctx, payload)
}

func (n *Notifier) resend(ctx context.Context, payload any) error {
	body, _ := json.Marshal(payload)
	headers := map[string]string{
		"Authorization": "Bearer " + n.cfg.ResendAPIKey,
	}
	return n.post(ctx, "https://api.resend.com/emails", headers, body)
}

func (n *Notifier) post(ctx context.Context, url string, headers map[string]string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d from %s", resp.StatusCode, url)
	}
	return nil
}

// ---- helpers ---------------------------------------------------------

func esc(s string) string {
	r := strings.NewReplacer("_", "\\_", "*", "\\*", "[", "\\[", "`", "\\`")
	return r.Replace(s)
}
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func derefOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}
