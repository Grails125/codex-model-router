package main

import (
	"strings"
	"testing"
)

func TestNativeProviderDoctorRejectsCompressedRequests(t *testing.T) {
	options := RuntimeOptions{Port: 49627, RouterKey: "router-secret-at-least-24-bytes"}
	active, _, err := ActivateConfig(nil, InstallOptions{Port: options.Port, RouterKey: options.RouterKey})
	if err != nil {
		t.Fatal(err)
	}
	for name, config := range map[string]string{
		"enabled": strings.Replace(string(active), "features.enable_request_compression = false", "features.enable_request_compression = true", 1),
		"default": strings.Replace(string(active), "features.enable_request_compression = false\n", "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if routerProviderConfigured([]byte(config), options) {
				t.Fatal("native provider with unsupported request compression reported healthy")
			}
		})
	}
}

func TestActivateConfigPreservesUnrelatedSettingsAndCanRollback(t *testing.T) {
	original := strings.Join([]string{
		`model = "gpt-6.1-sol"`,
		`model_provider = "openai"`,
		`model_catalog_json = "C:/old/catalog.json"`,
		`web_search = "cached"`,
		``,
		`[desktop]`,
		`enabled-reasoning-efforts = ["low", "high"]`,
		``,
		`[mcp_servers.docs]`,
		`enabled = true`,
		``,
	}, "\r\n")

	activated, receipt, err := ActivateConfig([]byte(original), InstallOptions{
		Port:      49627,
		RouterKey: "router-secret-at-least-24-bytes",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(activated)
	for _, want := range []string{
		`model_provider = "openai"`,
		`[model_providers.codex-router]`,
		`base_url = "http://127.0.0.1:49627/backend-api/codex"`,
		`http_headers = { "X-Codex-Router-Key" = "router-secret-at-least-24-bytes" }`,
		`[desktop]`,
		`[mcp_servers.docs]`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("activated config missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "model_catalog_json") {
		t.Fatalf("effective catalog override must be removed:\n%s", text)
	}
	if receipt.OriginalProvider != "openai" || receipt.OriginalCatalog != "C:/old/catalog.json" {
		t.Fatalf("unexpected receipt: %#v", receipt)
	}

	restored, err := RestoreConfig([]byte(text), receipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(restored), "model_providers.codex-router") {
		t.Fatalf("router table survived rollback:\n%s", restored)
	}
	for _, want := range []string{
		`model_provider = "openai"`,
		`model_catalog_json = "C:/old/catalog.json"`,
		`web_search = "cached"`,
		`[desktop]`,
		`[mcp_servers.docs]`,
	} {
		if !strings.Contains(string(restored), want) {
			t.Fatalf("restored config missing %q:\n%s", want, restored)
		}
	}
}

func TestActivateConfigIsIdempotentAndKeepsFirstReceipt(t *testing.T) {
	original := []byte("model = \"gpt-6.1-sol\"\nmodel_provider = \"openai\"\nweb_search = \"cached\"\n")
	first, receipt, err := ActivateConfig(original, InstallOptions{Port: 48123, RouterKey: "one-router-key-at-least-24b"})
	if err != nil {
		t.Fatal(err)
	}
	second, secondReceipt, err := ActivateConfig(first, InstallOptions{
		Port: 48124, RouterKey: "two-router-key-at-least-24b", Previous: &receipt,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(second)
	if strings.Count(text, "[model_providers.codex-router]") != 1 {
		t.Fatalf("duplicate router table:\n%s", text)
	}
	if !strings.Contains(text, "48124") || strings.Contains(text, "48123") {
		t.Fatalf("old router table not replaced:\n%s", text)
	}
	if secondReceipt.OriginalProvider != "openai" || secondReceipt.OriginalCatalog != "" {
		t.Fatalf("update lost original values: %#v", secondReceipt)
	}
}

func TestConfigRejectsKeysAfterFirstTable(t *testing.T) {
	bad := []byte("[desktop]\nenabled = true\nmodel_provider = \"openai\"\n")
	if _, _, err := ActivateConfig(bad, InstallOptions{Port: 49627, RouterKey: "x-router-key-at-least-24b"}); err == nil {
		t.Fatal("expected top-level provider validation failure")
	}
}

func TestActivateConfigWithDefaultOpenAIProviderCanRollback(t *testing.T) {
	for name, original := range map[string]string{
		"empty":              "",
		"unrelated settings": "model = \"gpt-6.1-sol\"\nweb_search = \"cached\"\n\n[mcp_servers.docs]\nenabled = true\n",
	} {
		t.Run(name, func(t *testing.T) {
			activated, receipt, err := ActivateConfig([]byte(original), InstallOptions{
				Port: 49627, RouterKey: "default-provider-test-key-at-least-24b",
			})
			if err != nil {
				t.Fatal(err)
			}
			if receipt.OriginalProvider != "" {
				t.Fatalf("default provider was made explicit in receipt: %+v", receipt)
			}
			if !strings.Contains(string(activated), `model_provider = "openai"`) {
				t.Fatal("router provider was not activated")
			}
			restored, err := RestoreConfig(activated, receipt)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(restored), "model_provider") || strings.Contains(string(restored), "codex-router") {
				t.Fatalf("rollback did not restore default provider: %s", restored)
			}
			if strings.TrimSpace(string(restored)) != strings.TrimSpace(original) {
				t.Fatalf("rollback changed unrelated settings: want %q, got %q", original, restored)
			}
		})
	}
}

func TestNativeOpenAIProviderKeepsHistoryIdentityAndRestoresBaseURL(t *testing.T) {
	original := []byte("model_provider = \"openai\"\nopenai_base_url = \"https://existing.example/v1\"\nweb_search = \"cached\"\n")
	active, receipt, err := ActivateConfig(original, InstallOptions{Port: 48123, RouterKey: "test-router-key-at-least-24-bytes"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`model_provider = "openai"`,
		`openai_base_url = "http://127.0.0.1:48123/__codex_router/test-router-key-at-least-24-bytes/backend-api/codex"`,
	} {
		if !strings.Contains(string(active), want) {
			t.Fatalf("native routing missing %q", want)
		}
	}
	restored, err := RestoreConfig(active, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(restored)) != strings.TrimSpace(string(original)) {
		t.Fatalf("original OpenAI base URL was not restored: %s", restored)
	}
}

func TestNativeOpenAIProviderEscapesCredentialAndPreservesURLAcrossReinstall(t *testing.T) {
	original := []byte("openai_base_url = \"https://existing.example/v1\"\n")
	first, receipt, err := ActivateConfig(original, InstallOptions{Port: 48123, RouterKey: "test-router-key-with /slash?&%=value"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), `/__codex_router/test-router-key-with%20%2Fslash%3F&%25=value/backend-api/codex`) {
		t.Fatal("router credential was not escaped as one URL path segment")
	}
	second, secondReceipt, err := ActivateConfig(first, InstallOptions{Port: 48124, RouterKey: "replacement-test-router-key-24bytes", Previous: &receipt})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreConfig(second, secondReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(restored)) != strings.TrimSpace(string(original)) {
		t.Fatalf("reinstall replaced the original OpenAI base URL: %s", restored)
	}
}

func TestNativeOpenAIUpgradeFromLegacyReceiptRestoresExistingBaseURL(t *testing.T) {
	original := []byte("model_provider = \"codex-router\"\nopenai_base_url = \"https://existing.example/v1\"\n")
	legacyReceipt := InstallReceipt{OriginalProvider: "openai"}
	active, receipt, err := ActivateConfig(original, InstallOptions{
		Port: 48123, RouterKey: "test-router-key-at-least-24-bytes", Previous: &legacyReceipt,
	})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreConfig(active, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), `openai_base_url = "https://existing.example/v1"`) {
		t.Fatal("upgrade lost the existing OpenAI base URL")
	}
	legacyRestored, err := RestoreConfig(original, legacyReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(legacyRestored), `openai_base_url = "https://existing.example/v1"`) {
		t.Fatal("legacy uninstall changed an unmanaged OpenAI base URL")
	}
}

func TestActivateConfigAcceptsTOMLStringURLsAndInlineComments(t *testing.T) {
	for name, setting := range map[string]string{
		"literal":              `openai_base_url = 'https://existing.example/v1#literal'`,
		"basic with comment":   `openai_base_url = "https://existing.example/v1#literal" # original endpoint`,
		"literal with comment": `openai_base_url = 'https://existing.example/v1#literal' # original endpoint`,
	} {
		t.Run(name, func(t *testing.T) {
			original := []byte("model_provider = 'openai' # builtin\n" + setting + "\n")
			active, receipt, err := ActivateConfig(original, InstallOptions{
				Port: 48123, RouterKey: "test-router-key-at-least-24-bytes",
			})
			if err != nil {
				t.Fatal(err)
			}
			restored, err := RestoreConfig(active, receipt)
			if err != nil {
				t.Fatal(err)
			}
			lines, _ := splitLinesPreserve(string(restored))
			baseURL, err := topLevelValue(lines, "openai_base_url")
			if err != nil || baseURL != "https://existing.example/v1#literal" {
				t.Fatalf("OpenAI URL did not roundtrip: value=%q err=%v", baseURL, err)
			}
		})
	}
}

func TestNativeRoutingDisablesRequestCompressionAndRestoresOriginal(t *testing.T) {
	for name, original := range map[string]string{
		"empty":                    "",
		"feature table enabled":    "[features]\nenable_request_compression = true # keep this comment\nother_feature = true\n",
		"feature table disabled":   "[features]\nenable_request_compression = false\nother_feature = true\n",
		"feature table absent key": "[features]\nother_feature = true\n",
		"quoted feature table":     "[ \"features\" ]\n'enable_request_compression' = true\n",
		"dotted enabled":           "features.enable_request_compression = true # dotted\nfeatures.other_feature = true\n",
		"spaced dotted enabled":    "\"features\" . 'enable_request_compression' = true\n",
		"other dotted feature":     "features.other_feature = true\n",
	} {
		t.Run(name, func(t *testing.T) {
			active, receipt, err := ActivateConfig([]byte(original), InstallOptions{Port: 48123, RouterKey: "test-router-key-at-least-24-bytes"})
			if err != nil {
				t.Fatal(err)
			}
			lines, _ := splitLinesPreserve(string(active))
			setting, _, err := requestCompressionLocation(lines)
			if err != nil || setting < 0 || !strings.Contains(lines[setting], "= false") {
				t.Fatal("native routing did not disable request compression")
			}
			second, secondReceipt, err := ActivateConfig(active, InstallOptions{Port: 48123, RouterKey: "test-router-key-at-least-24-bytes", Previous: &receipt})
			if err != nil {
				t.Fatal(err)
			}
			restored, err := RestoreConfig(second, secondReceipt)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(restored)) != strings.TrimSpace(original) {
				t.Fatalf("request compression rollback changed unrelated configuration:\nwant: %s\ngot: %s", original, restored)
			}
		})
	}
}

func TestNativeRoutingCompressionUpgradeFromLegacyReceipt(t *testing.T) {
	original := []byte("model_provider = \"openai\"\n[features]\nenable_request_compression = true\n")
	legacy := InstallReceipt{OriginalProvider: "openai"}
	active, receipt, err := ActivateConfig(original, InstallOptions{Port: 48123, RouterKey: "test-router-key-at-least-24-bytes", Previous: &legacy})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(active), "enable_request_compression = false") {
		t.Fatal("legacy upgrade did not disable request compression")
	}
	restored, err := RestoreConfig(active, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(restored)) != strings.TrimSpace(string(original)) {
		t.Fatal("legacy upgrade lost original request compression value")
	}
}

func TestActivateConfigRejectsInlineFeaturesTable(t *testing.T) {
	for _, key := range []string{"features", `"features"`, "'features'"} {
		t.Run(key, func(t *testing.T) {
			original := []byte(key + " = { unrelated = true }\n")
			active, _, err := ActivateConfig(original, InstallOptions{
				Port: 48123, RouterKey: "test-router-key-at-least-24-bytes",
			})
			if err == nil || !strings.Contains(err.Error(), "top-level features") || active != nil {
				t.Fatalf("expected an actionable preflight error, got %v", err)
			}
		})
	}
}
