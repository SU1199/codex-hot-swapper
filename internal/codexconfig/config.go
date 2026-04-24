package codexconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const ProviderName = "codex-hot-swapper"

const ProviderBlock = `[model_providers.codex-hot-swapper]
name = "OpenAI"
base_url = "http://127.0.0.1:2455/backend-api/codex"
wire_api = "responses"
supports_websockets = false
requires_openai_auth = true`

type InstallResult struct {
	ConfigPath string
	BackupPath string
}

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

func Snippet() string {
	return fmt.Sprintf("model_provider = %q\n\n%s", ProviderName, ProviderBlock)
}

func InstallDefault(now time.Time) (InstallResult, error) {
	path, err := DefaultPath()
	if err != nil {
		return InstallResult{}, err
	}
	return Install(path, now)
}

func Install(path string, now time.Time) (InstallResult, error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return InstallResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return InstallResult{}, err
	}
	backupPath := backupName(path, now)
	if err == nil {
		if err := os.WriteFile(backupPath, data, 0o600); err != nil {
			return InstallResult{}, err
		}
	} else {
		backupPath = ""
	}
	next := Apply(string(data))
	if err := os.WriteFile(path, []byte(next), 0o600); err != nil {
		return InstallResult{}, err
	}
	return InstallResult{ConfigPath: path, BackupPath: backupPath}, nil
}

func Apply(input string) string {
	lines := strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines)+8)
	sawProvider := false
	skipProviderBlock := false
	currentTable := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if skipProviderBlock {
			if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
				skipProviderBlock = false
			} else {
				continue
			}
		}
		if trimmed == "[model_providers."+ProviderName+"]" {
			skipProviderBlock = true
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			currentTable = trimmed
		}
		if currentTable == "" && isTopLevelModelProvider(trimmed) {
			if !sawProvider {
				out = append(out, fmt.Sprintf("model_provider = %q", ProviderName))
				sawProvider = true
			}
			continue
		}
		out = append(out, line)
	}
	if !sawProvider {
		out = append([]string{fmt.Sprintf("model_provider = %q", ProviderName), ""}, out...)
	}
	text := strings.TrimRight(strings.Join(out, "\n"), "\n")
	if text != "" {
		text += "\n\n"
	}
	text += ProviderBlock + "\n"
	return text
}

func isTopLevelModelProvider(line string) bool {
	if strings.HasPrefix(line, "#") {
		return false
	}
	return strings.HasPrefix(line, "model_provider") && strings.Contains(line, "=")
}

func backupName(path string, now time.Time) string {
	return fmt.Sprintf("%s.bak-%s", path, now.Local().Format("20060102-150405"))
}
