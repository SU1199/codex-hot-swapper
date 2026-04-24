package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"codex-hot-swapper/internal/accounts"
)

type Settings struct {
	UpstreamBaseURL string `json:"upstream_base_url"`
	AuthBaseURL     string `json:"auth_base_url"`
	OAuthClientID   string `json:"oauth_client_id"`
	OAuthScope      string `json:"oauth_scope"`
}

type RequestLogEntry struct {
	Time      time.Time `json:"time"`
	AccountID string    `json:"account_id,omitempty"`
	Path      string    `json:"path"`
	Status    int       `json:"status,omitempty"`
	Error     string    `json:"error,omitempty"`
	Attempt   int       `json:"attempt,omitempty"`
}

type Runtime struct {
	Sticky map[string]string `json:"sticky"`
}

type Store struct {
	mu       sync.Mutex
	dir      string
	Accounts []accounts.Account
	Settings Settings
	Runtime  Runtime
}

func OpenDefault() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return Open(filepath.Join(home, "Library", "Application Support", "codex-hot-swapper"))
}

func Open(dir string) (*Store, error) {
	s := &Store{
		dir: dir,
		Settings: Settings{
			UpstreamBaseURL: "https://chatgpt.com/backend-api",
			AuthBaseURL:     "https://auth.openai.com",
			OAuthClientID:   "app_EMoamEEZ73f0CkXaXp7hrann",
			OAuthScope:      "openid profile email offline_access",
		},
		Runtime: Runtime{Sticky: map[string]string{}},
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := loadJSON(filepath.Join(dir, "accounts.json"), &s.Accounts); err != nil {
		return nil, err
	}
	_ = loadJSON(filepath.Join(dir, "settings.json"), &s.Settings)
	_ = loadJSON(filepath.Join(dir, "runtime.json"), &s.Runtime)
	if s.Runtime.Sticky == nil {
		s.Runtime.Sticky = map[string]string{}
	}
	if s.Settings.UpstreamBaseURL == "" {
		s.Settings.UpstreamBaseURL = "https://chatgpt.com/backend-api"
	}
	if s.Settings.AuthBaseURL == "" {
		s.Settings.AuthBaseURL = "https://auth.openai.com"
	}
	if s.Settings.OAuthClientID == "" {
		s.Settings.OAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	}
	if s.Settings.OAuthScope == "" {
		s.Settings.OAuthScope = "openid profile email offline_access"
	}
	return s, nil
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) Snapshot() ([]accounts.Account, Settings, Runtime) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accts := append([]accounts.Account(nil), s.Accounts...)
	rt := Runtime{Sticky: map[string]string{}}
	for k, v := range s.Runtime.Sticky {
		rt.Sticky[k] = v
	}
	return accts, s.Settings, rt
}

func (s *Store) UpsertAccount(account accounts.Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if account.Status == "" {
		account.Status = accounts.StatusActive
	}
	for i := range s.Accounts {
		if s.Accounts[i].ID == account.ID {
			s.Accounts[i] = account
			return s.saveAccountsLocked()
		}
	}
	s.Accounts = append(s.Accounts, account)
	return s.saveAccountsLocked()
}

func (s *Store) UpdateAccount(id string, fn func(*accounts.Account)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.Accounts {
		if s.Accounts[i].ID == id {
			fn(&s.Accounts[i])
			return s.saveAccountsLocked()
		}
	}
	return fmt.Errorf("account not found: %s", id)
}

func (s *Store) DeleteAccount(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.Accounts[:0]
	for _, acct := range s.Accounts {
		if acct.ID != id {
			out = append(out, acct)
		}
	}
	s.Accounts = out
	for key, accountID := range s.Runtime.Sticky {
		if accountID == id {
			delete(s.Runtime.Sticky, key)
		}
	}
	if err := s.saveAccountsLocked(); err != nil {
		return err
	}
	return s.saveRuntimeLocked()
}

func (s *Store) PreferAccount(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for i, acct := range s.Accounts {
		if acct.ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("account not found: %s", id)
	}
	selected := s.Accounts[index]
	copy(s.Accounts[1:index+1], s.Accounts[:index])
	s.Accounts[0] = selected
	s.Runtime.Sticky = map[string]string{}
	if err := s.saveAccountsLocked(); err != nil {
		return err
	}
	return s.saveRuntimeLocked()
}

func (s *Store) SetSticky(key, accountID string) error {
	if key == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Runtime.Sticky[key] = accountID
	return s.saveRuntimeLocked()
}

func (s *Store) AppendRequestLog(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(s.dir, "requests.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(v)
}

func (s *Store) RecentRequestLogs(limit int) []RequestLogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(filepath.Join(s.dir, "requests.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var entries []RequestLogEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry RequestLogEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err == nil {
			entries = append(entries, entry)
		}
	}
	if limit <= 0 || len(entries) <= limit {
		reverse(entries)
		return entries
	}
	entries = entries[len(entries)-limit:]
	reverse(entries)
	return entries
}

func reverse(entries []RequestLogEntry) {
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
}

func (s *Store) saveAccountsLocked() error {
	return atomicWriteJSON(filepath.Join(s.dir, "accounts.json"), s.Accounts)
}

func (s *Store) saveRuntimeLocked() error {
	return atomicWriteJSON(filepath.Join(s.dir, "runtime.json"), s.Runtime)
}

func loadJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func atomicWriteJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d", path, time.Now().UnixNano())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	_ = f.Sync()
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
