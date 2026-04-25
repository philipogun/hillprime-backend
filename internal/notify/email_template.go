package notify

import (
	"fmt"
	"html"
	"strings"

	"github.com/hillprime/api/internal/store"
)

func renderLeadEmail(l *store.Lead, adminURL string) string {
	var rows strings.Builder
	row := func(k, v string) {
		if v == "" {
			return
		}
		fmt.Fprintf(&rows,
			`<tr><td style="padding:8px 12px;color:#64748b;font-size:12px;text-transform:uppercase;letter-spacing:.05em;width:130px;vertical-align:top;">%s</td><td style="padding:8px 12px;color:#0f172a;font-size:14px;">%s</td></tr>`,
			html.EscapeString(k), html.EscapeString(v))
	}

	row("Score", fmt.Sprintf("%d / 100%s", l.Score, yesNo(l.IsHot, " 🔥 HOT", "")))
	row("Name", derefOr(l.Name, ""))
	row("Email", derefOr(l.Email, ""))
	row("Phone", derefOr(l.Phone, ""))
	if l.Budget != "" && l.Budget != "undisclosed" {
		row("Budget", strings.ToUpper(l.Budget))
	}
	row("Project type", derefOr(l.ProjectType, ""))
	row("Timeline", derefOr(l.Timeline, ""))
	row("Painpoint", derefOr(l.Painpoint, ""))
	row("Message", derefOr(l.Message, ""))
	row("Source", derefOr(l.UTMSource, ""))
	row("Campaign", derefOr(l.UTMCampaign, ""))
	row("Landing page", derefOr(l.LandingPage, ""))
	row("Referrer", derefOr(l.Referrer, ""))
	row("Country", derefOr(l.Country, ""))
	row("Pages viewed", fmt.Sprintf("%d", l.PagesViewed))

	openURL := fmt.Sprintf("%s/leads/%s", adminURL, l.ID)

	return fmt.Sprintf(`<!doctype html>
<html><body style="margin:0;padding:0;background:#f8fafc;font-family:-apple-system,Segoe UI,Roboto,sans-serif;">
<table width="100%%" cellspacing="0" cellpadding="0" style="max-width:600px;margin:20px auto;background:#fff;border-radius:12px;border:1px solid #e2e8f0;overflow:hidden;">
  <tr><td style="padding:24px;background:#0B2B40;color:#fff;">
    <div style="font-size:12px;opacity:.7;margin-bottom:4px;">HILLPRIME INNOVATIONS</div>
    <div style="font-size:20px;font-weight:700;">%s</div>
  </td></tr>
  <tr><td style="padding:8px 12px;">
    <table cellspacing="0" cellpadding="0" width="100%%">%s</table>
  </td></tr>
  <tr><td style="padding:20px 24px;background:#f8fafc;border-top:1px solid #e2e8f0;">
    <a href="%s" style="display:inline-block;padding:10px 20px;background:#2A7DE1;color:#fff;text-decoration:none;border-radius:8px;font-weight:600;font-size:14px;">Open lead in dashboard →</a>
  </td></tr>
</table>
</body></html>`,
		yesNo(l.IsHot, "🔥 Hot lead", "New lead"), rows.String(), html.EscapeString(openURL))
}

func yesNo(b bool, y, n string) string {
	if b {
		return y
	}
	return n
}
