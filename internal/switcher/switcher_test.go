package switcher

import (
	"testing"
	"time"

	"codex-hot-swapper/internal/accounts"
	"codex-hot-swapper/internal/store"
)

func TestSelectSkipsPausedAndCooldown(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cooldown := time.Now().UTC().Add(time.Hour)
	_ = st.UpsertAccount(accounts.Account{ID: "paused", AccessToken: "a", RefreshToken: "r", Paused: true, Status: accounts.StatusPaused})
	_ = st.UpsertAccount(accounts.Account{ID: "cool", AccessToken: "a", RefreshToken: "r", Status: accounts.StatusRateLimited, CooldownUntil: &cooldown})
	_ = st.UpsertAccount(accounts.Account{ID: "ok", AccessToken: "a", RefreshToken: "r", Status: accounts.StatusActive})
	selected, err := New(st).Select("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != "ok" {
		t.Fatalf("selected %s", selected.ID)
	}
}

func TestStickySelection(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.UpsertAccount(accounts.Account{ID: "a", AccessToken: "a", RefreshToken: "r", Status: accounts.StatusActive})
	_ = st.UpsertAccount(accounts.Account{ID: "b", AccessToken: "a", RefreshToken: "r", Status: accounts.StatusActive})
	_ = st.SetSticky("session:1", "b")
	selected, err := New(st).Select("session:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != "b" {
		t.Fatalf("selected %s", selected.ID)
	}
}
