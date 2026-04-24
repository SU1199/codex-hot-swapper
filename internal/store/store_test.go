package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"codex-hot-swapper/internal/accounts"
)

func TestStoreAtomicSaveLoad(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(accounts.Account{ID: "a", Email: "a@example.com", AccessToken: "tok", RefreshToken: "ref", Status: accounts.StatusActive}); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	accts, _, _ := st2.Snapshot()
	if len(accts) != 1 || accts[0].ID != "a" {
		t.Fatalf("unexpected accounts: %#v", accts)
	}
}

func TestStoreCorruptAccountsFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("expected corrupt accounts to fail")
	}
}

func TestPreferAccountBiasesSelectionAndClearsSticky(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := time.Now().UTC().Add(-time.Hour)
	second := time.Now().UTC().Add(-2 * time.Hour)
	if err := st.UpsertAccount(accounts.Account{ID: "a", LastSelectedAt: &first}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(accounts.Account{ID: "b", LastSelectedAt: &second}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSticky("session:1", "a"); err != nil {
		t.Fatal(err)
	}

	if err := st.PreferAccount("b"); err != nil {
		t.Fatal(err)
	}

	accts, _, runtime := st.Snapshot()
	if len(runtime.Sticky) != 0 {
		t.Fatalf("expected sticky mappings to be cleared, got %#v", runtime.Sticky)
	}
	for _, acct := range accts {
		if acct.ID == "b" && (acct.LastSelectedAt == nil || !acct.LastSelectedAt.Equal(time.Unix(0, 0).UTC())) {
			t.Fatalf("preferred account was not marked oldest: %#v", acct.LastSelectedAt)
		}
		if acct.ID == "a" && (acct.LastSelectedAt == nil || !acct.LastSelectedAt.After(time.Unix(0, 0).UTC())) {
			t.Fatalf("other account was not moved after preferred account: %#v", acct.LastSelectedAt)
		}
	}
}
