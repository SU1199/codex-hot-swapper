package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"codex-hot-swapper/internal/accounts"
	"codex-hot-swapper/internal/oauth"
	"codex-hot-swapper/internal/store"
	"codex-hot-swapper/internal/switcher"
)

type Proxy struct {
	store    *store.Store
	switcher *switcher.Switcher
	oauth    *oauth.Service
	client   *http.Client
}

func New(st *store.Store, accountSwitcher *switcher.Switcher) *Proxy {
	return &Proxy{
		store:    st,
		switcher: accountSwitcher,
		oauth:    oauth.New(st),
		client: &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   20,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
			},
		},
	}
}

func (p *Proxy) Register(mux *http.ServeMux) {
	mux.HandleFunc("/backend-api/codex/models", p.models)
	mux.HandleFunc("/backend-api/codex/responses", p.responses)
	mux.HandleFunc("/backend-api/codex/responses/compact", p.compact)
}

func (p *Proxy) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{
		modelEntry("gpt-5.3-codex", "GPT-5.3 Codex"),
		modelEntry("gpt-5.4", "GPT-5.4"),
	}})
}

func modelEntry(slug, name string) map[string]any {
	return map[string]any{
		"slug":                         slug,
		"display_name":                 name,
		"description":                  name,
		"supported_in_api":             true,
		"supports_reasoning_summaries": true,
		"supports_parallel_tool_calls": true,
		"prefer_websockets":            false,
		"context_window":               1000000,
		"input_modalities":             []string{"text", "image"},
		"supported_reasoning_levels": []map[string]string{
			{"effort": "low", "description": "Low"},
			{"effort": "medium", "description": "Medium"},
			{"effort": "high", "description": "High"},
			{"effort": "xhigh", "description": "Extra high"},
		},
	}
}

func (p *Proxy) responses(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		p.responsesWebSocket(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p.forwardHTTP(w, r, "/codex/responses", true)
}

func (p *Proxy) compact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p.forwardHTTP(w, r, "/codex/responses/compact", false)
}

