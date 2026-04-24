package web

import (
	"context"
	_ "embed"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codex-hot-swapper/internal/accounts"
	"codex-hot-swapper/internal/oauth"
	"codex-hot-swapper/internal/store"
	"codex-hot-swapper/internal/usage"
)

//go:embed templates/index.html
var indexHTML string

type Web struct {
	store *store.Store
	oauth *oauth.Service
	usage *usage.Service
	tmpl  *template.Template
}

func New(st *store.Store, oauthSvc *oauth.Service, usageSvc *usage.Service) *Web {
	return &Web{
		store: st,
		oauth: oauthSvc,
		usage: usageSvc,
		tmpl: template.Must(template.New("index").Funcs(template.FuncMap{
			"usageWindow": usageWindow,
			"credits":     credits,
			"ago":         ago,
		}).Parse(indexHTML)),
	}
}

func (w *Web) Register(mux *http.ServeMux) {
	mux.HandleFunc("/", w.index)
	mux.HandleFunc("/oauth/start", w.oauthStart)
	mux.HandleFunc("/account/switch", w.switchAccount)
	mux.HandleFunc("/account/pause", w.pause)
	mux.HandleFunc("/account/resume", w.resume)
	mux.HandleFunc("/account/delete", w.delete)
	mux.HandleFunc("/usage/refresh", w.refreshUsage)
	mux.HandleFunc("/usage/refresh-all", w.refreshAllUsage)
}

func (w *Web) index(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}
	accts, settings, _ := w.store.Snapshot()
	data := map[string]any{
		"Accounts": accts,
		"Settings": settings,
		"DataDir":  w.store.Dir(),
		"Logs":     w.store.RecentRequestLogs(25),
		"Config": `model_provider = "codex-hot-swapper"

[model_providers.codex-hot-swapper]
name = "OpenAI"
base_url = "http://127.0.0.1:2455/backend-api/codex"
wire_api = "responses"
supports_websockets = true
requires_openai_auth = true`,
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = w.tmpl.Execute(rw, data)
}

func (w *Web) oauthStart(rw http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	u, err := w.oauth.StartLogin(ctx)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(rw, r, u, http.StatusFound)
}

func (w *Web) refreshUsage(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	_ = w.usage.RefreshAccount(ctx, r.FormValue("id"))
	http.Redirect(rw, r, "/", http.StatusFound)
}

func (w *Web) refreshAllUsage(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	w.usage.RefreshAll(ctx)
	http.Redirect(rw, r, "/", http.StatusFound)
}

func (w *Web) switchAccount(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = w.store.PreferAccount(r.FormValue("id"))
	http.Redirect(rw, r, "/", http.StatusFound)
}

func (w *Web) pause(rw http.ResponseWriter, r *http.Request) {
	w.setPaused(rw, r, true)
}

func (w *Web) resume(rw http.ResponseWriter, r *http.Request) {
	w.setPaused(rw, r, false)
}

func (w *Web) setPaused(rw http.ResponseWriter, r *http.Request, paused bool) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.FormValue("id")
	_ = w.store.UpdateAccount(id, func(acct *accounts.Account) {
		acct.Paused = paused
		if paused {
			acct.Status = accounts.StatusPaused
		} else {
			acct.Status = accounts.StatusActive
			acct.CooldownUntil = nil
		}
	})
	http.Redirect(rw, r, "/", http.StatusFound)
}

func (w *Web) delete(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = w.store.DeleteAccount(r.FormValue("id"))
	http.Redirect(rw, r, "/", http.StatusFound)
}

func usageWindow(win *accounts.UsageWindow) string {
	if win == nil || win.UsedPercent == nil {
		return "unknown"
	}
	remaining := 100 - *win.UsedPercent
	if remaining < 0 {
		remaining = 0
	}
	parts := []string{formatFloat(*win.UsedPercent) + "% used", formatFloat(remaining) + "% left"}
	if win.LimitWindowSeconds != nil && *win.LimitWindowSeconds > 0 {
		parts = append(parts, formatDuration(time.Duration(*win.LimitWindowSeconds)*time.Second)+" window")
	}
	if win.ResetAt != nil && *win.ResetAt > 0 {
		parts = append(parts, "resets "+time.Unix(*win.ResetAt, 0).Local().Format("Jan 2 15:04"))
	} else if win.ResetAfterSeconds != nil && *win.ResetAfterSeconds > 0 {
		parts = append(parts, "resets in "+formatDuration(time.Duration(*win.ResetAfterSeconds)*time.Second))
	}
	return strings.Join(parts, " · ")
}

func credits(c *accounts.Credits) string {
	if c == nil {
		return "unknown"
	}
	if c.Unlimited != nil && *c.Unlimited {
		return "unlimited"
	}
	if c.Balance != nil && *c.Balance != "" {
		return *c.Balance
	}
	if c.HasCredits != nil && !*c.HasCredits {
		return "none"
	}
	return "available"
}

func ago(t *time.Time) string {
	if t == nil {
		return "never"
	}
	d := time.Since(*t)
	if d < time.Minute {
		return "just now"
	}
	return formatDuration(d) + " ago"
}

func formatDuration(d time.Duration) string {
	if d < time.Hour {
		return strings.TrimSuffix((d.Round(time.Minute)).String(), "0s")
	}
	if d < 48*time.Hour {
		return strings.TrimSuffix((d.Round(time.Hour)).String(), "0m0s")
	}
	return strings.TrimSuffix((d.Round(24 * time.Hour)).String(), "0h0m0s")
}

func formatFloat(v float64) string {
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', 1, 64), "0"), ".")
}
