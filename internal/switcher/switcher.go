package switcher

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"codex-hot-swapper/internal/accounts"
	"codex-hot-swapper/internal/store"
)

type Switcher struct {
	store *store.Store
}

func New(st *store.Store) *Switcher {
	return &Switcher{store: st}
}

func StickyKey(r *http.Request) string {
	for _, name := range []string{
		"x-codex-session-id",
		"x-codex-conversation-id",
		"x-openai-conversation-id",
		"x-codex-turn-state",
	} {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return name + ":" + value
		}
	}
	return ""
}

func (b *Switcher) Select(stickyKey string, exclude map[string]bool) (accounts.Account, error) {
	accts, settings, rt := b.store.Snapshot()
	now := time.Now().UTC()
	if stickyKey != "" {
		if id := rt.Sticky[stickyKey]; id != "" && !exclude[id] {
			for _, acct := range accts {
				if acct.ID == id && acct.Available(now) {
					return acct, nil
				}
			}
		}
	}

	if settings.Strategy == store.StrategyRoundRobin {
		return b.selectRoundRobin(accts, stickyKey, exclude, now)
	}
	return b.selectDrainOrder(accts, stickyKey, exclude, now)
}

func (b *Switcher) selectDrainOrder(accts []accounts.Account, stickyKey string, exclude map[string]bool, now time.Time) (accounts.Account, error) {
	for i := range accts {
		acct := accts[i]
		if exclude[acct.ID] || !acct.Available(now) {
			continue
		}
		if stickyKey != "" {
			_ = b.store.SetSticky(stickyKey, acct.ID)
		}
		return acct, nil
	}
	return accounts.Account{}, errors.New("no active accounts available")
}

func (b *Switcher) selectRoundRobin(accts []accounts.Account, stickyKey string, exclude map[string]bool, now time.Time) (accounts.Account, error) {
	var selected *accounts.Account
	for i := range accts {
		acct := accts[i]
		if exclude[acct.ID] || !acct.Available(now) {
			continue
		}
		if selected == nil {
			selected = &acct
			continue
		}
		if acct.LastSelectedAt == nil {
			selected = &acct
			continue
		}
		if selected.LastSelectedAt != nil && acct.LastSelectedAt.Before(*selected.LastSelectedAt) {
			selected = &acct
		}
	}
	if selected == nil {
		return accounts.Account{}, errors.New("no active accounts available")
	}
	if stickyKey != "" {
		_ = b.store.SetSticky(stickyKey, selected.ID)
	}
	return *selected, nil
}

func (b *Switcher) RecordSuccess(id string) {
	now := time.Now().UTC()
	_ = b.store.UpdateAccount(id, func(acct *accounts.Account) {
		acct.Status = accounts.StatusActive
		acct.LastError = ""
		acct.CooldownUntil = nil
		acct.LastSelectedAt = &now
	})
}

func (b *Switcher) RecordFailure(id, status, message string, cooldown time.Duration) {
	now := time.Now().UTC().Add(cooldown)
	_ = b.store.UpdateAccount(id, func(acct *accounts.Account) {
		acct.Status = status
		acct.LastError = message
		if cooldown > 0 {
			acct.CooldownUntil = &now
		}
	})
}

func ErrorStatus(httpStatus int, body string) (string, time.Duration, bool) {
	lower := strings.ToLower(body)
	switch {
	case httpStatus == http.StatusUnauthorized || httpStatus == http.StatusForbidden:
		return accounts.StatusDeactivated, 0, true
	case httpStatus == http.StatusTooManyRequests || strings.Contains(lower, "rate_limit"):
		return accounts.StatusRateLimited, 2 * time.Minute, true
	case strings.Contains(lower, "quota") || strings.Contains(lower, "usage_limit") || strings.Contains(lower, "usage_not_included"):
		return accounts.StatusQuotaExceeded, 10 * time.Minute, true
	case httpStatus >= 500:
		return accounts.StatusActive, 10 * time.Second, true
	default:
		return accounts.StatusActive, 0, false
	}
}
