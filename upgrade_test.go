package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const upgradeTestKey = "upgrade-test-router-key-at-least-24-bytes"

func testServerPort(t *testing.T, server *httptest.Server) int {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	value, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestHealthRejectsLegacy200AndUnprovenIdentity(t *testing.T) {
	for name, body := range map[string]string{
		"legacy":   `{"service":"codex-model-router","models":14}`,
		"plain200": `OK`,
		"forged":   `{"service":"codex-model-router","version":"` + Version + `","proof":"made-up"}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			options := RuntimeOptions{Port: testServerPort(t, server), RouterKey: upgradeTestKey, Version: Version}
			if _, err := checkHealth(options); err == nil {
				t.Fatal("foreign HTTP 200 accepted as installed router")
			}
		})
	}
}

func TestHealthAuthenticatesVersionAndInstalledInstance(t *testing.T) {
	router, err := NewRouter(RuntimeOptions{RouterKey: upgradeTestKey, Version: Version},
		func() (string, error) { return "test-deepseek-key", nil }, func(AuditRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	defer server.Close()
	options := RuntimeOptions{Port: testServerPort(t, server), RouterKey: upgradeTestKey, Version: Version}
	health, err := checkHealth(options)
	if err != nil || health.Service != "codex-model-router" || health.Proof != "" {
		t.Fatalf("valid installed instance rejected: health=%+v err=%v", health, err)
	}
	wrongKey := options
	wrongKey.RouterKey = "another-test-key"
	if _, err := checkHealth(wrongKey); err == nil {
		t.Fatal("wrong key accepted")
	}
	wrongVersion := options
	wrongVersion.Version = "older-version"
	if _, err := checkHealth(wrongVersion); err == nil {
		t.Fatal("wrong version accepted")
	}
	for name, origin := range map[string]string{"missing-auth": "", "browser": "https://example.com"} {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/health?challenge="+strings.Repeat("c", 32), nil)
		if name == "browser" {
			request.Header.Set("X-Codex-Router-Key", upgradeTestKey)
			request.Header.Set("Origin", origin)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code == http.StatusOK {
			t.Fatalf("health accepted %s", name)
		}
	}
	if port, err := selectInstallPort(options.Port, options, true); err != nil || port != options.Port {
		t.Fatalf("owned instance was not reused: port=%d err=%v", port, err)
	}
	if port, err := selectInstallPort(defaultPort, options, true); err != nil || port != options.Port {
		t.Fatalf("default reinstall abandoned owned custom port: port=%d err=%v", port, err)
	}
}

func TestInstallAvoidsLegacyPortAndKeepsCredentialOnReinstall(t *testing.T) {
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"service":"codex-model-router","models":14}`)
	}))
	defer legacy.Close()
	port := testServerPort(t, legacy)
	root := t.TempDir()
	configPath := filepath.Join(root, "config.toml")
	original := `model = "gpt-6.1-sol"
model_provider = "desktop-router"
model_catalog_json = "C:/old/catalog.json"
`
	if err := os.WriteFile(configPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "test-deepseek-key")
	arguments := []string{"--codex-home", root, "--port", strconv.Itoa(port), "--no-service"}
	if err := commandInstall(arguments); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(root, "codex-model-router")
	first, err := readSettings(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Port == port {
		t.Fatal("installer kept occupied legacy port")
	}
	config := string(mustRead(t, configPath))
	if !strings.Contains(config, fmt.Sprintf("127.0.0.1:%d/backend-api/codex", first.Port)) {
		t.Fatal("configuration and installed runtime use different ports")
	}
	if err := commandInstall(arguments); err != nil {
		t.Fatal(err)
	}
	second, err := readSettings(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if second.RouterKey != first.RouterKey {
		t.Fatal("reinstall rotated existing router credential")
	}
	receipt, err := readReceipt(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.OriginalProvider != "desktop-router" || receipt.OriginalCatalog != "C:/old/catalog.json" {
		t.Fatalf("reinstall lost initial rollback receipt: %+v", receipt)
	}
	response, err := http.Get(legacy.URL + "/health")
	if err != nil {
		t.Fatal("installer interrupted legacy listener")
	}
	response.Body.Close()
}

func TestStatusAndDoctorRejectLegacyHealthyPort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"service":"codex-model-router","models":14}`)
	}))
	defer server.Close()
	root := t.TempDir()
	dataDir := filepath.Join(root, "codex-model-router")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	options := RuntimeOptions{Port: testServerPort(t, server), RouterKey: upgradeTestKey, Version: Version}
	raw, _ := json.Marshal(options)
	for path, data := range map[string][]byte{
		filepath.Join(dataDir, "settings.json"): raw,
		filepath.Join(dataDir, "deepseek.key"):  []byte("test-deepseek-key"),
		filepath.Join(root, "config.toml"):      []byte("model_provider = \"codex-router\"\n"),
	} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := commandStatus([]string{"--codex-home", root}); err == nil {
		t.Fatal("status accepted legacy listener")
	}
	if err := commandDoctor([]string{"--codex-home", root}); err == nil {
		t.Fatal("doctor accepted legacy listener")
	}
}

func TestRestoreSnapshotsPreservesPreviousInstallState(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "settings.json")
	created := filepath.Join(root, "install.json")
	if err := os.WriteFile(old, []byte("previous-key-and-port"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshots, err := snapshotFiles(old, created)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("new-key-and-port"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(created, []byte("new-receipt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restoreSnapshots(snapshots); err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, old)) != "previous-key-and-port" {
		t.Fatal("rollback lost old settings")
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatal("rollback retained newly created receipt")
	}
}

func TestTaskIdentityRequiresMatchingBinaryAndDataDirectory(t *testing.T) {
	t.Setenv("USERDOMAIN", "TESTDOMAIN")
	t.Setenv("USERNAME", "TestUser")
	binary := `C:\Users\Test\codex-router.exe`
	dataDir := `C:\Users\Test\.codex\codex-model-router`
	xmlBytes, err := windowsTaskXML(binary, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{xmlBytes, encodeUTF16LE(xmlBytes)} {
		match, err := taskXMLMatches(raw, binary, dataDir)
		if err != nil || !match {
			t.Fatalf("matching task rejected: %v", err)
		}
		if match, _ := taskXMLMatches(raw, binary+".other", dataDir); match {
			t.Fatal("foreign binary accepted")
		}
		if match, _ := taskXMLMatches(raw, binary, dataDir+"-other"); match {
			t.Fatal("foreign data directory accepted")
		}
	}
}
