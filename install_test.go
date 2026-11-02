package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallAndUninstallInIsolatedCodexHome(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(codexHome, "config.toml")
	original := `model = "gpt-6.1-sol"
model_provider = "openai"
web_search = "cached"

[desktop]
enabled-reasoning-efforts = ["low", "high"]
`
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "test-deepseek-key")
	if err := commandInstall([]string{
		"--codex-home", codexHome, "--port", "48123", "--no-service",
	}); err != nil {
		t.Fatal(err)
	}
	activated, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(mustRead(t, filepath.Join(codexHome, "codex-model-router", "settings.json"))) {
		t.Fatal("settings are not valid JSON")
	}
	if got := string(activated); !containsAll(got,
		`model_provider = "codex-router"`,
		`[model_providers.codex-router]`,
		`[desktop]`,
	) {
		t.Fatalf("unexpected activated config:\n%s", got)
	}
	if err := commandUninstall([]string{
		"--codex-home", codexHome, "--no-service",
	}); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Fatalf("uninstall did not restore exact config:\nwant:\n%s\ngot:\n%s", original, restored)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func containsAll(text string, values ...string) bool {
	for _, value := range values {
		if !contains(text, value) {
			return false
		}
	}
	return true
}

func contains(text, value string) bool {
	for i := 0; i+len(value) <= len(text); i++ {
		if text[i:i+len(value)] == value {
			return true
		}
	}
	return false
}

func TestInstallWithoutConfigUsesDefaultsAndCanUninstall(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), ".codex")
	t.Setenv("DEEPSEEK_API_KEY", "test-default-install-key")
	if err := commandInstall([]string{"--codex-home", codexHome, "--no-service"}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(codexHome, "config.toml")
	if !strings.Contains(string(mustRead(t, configPath)), `model_provider = "codex-router"`) {
		t.Fatal("first installation did not create the router configuration")
	}
	receipt, err := readReceipt(filepath.Join(codexHome, "codex-model-router"))
	if err != nil || receipt == nil || receipt.OriginalProvider != "" {
		t.Fatalf("default provider was not preserved: receipt=%+v err=%v", receipt, err)
	}
	if err := commandUninstall([]string{"--codex-home", codexHome, "--no-service"}); err != nil {
		t.Fatal(err)
	}
	if restored := strings.TrimSpace(string(mustRead(t, configPath))); restored != "" {
		t.Fatalf("uninstall did not restore default settings: %q", restored)
	}
}