func (p *Proxy) forwardHTTP(w http.ResponseWriter, r *http.Request, upstreamPath string, streaming bool) {
	log.Printf("proxy request method=%s path=%s remote=%s", r.Method, r.URL.Path, r.RemoteAddr)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sticky := switcher.StickyKey(r)
	exclude := map[string]bool{}
	var lastErr error
	maxAttempts := p.maxAttempts()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		acct, err := p.switcher.Select(sticky, exclude)
		if err != nil {
			log.Printf("proxy select failed path=%s error=%s", upstreamPath, err)
			p.store.AppendRequestLog(store.RequestLogEntry{Time: time.Now().UTC(), Path: upstreamPath, Error: err.Error(), Attempt: attempt + 1})
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		log.Printf("proxy attempt path=%s account=%s attempt=%d", upstreamPath, acct.ID, attempt+1)
		acct, err = p.ensureFresh(r.Context(), acct, false)
		if err != nil {
			log.Printf("proxy refresh failed path=%s account=%s error=%s", upstreamPath, acct.ID, err)
			p.store.AppendRequestLog(store.RequestLogEntry{Time: time.Now().UTC(), AccountID: acct.ID, Path: upstreamPath, Error: err.Error(), Attempt: attempt + 1})
			p.switcher.RecordFailure(acct.ID, accounts.StatusDeactivated, err.Error(), 0)
			exclude[acct.ID] = true
			lastErr = err
			continue
		}
		resp, err := p.doUpstream(r.Context(), r, upstreamPath, body, acct)
		if err != nil {
			log.Printf("proxy upstream failed path=%s account=%s error=%s", upstreamPath, acct.ID, err)
			p.store.AppendRequestLog(store.RequestLogEntry{Time: time.Now().UTC(), AccountID: acct.ID, Path: upstreamPath, Error: err.Error(), Attempt: attempt + 1})
			p.switcher.RecordFailure(acct.ID, accounts.StatusActive, err.Error(), 10*time.Second)
			exclude[acct.ID] = true
			lastErr = err
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized {
			_ = resp.Body.Close()
			refreshed, refreshErr := p.ensureFresh(r.Context(), acct, true)
			if refreshErr == nil {
				acct = refreshed
				resp, err = p.doUpstream(r.Context(), r, upstreamPath, body, acct)
				if err != nil {
					lastErr = err
					continue
				}
			}
		}
		if resp.StatusCode >= 400 {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			_ = resp.Body.Close()
			status, cooldown, retry := switcher.ErrorStatus(resp.StatusCode, string(data))
			log.Printf("proxy upstream status path=%s account=%s status=%d retry=%v", upstreamPath, acct.ID, resp.StatusCode, retry)
			p.store.AppendRequestLog(store.RequestLogEntry{Time: time.Now().UTC(), AccountID: acct.ID, Path: upstreamPath, Status: resp.StatusCode, Error: strings.TrimSpace(string(data)), Attempt: attempt + 1})
			p.switcher.RecordFailure(acct.ID, status, strings.TrimSpace(string(data)), cooldown)
			if retry && attempt < maxAttempts-1 {
				exclude[acct.ID] = true
				lastErr = errors.New(resp.Status)
				continue
			}
			copyHeader(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(data)
			return
		}
		p.switcher.RecordSuccess(acct.ID)
		log.Printf("proxy success path=%s account=%s status=%d", upstreamPath, acct.ID, resp.StatusCode)
		p.store.AppendRequestLog(store.RequestLogEntry{Time: time.Now().UTC(), AccountID: acct.ID, Path: upstreamPath, Status: resp.StatusCode, Attempt: attempt + 1})
		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if streaming {
			streamCopy(w, resp.Body)
		} else {
			_, _ = io.Copy(w, resp.Body)
		}
		_ = resp.Body.Close()
		return
	}
	if lastErr == nil {
		lastErr = errors.New("all account attempts exhausted")
	}
	log.Printf("proxy exhausted path=%s error=%s", upstreamPath, lastErr)
	http.Error(w, lastErr.Error(), http.StatusBadGateway)
}

func (p *Proxy) maxAttempts() int {
	accts, _, _ := p.store.Snapshot()
	if len(accts) == 0 {
		return 1
	}
	return len(accts)
}

func (p *Proxy) doUpstream(ctx context.Context, inbound *http.Request, upstreamPath string, body []byte, acct accounts.Account) (*http.Response, error) {
	_, settings, _ := p.store.Snapshot()
	req, err := http.NewRequestWithContext(ctx, inbound.Method, strings.TrimRight(settings.UpstreamBaseURL, "/")+upstreamPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = upstreamHeaders(inbound.Header, acct.AccessToken, acct.ChatGPTAccount, false)
	return p.client.Do(req)
}

func (p *Proxy) ensureFresh(ctx context.Context, acct accounts.Account, force bool) (accounts.Account, error) {
	if !force && !accounts.ShouldRefresh(acct.LastRefresh, time.Now().UTC()) {
		return acct, nil
	}
	return p.oauth.Refresh(ctx, acct)
}

func upstreamHeaders(in http.Header, accessToken, accountID string, websocketMode bool) http.Header {
	out := http.Header{}
	for key, values := range in {
		lower := strings.ToLower(key)
		if dropHeader(lower) {
			continue
		}
		for _, value := range values {
			out.Add(key, value)
		}
	}
	out.Set("Authorization", "Bearer "+accessToken)
	if accountID != "" {
		out.Set("chatgpt-account-id", accountID)
	}
	if websocketMode {
		ensureBeta(out)
	} else {
		out.Set("Content-Type", "application/json")
		if out.Get("Accept") == "" {
			out.Set("Accept", "text/event-stream")
		}
	}
	return out
}

func dropHeader(lower string) bool {
	switch lower {
	case "authorization", "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "host", "content-length", "sec-websocket-key", "sec-websocket-version", "sec-websocket-extensions", "sec-websocket-protocol":
		return true
	}
	return strings.HasPrefix(lower, "x-forwarded-") || strings.HasPrefix(lower, "cf-")
}

func ensureBeta(h http.Header) {
	const beta = "responses_websockets=2026-02-06"
	current := h.Get("openai-beta")
	if current == "" {
		h.Set("openai-beta", beta)
		return
	}
	if !strings.Contains(strings.ToLower(current), strings.ToLower(beta)) {
		h.Set("openai-beta", current+", "+beta)
	}
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		if dropHeader(strings.ToLower(key)) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func streamCopy(w http.ResponseWriter, r io.Reader) {
	buf := make([]byte, 32*1024)
	flusher, _ := w.(http.Flusher)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *Proxy) responsesWebSocket(w http.ResponseWriter, r *http.Request) {
	log.Printf("proxy websocket path=%s remote=%s", r.URL.Path, r.RemoteAddr)
	sticky := switcher.StickyKey(r)
	_, settings, _ := p.store.Snapshot()
	u, err := url.Parse(strings.TrimRight(settings.UpstreamBaseURL, "/") + "/codex/responses")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}

	exclude := map[string]bool{}
	var acct accounts.Account
	var up *websocket.Conn
	var lastErr error
	maxAttempts := p.maxAttempts()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		acct, err = p.switcher.Select(sticky, exclude)
		if err != nil {
			log.Printf("proxy websocket select failed error=%s", err)
			p.store.AppendRequestLog(store.RequestLogEntry{Time: time.Now().UTC(), Path: "/codex/responses", Error: err.Error(), Attempt: attempt + 1})
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		acct, err = p.ensureFresh(r.Context(), acct, false)
		if err != nil {
			log.Printf("proxy websocket refresh failed account=%s error=%s", acct.ID, err)
			p.store.AppendRequestLog(store.RequestLogEntry{Time: time.Now().UTC(), AccountID: acct.ID, Path: "/codex/responses", Error: err.Error(), Attempt: attempt + 1})
			p.switcher.RecordFailure(acct.ID, accounts.StatusDeactivated, err.Error(), 0)
			exclude[acct.ID] = true
			lastErr = err
			continue
		}
		header := upstreamHeaders(r.Header, acct.AccessToken, acct.ChatGPTAccount, true)
		var resp *http.Response
		up, resp, err = websocket.DefaultDialer.DialContext(r.Context(), u.String(), header)
		if err == nil {
			break
		}
		status, cooldown, retry, message := websocketDialFailure(resp, err)
		log.Printf("proxy websocket upstream failed account=%s retry=%v error=%s", acct.ID, retry, message)
		p.switcher.RecordFailure(acct.ID, status, message, cooldown)
		p.store.AppendRequestLog(store.RequestLogEntry{Time: time.Now().UTC(), AccountID: acct.ID, Path: "/codex/responses", Status: responseStatus(resp), Error: message, Attempt: attempt + 1})
		lastErr = errors.New(message)
		if retry && attempt < maxAttempts-1 {
			exclude[acct.ID] = true
			continue
		}
		break
	}
	if err != nil {
		if lastErr == nil {
			lastErr = err
		}
		http.Error(w, lastErr.Error(), http.StatusBadGateway)
		return
	}
	defer up.Close()

	down, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer down.Close()
	p.switcher.RecordSuccess(acct.ID)
	log.Printf("proxy websocket connected account=%s", acct.ID)
	p.store.AppendRequestLog(store.RequestLogEntry{Time: time.Now().UTC(), AccountID: acct.ID, Path: "/codex/responses", Status: http.StatusSwitchingProtocols})

	errc := make(chan error, 2)
	go relayWS(up, down, errc)
	go relayWS(down, up, errc)
	<-errc
}

func websocketDialFailure(resp *http.Response, err error) (string, time.Duration, bool, string) {
	if resp == nil {
		return accounts.StatusActive, 10 * time.Second, true, err.Error()
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	message := strings.TrimSpace(string(data))
	if message == "" {
		message = err.Error()
	}
	status, cooldown, retry := switcher.ErrorStatus(resp.StatusCode, message)
	return status, cooldown, retry, message
}

func responseStatus(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func relayWS(dst, src *websocket.Conn, errc chan<- error) {
	for {
		mt, msg, err := src.ReadMessage()
		if err != nil {
			errc <- err
			return
		}
		if err := dst.WriteMessage(mt, msg); err != nil {
			errc <- err
			return
		}
	}
}
