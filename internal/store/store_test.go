package store

import (
	"os"
	"path/filepath"
	"testing"

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

func TestPreferAccountMovesAccountFirstAndClearsSticky(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(accounts.Account{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(accounts.Account{ID: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAccount(accounts.Account{ID: "c"}); err != nil {
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
	got := []string{accts[0].ID, accts[1].ID, accts[2].ID}
	want := []string{"b", "a", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("account order = %v, want %v", got, want)
		}
	}
}

func TestUpdateStrategyPersistsSetting(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := st.UpdateStrategy(StrategyRoundRobin); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, settings, _ := reopened.Snapshot()
	if settings.Strategy != StrategyRoundRobin {
		t.Fatalf("strategy = %q, want %q", settings.Strategy, StrategyRoundRobin)
	}
}

func TestUpdateStrategyRejectsUnknownStrategy(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateStrategy("shuffle"); err == nil {
		t.Fatal("expected unknown strategy to fail")
	}
}
