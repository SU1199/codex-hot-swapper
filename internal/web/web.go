package web

import (
	"context"
	_ "embed"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"codex-hot-swapper/internal/accounts"
	"codex-hot-swapper/internal/codexconfig"
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

type accountView struct {
	Account          accounts.Account
	DisplayStatus    string
	PrimaryUsed      string
	PrimaryLeft      string
	PrimaryReset     string
	SecondaryUsed    string
	SecondaryLeft    string
	SecondaryReset   string
	Credits          string
	LastSelected     string
	Cooldown         string
	Fetched          string
	HasPrimary       bool
	PrimaryLeftValue string
}

type usageSummary struct {
	AccountCount        int
	ActiveCount         int
	KnownPrimaryCount   int
	PrimaryUsed         string
	PrimaryLeft         string
	PrimaryLeftValue    string
	KnownSecondaryCount int
	SecondaryUsed       string
	SecondaryLeft       string
	SecondaryLeftValue  string
	StrategyLabel       string
}

type requestLogView struct {
	Time      time.Time
	Action    string
	AccountID string
	Result    string
	Attempt   int
	Error     string
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
	mux.HandleFunc("/settings/strategy", w.updateStrategy)
	mux.HandleFunc("/codex-config/install", w.installCodexConfig)
}

func (w *Web) index(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}
	accts, settings, _ := w.store.Snapshot()
	accountViews := buildAccountViews(accts)
	data := map[string]any{
		"Accounts": accountViews,
		"Summary":  buildUsageSummary(accts, settings.Strategy),
		"Settings": settings,
		"DataDir":  w.store.Dir(),
		"Logs":     buildRequestLogViews(w.store.RecentRequestLogs(25)),
		"Config":   codexconfig.Snippet(),
		"Strategy": settings.Strategy,
		"Notice":   r.URL.Query().Get("notice"),
		"Error":    r.URL.Query().Get("error"),
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

func (w *Web) updateStrategy(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := w.store.UpdateStrategy(r.FormValue("strategy")); err != nil {
		http.Redirect(rw, r, "/?error="+url.QueryEscape(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(rw, r, "/?notice="+url.QueryEscape("Updated account strategy"), http.StatusFound)
}

func (w *Web) installCodexConfig(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	result, err := codexconfig.InstallDefault(time.Now())
	if err != nil {
		http.Redirect(rw, r, "/?error="+url.QueryEscape(err.Error()), http.StatusFound)
		return
	}
	message := "Updated " + result.ConfigPath
	if result.BackupPath != "" {
		message += " and wrote backup " + result.BackupPath
	}
	http.Redirect(rw, r, "/?notice="+url.QueryEscape(message), http.StatusFound)
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

func buildAccountViews(accts []accounts.Account) []accountView {
	out := make([]accountView, 0, len(accts))
	for _, acct := range accts {
		view := accountView{
			Account:        acct,
			DisplayStatus:  displayStatus(acct),
			PrimaryUsed:    usageUsed(acct.Usage.Primary),
			PrimaryLeft:    usageLeft(acct.Usage.Primary),
			PrimaryReset:   usageReset(acct.Usage.Primary),
			SecondaryUsed:  usageUsed(acct.Usage.Secondary),
			SecondaryLeft:  usageLeft(acct.Usage.Secondary),
			SecondaryReset: usageReset(acct.Usage.Secondary),
			Credits:        credits(acct.Usage.Credits),
			LastSelected:   formatTime(acct.LastSelectedAt),
			Cooldown:       formatTime(acct.CooldownUntil),
			Fetched:        ago(acct.Usage.LastFetched),
			HasPrimary:     acct.Usage.Primary != nil && acct.Usage.Primary.UsedPercent != nil,
		}
		if view.HasPrimary {
			view.PrimaryLeftValue = usageLeft(acct.Usage.Primary)
		}
		out = append(out, view)
	}
	return out
}

func buildUsageSummary(accts []accounts.Account, strategy string) usageSummary {
	summary := usageSummary{AccountCount: len(accts), StrategyLabel: "Use first"}
	if strategy == store.StrategyRoundRobin {
		summary.StrategyLabel = "Share"
	}
	var primaryUsed float64
	var secondaryUsed float64
	now := time.Now().UTC()
	for _, acct := range accts {
		if acct.Available(now) {
			summary.ActiveCount++
		}
		if acct.Usage.Primary != nil && acct.Usage.Primary.UsedPercent != nil {
			summary.KnownPrimaryCount++
			primaryUsed += clampPercent(*acct.Usage.Primary.UsedPercent)
		}
		if acct.Usage.Secondary != nil && acct.Usage.Secondary.UsedPercent != nil {
			summary.KnownSecondaryCount++
			secondaryUsed += clampPercent(*acct.Usage.Secondary.UsedPercent)
		}
	}
	if summary.KnownPrimaryCount > 0 {
		used := primaryUsed / float64(summary.KnownPrimaryCount)
		left := 100 - used
		summary.PrimaryUsed = formatFloat(used)
		summary.PrimaryLeft = formatFloat(left)
		summary.PrimaryLeftValue = formatFloat(left)
	} else {
		summary.PrimaryUsed = "unknown"
		summary.PrimaryLeft = "unknown"
		summary.PrimaryLeftValue = "0"
	}
	if summary.KnownSecondaryCount > 0 {
		used := secondaryUsed / float64(summary.KnownSecondaryCount)
		left := 100 - used
		summary.SecondaryUsed = formatFloat(used)
		summary.SecondaryLeft = formatFloat(left)
		summary.SecondaryLeftValue = formatFloat(left)
	} else {
		summary.SecondaryUsed = "unknown"
		summary.SecondaryLeft = "unknown"
		summary.SecondaryLeftValue = "0"
	}
	return summary
}

func buildRequestLogViews(entries []store.RequestLogEntry) []requestLogView {
	out := make([]requestLogView, 0, len(entries))
	for _, entry := range entries {
		out = append(out, requestLogView{
			Time:      entry.Time,
			Action:    displayAction(entry.Path),
			AccountID: entry.AccountID,
			Result:    displayResult(entry.Status, entry.Error),
			Attempt:   entry.Attempt,
			Error:     entry.Error,
		})
	}
	return out
}

func displayAction(path string) string {
	switch {
	case strings.Contains(path, "compact"):
		return "Shorten"
	case strings.Contains(path, "responses"):
		return "Request"
	case strings.Contains(path, "models"):
		return "Models"
	default:
		return "Check"
	}
}

func displayResult(status int, err string) string {
	if err != "" {
		return "Problem"
	}
	if status == 0 {
		return "Started"
	}
	if status >= 200 && status < 400 {
		return "OK"
	}
	return strconv.Itoa(status)
}

func displayStatus(acct accounts.Account) string {
	if acct.Paused {
		return "Paused"
	}
	switch acct.Status {
	case accounts.StatusActive:
		return "Ready"
	case accounts.StatusRateLimited:
		return "Waiting"
	case accounts.StatusQuotaExceeded:
		return "Empty"
	case accounts.StatusDeactivated:
		return "Needs login"
	case "":
		return "Ready"
	default:
		return acct.Status
	}
}

func usageUsed(win *accounts.UsageWindow) string {
	if win == nil || win.UsedPercent == nil {
		return "unknown"
	}
	return formatFloat(clampPercent(*win.UsedPercent))
}

func usageLeft(win *accounts.UsageWindow) string {
	if win == nil || win.UsedPercent == nil {
		return "unknown"
	}
	return formatFloat(100 - clampPercent(*win.UsedPercent))
}

func usageReset(win *accounts.UsageWindow) string {
	if win == nil {
		return "unknown"
	}
	if win.ResetAt != nil && *win.ResetAt > 0 {
		return time.Unix(*win.ResetAt, 0).Local().Format("Jan 2 15:04")
	}
	if win.ResetAfterSeconds != nil && *win.ResetAfterSeconds > 0 {
		return "in " + formatDuration(time.Duration(*win.ResetAfterSeconds)*time.Second)
	}
	return "unknown"
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

func formatTime(t *time.Time) string {
	if t == nil {
		return "none"
	}
	return t.Local().Format("Jan 2 15:04")
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

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
