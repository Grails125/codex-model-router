package main

import (
	"strings"
	"testing"
)

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
		`model_provider = "codex-router"`,
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
			if !strings.Contains(string(activated), `model_provider = "codex-router"`) {
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
