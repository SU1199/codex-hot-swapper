package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-hot-swapper/internal/accounts"
	"codex-hot-swapper/internal/store"
	"codex-hot-swapper/internal/switcher"
)

func TestUpstreamHeadersRewriteAuth(t *testing.T) {
	in := http.Header{}
	in.Set("Authorization", "Bearer local")
	in.Set("X-Codex-Session-Id", "s")
	out := upstreamHeaders(in, "upstream-token", "acct", false)
	if got := out.Get("Authorization"); got != "Bearer upstream-token" {
		t.Fatalf("auth = %q", got)
	}
	if got := out.Get("chatgpt-account-id"); got != "acct" {
		t.Fatalf("account = %q", got)
	}
	if got := out.Get("X-Codex-Session-Id"); got != "s" {
		t.Fatalf("codex header = %q", got)
	}
}

func TestWebsocketBetaHeader(t *testing.T) {
	out := upstreamHeaders(http.Header{}, "tok", "", true)
	if got := out.Get("openai-beta"); got != "responses_websockets=2026-02-06" {
		t.Fatalf("beta = %q", got)
	}
}

func TestMaxAttemptsMatchesAccountCount(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertAccount(accounts.Account{ID: "a"})
	_ = st.UpsertAccount(accounts.Account{ID: "b"})
	_ = st.UpsertAccount(accounts.Account{ID: "c"})

	proxy := New(st, switcher.New(st))
	if got := proxy.maxAttempts(); got != 3 {
		t.Fatalf("maxAttempts = %d, want 3", got)
	}
}

func TestWebsocketDialFailureUsesQuotaStatus(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"usage limit reached"}}`)),
	}
	status, cooldown, retry, message := websocketDialFailure(resp, http.ErrAbortHandler)
	if status != accounts.StatusQuotaExceeded || cooldown <= 0 || !retry {
		t.Fatalf("status=%s cooldown=%s retry=%v message=%s", status, cooldown, retry, message)
	}
}
