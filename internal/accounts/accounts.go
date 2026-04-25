package accounts

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

const (
	StatusActive        = "active"
	StatusRateLimited   = "rate_limited"
	StatusQuotaExceeded = "quota_exceeded"
	StatusPaused        = "paused"
	StatusDeactivated   = "deactivated"

	MinRemainingPercent = 5.0
)

type Account struct {
	ID             string     `json:"id"`
	Email          string     `json:"email"`
	ChatGPTAccount string     `json:"chatgpt_account_id"`
	PlanType       string     `json:"plan_type"`
	AccessToken    string     `json:"access_token"`
	RefreshToken   string     `json:"refresh_token"`
	IDToken        string     `json:"id_token"`
	LastRefresh    time.Time  `json:"last_refresh"`
	Status         string     `json:"status"`
	Paused         bool       `json:"paused"`
	CooldownUntil  *time.Time `json:"cooldown_until,omitempty"`
	ResetAt        *int64     `json:"reset_at,omitempty"`
	LastSelectedAt *time.Time `json:"last_selected_at,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	DeactivatedWhy string     `json:"deactivated_reason,omitempty"`
	Usage          UsageState `json:"usage,omitempty"`
}

type TokenClaims struct {
	Email            string
	ChatGPTAccountID string
	ChatGPTPlanType  string
	Subject          string
}

type UsageState struct {
	PlanType    string       `json:"plan_type,omitempty"`
	Primary     *UsageWindow `json:"primary_window,omitempty"`
	Secondary   *UsageWindow `json:"secondary_window,omitempty"`
	Credits     *Credits     `json:"credits,omitempty"`
	LastFetched *time.Time   `json:"last_fetched,omitempty"`
	LastError   string       `json:"last_error,omitempty"`
}

type UsageWindow struct {
	UsedPercent        *float64 `json:"used_percent,omitempty"`
	ResetAt            *int64   `json:"reset_at,omitempty"`
	LimitWindowSeconds *int64   `json:"limit_window_seconds,omitempty"`
	ResetAfterSeconds  *int64   `json:"reset_after_seconds,omitempty"`
}

type Credits struct {
	HasCredits *bool   `json:"has_credits,omitempty"`
	Unlimited  *bool   `json:"unlimited,omitempty"`
	Balance    *string `json:"balance,omitempty"`
}

func (a Account) Available(now time.Time) bool {
	if a.Paused || a.Status == StatusPaused || a.Status == StatusDeactivated {
		return false
	}
	if a.AccessToken == "" || a.RefreshToken == "" {
		return false
	}
	if a.CooldownUntil != nil && now.Before(*a.CooldownUntil) {
		return false
	}
	if a.Usage.LowRemaining(now) {
		return false
	}
	return true
}

func (u UsageState) Exhausted(now time.Time) bool {
	return u.Primary.Exhausted(now) || u.Secondary.Exhausted(now)
}

func (u UsageState) LowRemaining(now time.Time) bool {
	return u.Primary.LowRemaining(now) || u.Secondary.LowRemaining(now)
}

func (w *UsageWindow) Exhausted(now time.Time) bool {
	if w == nil || w.UsedPercent == nil || *w.UsedPercent < 100 {
		return false
	}
	return w.activeUntilReset(now)
}

func (w *UsageWindow) LowRemaining(now time.Time) bool {
	if w == nil || w.UsedPercent == nil {
		return false
	}
	if 100-*w.UsedPercent > MinRemainingPercent {
		return false
	}
	return w.activeUntilReset(now)
}

func (w *UsageWindow) activeUntilReset(now time.Time) bool {
	if w.ResetAt != nil && *w.ResetAt > 0 {
		return now.Before(time.Unix(*w.ResetAt, 0))
	}
	if w.ResetAfterSeconds != nil && *w.ResetAfterSeconds > 0 {
		return true
	}
	return true
}

func ShouldRefresh(lastRefresh time.Time, now time.Time) bool {
	if lastRefresh.IsZero() {
		return true
	}
	return now.Sub(lastRefresh) > 8*24*time.Hour
}

func ClaimsFromIDToken(idToken string) TokenClaims {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return TokenClaims{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return TokenClaims{}
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return TokenClaims{}
	}
	out := TokenClaims{}
	if v, ok := claims["email"].(string); ok {
		out.Email = v
	}
	if v, ok := claims["sub"].(string); ok {
		out.Subject = v
	}
	if v, ok := claims["chatgpt_account_id"].(string); ok {
		out.ChatGPTAccountID = v
	}
	if v, ok := claims["chatgpt_plan_type"].(string); ok {
		out.ChatGPTPlanType = v
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if v, ok := auth["chatgpt_account_id"].(string); ok && out.ChatGPTAccountID == "" {
			out.ChatGPTAccountID = v
		}
		if v, ok := auth["chatgpt_plan_type"].(string); ok && out.ChatGPTPlanType == "" {
			out.ChatGPTPlanType = v
		}
	}
	if auth, ok := claims["auth"].(map[string]any); ok {
		if v, ok := auth["chatgpt_account_id"].(string); ok && out.ChatGPTAccountID == "" {
			out.ChatGPTAccountID = v
		}
		if v, ok := auth["chatgpt_plan_type"].(string); ok && out.ChatGPTPlanType == "" {
			out.ChatGPTPlanType = v
		}
	}
	return out
}
