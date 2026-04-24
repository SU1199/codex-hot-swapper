package codexconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestApplyPreservesOtherConfigAndInstallsProvider(t *testing.T) {
	input := `model = "gpt-5.4"
model_provider = "direct"

[profiles.work]
model_provider = "direct"
model = "gpt-5.3-codex"

[model_providers.direct]
name = "OpenAI"
base_url = "https://api.openai.com/v1"
`

	got := Apply(input)

	if !strings.Contains(got, `model_provider = "codex-hot-swapper"`) {
		t.Fatalf("missing model_provider:\n%s", got)
	}
	if !strings.Contains(got, "[profiles.work]") || !strings.Contains(got, "[model_providers.direct]") {
		t.Fatalf("did not preserve unrelated config:\n%s", got)
	}
	if !strings.Contains(got, "[profiles.work]\nmodel_provider = \"direct\"") {
		t.Fatalf("changed profile model_provider:\n%s", got)
	}
	if !strings.Contains(got, ProviderBlock) {
		t.Fatalf("missing provider block:\n%s", got)
	}
}

func TestApplyReplacesExistingProviderBlock(t *testing.T) {
	input := `model_provider = "codex-hot-swapper"

[model_providers.codex-hot-swapper]
name = "Old"
base_url = "http://old"

[profiles.default]
model = "gpt-5.4"
`

	got := Apply(input)

	if strings.Contains(got, `name = "Old"`) || strings.Contains(got, "http://old") {
		t.Fatalf("old provider block remained:\n%s", got)
	}
	if strings.Count(got, "[model_providers.codex-hot-swapper]") != 1 {
		t.Fatalf("expected one provider block:\n%s", got)
	}
	if !strings.Contains(got, "[profiles.default]") {
		t.Fatalf("lost following table:\n%s", got)
	}
}

func TestInstallWritesBackupAndConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("model = \"gpt-5.4\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Install(path, time.Date(2026, 4, 24, 12, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	if result.BackupPath == "" {
		t.Fatal("expected backup path")
	}
	backup, err := os.ReadFile(result.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "model = \"gpt-5.4\"\n" {
		t.Fatalf("unexpected backup: %q", backup)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), ProviderBlock) {
		t.Fatalf("provider not installed:\n%s", data)
	}
}
