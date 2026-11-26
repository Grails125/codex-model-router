package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenCodeGoSessionUsesPromptCacheKeyWhenHeaderIsAbsent(t *testing.T) {
	incoming := make(http.Header)
	first, second := make(http.Header), make(http.Header)
	payload := map[string]any{"prompt_cache_key": "codex-thread-fixture"}
	setOpenCodeGoSession(first, incoming, payload)
	setOpenCodeGoSession(second, incoming, payload)
	session := first.Get("x-opencode-session")
	if session == "" || session == "codex-thread-fixture" || session != second.Get("x-opencode-session") {
		t.Fatal("expected a stable session identifier derived from the prompt cache key")
	}
	incoming.Set("session_id", "native-session")
	explicit := make(http.Header)
	setOpenCodeGoSession(explicit, incoming, payload)
	if explicit.Get("session_id") != "native-session" || explicit.Get("x-opencode-session") != "" {
		t.Fatal("expected the native session header to take precedence")
	}
}

func TestOpenCodeGoCatalogAndResponses(t *testing.T) {
	const nativeCatalog = `{"models":[{"slug":"gpt-6-luna","priority":1}]}`
	const goCatalog = `{"data":[
		{"id":"gpt-6-luna","name":"GPT 6 Luna","type":"chat","limit":{"context":1000000,"input":800000},"modalities":{"input":["text","image/png"]},"capabilities":{"reasoning":{"effort":["low","medium","bogus"]}}},
		{"id":"kimi-k3","type":"chat"},
		{"id":"image-model","type":"image"},
		{"id":"../invalid","type":"chat"},
		{"id":"kimi-k3","type":"chat"}
	]}`
	openCode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer go-fixture-key" {
			t.Error("OpenCode did not receive its own key")
		}
		for _, name := range []string{"ChatGPT-Account-ID", "Cookie", "X-Codex-Router-Key", "Referer"} {
			if r.Header.Get(name) != "" {
				t.Errorf("forwarded unrelated header %s", name)
			}
		}
		if !strings.HasPrefix(r.UserAgent(), "codex-model-router/") {
			t.Error("missing router user agent")
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/models":
			io.WriteString(w, goCatalog)
		case "POST /openai/v1/responses":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload["model"] != "gpt-6-luna" || r.Header.Get("session_id") != "fixture-session" {
				t.Errorf("unexpected upstream model or session: %v", payload["model"])
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
		default:
			t.Errorf("unexpected Go request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer openCode.Close()
	openAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer openai-secret" {
			t.Error("OpenAI did not receive its own credential")
		}
		if r.Method == http.MethodGet {
			io.WriteString(w, nativeCatalog)
		} else {
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer openAI.Close()
	router, err := NewRouter(RuntimeOptions{
		RouterKey: "local-secret-at-least-24-bytes", OpenAIURL: openAI.URL, OpenCodeGoURL: openCode.URL,
	}, func() (string, error) { return "deepseek-secret", nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	router.OpenCodeGoKey = func() (string, error) { return "go-fixture-key", nil }

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/backend-api/codex/models", nil)
	request.Header.Set("X-Codex-Router-Key", router.Options.RouterKey)
	request.Header.Set("Authorization", "Bearer openai-secret")
	request.Header.Set("ChatGPT-Account-ID", "account-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var catalog ModelCatalog
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &catalog) != nil {
		t.Fatalf("catalog failed: %d %s", response.Code, response.Body.String())
	}
	want := []string{"gpt-6-luna", "opencode-go/gpt-6-luna", "opencode-go/kimi-k3", "deepseek-flash", "deepseek-v4-pro"}
	if len(catalog.Models) != len(want) {
		t.Fatalf("got %d models, want %d", len(catalog.Models), len(want))
	}
	for i, slug := range want {
		if catalog.Models[i]["slug"] != slug {
			t.Errorf("entry %d: got %v, want %s", i, catalog.Models[i]["slug"], slug)
		}
	}
	model := catalog.Models[1]
	if model["display_name"] != "GPT 6 Luna (OpenCode Go)" || model["context_window"] != float64(800000) ||
		model["default_reasoning_level"] != "medium" || len(model["supported_reasoning_levels"].([]any)) != 2 ||
		len(model["input_modalities"].([]any)) != 2 {
		t.Error("expected OpenCode model metadata to be translated for Codex")
	}
	if response := postRouter(t, router, `{"model":"gpt-6-luna","stream":true}`); response.Code != 429 {
		t.Fatalf("native model did not use the OpenAI route: %d", response.Code)
	}
	response = postRouter(t, router, `{"model":"opencode-go/gpt-6-luna","stream":true}`, func(r *http.Request) {
		r.Header.Set("session_id", "fixture-session")
	})
	if response.Code != 200 || !strings.Contains(response.Body.String(), "response.completed") {
		t.Fatalf("Go stream failed: %d %s", response.Code, response.Body.String())
	}
}

func TestOpenCodeGoCatalogFailurePreservesOtherModels(t *testing.T) {
	openCode := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer openCode.Close()
	openAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"models":[{"slug":"gpt-6-luna"}]}`)
	}))
	defer openAI.Close()
	router, err := NewRouter(RuntimeOptions{
		RouterKey: "local-secret-at-least-24-bytes", OpenAIURL: openAI.URL, OpenCodeGoURL: openCode.URL,
	}, func() (string, error) { return "deepseek-secret", nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	router.OpenCodeGoKey = func() (string, error) { return "expired-fixture-key", nil }
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/backend-api/codex/models", nil)
	request.Header.Set("X-Codex-Router-Key", router.Options.RouterKey)
	request.Header.Set("Authorization", "Bearer openai-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "gpt-6-luna") ||
		!strings.Contains(response.Body.String(), "deepseek-flash") || strings.Contains(response.Body.String(), "opencode-go/") {
		t.Fatal("Go catalog failure changed the existing picker")
	}
	router.OpenCodeGoKey = nil
	response = postRouter(t, router, `{"model":"opencode-go/kimi-k3"}`)
	if response.Code != 503 || !strings.Contains(response.Body.String(), "OpenCode Go credential") {
		t.Fatalf("expected a credential error for a Go model: %d %s", response.Code, response.Body.String())
	}
}
