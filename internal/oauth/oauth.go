package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"codex-hot-swapper/internal/accounts"
	"codex-hot-swapper/internal/store"
)

const redirectURI = "http://localhost:1455/auth/callback"

type Service struct {
	store *store.Store
	mu    sync.Mutex
	state string
	pkce  string
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	Error        any    `json:"error"`
	ErrorCode    string `json:"error_code"`
	Description  string `json:"error_description"`
	Message      string `json:"message"`
}

func New(st *store.Store) *Service {
	return &Service{store: st}
}

func (s *Service) StartLogin(ctx context.Context) (string, error) {
	accts, settings, _ := s.store.Snapshot()
	_ = accts
	verifier, challenge, err := pkcePair()
	if err != nil {
		return "", err
	}
	state, err := randomToken(32)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.state = state
	s.pkce = verifier
	s.mu.Unlock()

	if err := s.startCallbackServer(ctx); err != nil {
		return "", err
	}

	u, err := url.Parse(strings.TrimRight(settings.AuthBaseURL, "/") + "/oauth/authorize")
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", settings.OAuthClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", ensureOffline(settings.OAuthScope))
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("originator", "codex_cli")
	u.RawQuery = q.Encode()
	authURL := u.String()
	_ = openBrowser(authURL)
	return authURL, nil
}

func (s *Service) Refresh(ctx context.Context, acct accounts.Account) (accounts.Account, error) {
	_, settings, _ := s.store.Snapshot()
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", settings.OAuthClientID)
	form.Set("refresh_token", acct.RefreshToken)
	form.Set("scope", ensureOffline(settings.OAuthScope))
	tokens, err := postToken(ctx, settings.AuthBaseURL, form)
	if err != nil {
		return acct, err
	}
	acct.AccessToken = tokens.AccessToken
	acct.RefreshToken = tokens.RefreshToken
	acct.IDToken = tokens.IDToken
	acct.LastRefresh = time.Now().UTC()
	fillClaims(&acct)
	if acct.Status == "" || acct.Status == accounts.StatusDeactivated {
		acct.Status = accounts.StatusActive
	}
	return acct, s.store.UpsertAccount(acct)
}

func (s *Service) startCallbackServer(ctx context.Context) error {
	ln, err := net.Listen("tcp", "127.0.0.1:1455")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 15 * time.Second}
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = server.Shutdown(ctx)
			}()
		}()
		if errText := r.URL.Query().Get("error"); errText != "" {
			http.Error(w, errText, http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")
		s.mu.Lock()
		expectedState, verifier := s.state, s.pkce
		s.mu.Unlock()
		if code == "" || state == "" || state != expectedState || verifier == "" {
			http.Error(w, "invalid OAuth callback", http.StatusBadRequest)
			return
		}
		_, settings, _ := s.store.Snapshot()
		form := url.Values{}
		form.Set("grant_type", "authorization_code")
		form.Set("client_id", settings.OAuthClientID)
		form.Set("code", code)
		form.Set("code_verifier", verifier)
		form.Set("redirect_uri", redirectURI)
		tokens, err := postToken(r.Context(), settings.AuthBaseURL, form)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		acct := accounts.Account{
			AccessToken:  tokens.AccessToken,
			RefreshToken: tokens.RefreshToken,
			IDToken:      tokens.IDToken,
			LastRefresh:  time.Now().UTC(),
			Status:       accounts.StatusActive,
		}
		fillClaims(&acct)
		if acct.ID == "" {
			acct.ID = "account-" + time.Now().UTC().Format("20060102150405")
		}
		if err := s.store.UpsertAccount(acct); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, "<html><body><h1>Login complete</h1><p>Account %s added. You can close this tab.</p><p><a href=\"http://127.0.0.1:2455/\">Return to codex-hot-swapper</a></p></body></html>", html.EscapeString(acct.Email))
	})
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	go func() {
		_ = server.Serve(ln)
	}()
	return nil
}

func postToken(ctx context.Context, authBase string, form url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(authBase, "/")+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return tokenResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	var out tokenResponse
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode >= 400 {
		msg := out.Message
		if msg == "" {
			msg = out.Description
		}
		if msg == "" {
			msg = string(body)
		}
		return tokenResponse{}, errors.New(strings.TrimSpace(msg))
	}
	if out.AccessToken == "" || out.RefreshToken == "" || out.IDToken == "" {
		return tokenResponse{}, errors.New("OAuth response missing tokens")
	}
	return out, nil
}

func fillClaims(acct *accounts.Account) {
	claims := accounts.ClaimsFromIDToken(acct.IDToken)
	acct.Email = first(acct.Email, claims.Email, "unknown@example.local")
	acct.ChatGPTAccount = first(acct.ChatGPTAccount, claims.ChatGPTAccountID)
	acct.PlanType = first(acct.PlanType, claims.ChatGPTPlanType, "unknown")
	acct.ID = first(acct.ID, claims.ChatGPTAccountID, claims.Subject, acct.Email)
}

func pkcePair() (string, string, error) {
	verifier, err := randomToken(32)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func randomToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func ensureOffline(scope string) string {
	if strings.Contains(" "+scope+" ", " offline_access ") {
		return scope
	}
	return strings.TrimSpace(scope + " offline_access")
}

func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func openBrowser(u string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", u).Start()
	}
	if runtime.GOOS == "windows" {
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	}
	return exec.Command("xdg-open", u).Start()
}
