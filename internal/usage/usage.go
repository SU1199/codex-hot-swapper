package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codex-hot-swapper/internal/accounts"
	"codex-hot-swapper/internal/oauth"
	"codex-hot-swapper/internal/store"
)

type Service struct {
	store  *store.Store
	oauth  *oauth.Service
	client *http.Client
}

type payload struct {
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		Primary   *accounts.UsageWindow `json:"primary_window"`
		Secondary *accounts.UsageWindow `json:"secondary_window"`
	} `json:"rate_limit"`
	Credits *accounts.Credits `json:"credits"`
	Message string            `json:"message"`
}

func New(st *store.Store) *Service {
	return &Service{store: st, oauth: oauth.New(st), client: &http.Client{Timeout: 12 * time.Second}}
}

func (s *Service) RefreshAll(ctx context.Context) {
	accts, _, _ := s.store.Snapshot()
	for _, acct := range accts {
		_ = s.RefreshAccount(ctx, acct.ID)
	}
}

func (s *Service) RefreshLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RefreshAll(ctx)
		}
	}
}

func (s *Service) RefreshAccount(ctx context.Context, id string) error {
	accts, _, _ := s.store.Snapshot()
	var acct accounts.Account
	found := false
	for _, candidate := range accts {
		if candidate.ID == id {
			acct = candidate
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("account not found: %s", id)
	}
	if accounts.ShouldRefresh(acct.LastRefresh, time.Now().UTC()) {
		refreshed, err := s.oauth.Refresh(ctx, acct)
		if err != nil {
			return s.recordError(id, err)
		}
		acct = refreshed
	}
	state, err := s.fetch(ctx, acct)
	if err != nil {
		if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "403") {
			refreshed, refreshErr := s.oauth.Refresh(ctx, acct)
			if refreshErr == nil {
				acct = refreshed
				state, err = s.fetch(ctx, acct)
			}
		}
		if err != nil {
			return s.recordError(id, err)
		}
	}
	return s.store.UpdateAccount(id, func(a *accounts.Account) {
		a.Usage = state
		if state.PlanType != "" {
			a.PlanType = state.PlanType
		}
	})
}

func (s *Service) fetch(ctx context.Context, acct accounts.Account) (accounts.UsageState, error) {
	_, settings, _ := s.store.Snapshot()
	base := strings.TrimRight(settings.UpstreamBaseURL, "/")
	if !strings.Contains(base, "/backend-api") {
		base += "/backend-api"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/wham/usage", nil)
	if err != nil {
		return accounts.UsageState{}, err
	}
	req.Header.Set("Authorization", "Bearer "+acct.AccessToken)
	req.Header.Set("Accept", "application/json")
	if acct.ChatGPTAccount != "" {
		req.Header.Set("chatgpt-account-id", acct.ChatGPTAccount)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return accounts.UsageState{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var data payload
	_ = json.Unmarshal(body, &data)
	if resp.StatusCode >= 400 {
		msg := data.Message
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		return accounts.UsageState{}, fmt.Errorf("usage fetch failed (%d): %s", resp.StatusCode, msg)
	}
	if data.RateLimit == nil && data.Credits == nil {
		return accounts.UsageState{}, errors.New("usage payload missing limits")
	}
	now := time.Now().UTC()
	state := accounts.UsageState{PlanType: data.PlanType, Credits: data.Credits, LastFetched: &now}
	if data.RateLimit != nil {
		state.Primary = data.RateLimit.Primary
		state.Secondary = data.RateLimit.Secondary
	}
	return state, nil
}

func (s *Service) recordError(id string, err error) error {
	now := time.Now().UTC()
	_ = s.store.UpdateAccount(id, func(a *accounts.Account) {
		a.Usage.LastFetched = &now
		a.Usage.LastError = err.Error()
	})
	return err
}
