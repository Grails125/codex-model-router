package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func keyTestInstallation(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	dataDir := filepath.Join(home, "codex-model-router")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(RuntimeOptions{Port: defaultPort, RouterKey: upgradeTestKey, Version: Version})
	for path, data := range map[string][]byte{
		filepath.Join(dataDir, "settings.json"): raw,
		filepath.Join(dataDir, "deepseek.key"):  []byte("test-old-key"),
		filepath.Join(home, "config.toml"):      []byte("preserved configuration"),
	} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return home, dataDir
}

func TestSetKeyChangesOnlyCredentialWithoutEcho(t *testing.T) {
	home, dataDir := keyTestInstallation(t)
	before := mustRead(t, filepath.Join(dataDir, "settings.json"))
	var output bytes.Buffer
	if err := setKey([]string{"--codex-home", home, "--stdin"}, strings.NewReader("test-new-key\r\n"), &output); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, filepath.Join(dataDir, "deepseek.key"))); got != "test-new-key" {
		t.Fatal("credential was not updated")
	}
	if !bytes.Equal(before, mustRead(t, filepath.Join(dataDir, "settings.json"))) ||
		string(mustRead(t, filepath.Join(home, "config.toml"))) != "preserved configuration" {
		t.Fatal("key update modified unrelated configuration")
	}
	if strings.Contains(output.String(), "test-new-key") || strings.Contains(output.String(), "test-old-key") {
		t.Fatal("credential leaked to output")
	}
}

func TestSetKeyRejectsInvalidInputAndPreservesCredential(t *testing.T) {
	for name, input := range map[string]string{
		"empty": "\r\n", "space": "key with space", "multiline": "key\nsecond-key",
		"control": "key\x00hidden", "unicode": "key中文", "oversize": strings.Repeat("s", 4097),
		"oversize-trailing-space": "key" + strings.Repeat(" ", 5000),
	} {
		t.Run(name, func(t *testing.T) {
			home, dataDir := keyTestInstallation(t)
			var output bytes.Buffer
			err := setKey([]string{"--data-dir", dataDir, "--stdin"}, strings.NewReader(input), &output)
			if err == nil {
				t.Fatal("invalid credential accepted")
			}
			if string(mustRead(t, filepath.Join(dataDir, "deepseek.key"))) != "test-old-key" ||
				string(mustRead(t, filepath.Join(home, "config.toml"))) != "preserved configuration" {
				t.Fatal("failed update changed installation")
			}
			if input != "\r\n" && strings.Contains(err.Error()+output.String(), input) {
				t.Fatal("invalid credential leaked to diagnostics")
			}
		})
	}
}

func TestSetKeyRequiresExistingInstallationAndExplicitInput(t *testing.T) {
	var output bytes.Buffer
	missing := filepath.Join(t.TempDir(), "not-installed")
	if err := setKey([]string{"--data-dir", missing, "--stdin"}, strings.NewReader("test-new-key"), &output); err == nil {
		t.Fatal("key update created an incomplete installation")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("missing installation was created")
	}
	home, dataDir := keyTestInstallation(t)
	t.Setenv("DEEPSEEK_API_KEY", "test-env-key")
	if err := setKey([]string{"--codex-home", home}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, filepath.Join(dataDir, "deepseek.key"))); got != "test-env-key" {
		t.Fatal("environment key was not applied")
	}
	if err := setKey([]string{"--codex-home", home, "--stdin"}, strings.NewReader(""), &output); err == nil {
		t.Fatal("empty explicit stdin silently used environment credential")
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	if err := setKey([]string{"--codex-home", home}, strings.NewReader("test-key"), &output); err == nil {
		t.Fatal("implicit stdin accepted")
	}
	if err := setKey([]string{"--codex-home", home, "test-secret-positional"}, strings.NewReader(""), &output); err == nil || strings.Contains(err.Error(), "test-secret-positional") {
		t.Fatal("positional credential was accepted or echoed")
	}
}

func TestRunningRouterUsesUpdatedKeyWithoutRestart(t *testing.T) {
	home, dataDir := keyTestInstallation(t)
	var received []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = append(received, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"test-response","output":[]}`)
	}))
	defer upstream.Close()
	router, err := NewRouter(RuntimeOptions{RouterKey: upgradeTestKey, DeepSeekURL: upstream.URL}, func() (string, error) {
		return readDeepSeekKey(dataDir)
	}, func(AuditRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	request := func() {
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/backend-api/codex/responses", strings.NewReader(`{"model":"deepseek-flash","input":"synthetic probe"}`))
		r.Header.Set("X-Codex-Router-Key", upgradeTestKey)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("request failed: %d", w.Code)
		}
	}
	request()
	if err := setKey([]string{"--codex-home", home, "--stdin"}, strings.NewReader("test-rotated-key"), io.Discard); err != nil {
		t.Fatal(err)
	}
	request()
	if len(received) != 2 || received[0] != "Bearer test-old-key" || received[1] != "Bearer test-rotated-key" {
		t.Fatal("running router did not pick up rotated credential")
	}
}
