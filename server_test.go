package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type capturedRequest struct {
	Path    string
	Headers http.Header
	Body    map[string]any
}

func newTestRouter(t *testing.T) (*Router, *httptest.Server, *httptest.Server, func() []capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var captured []capturedRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		captured = append(captured, capturedRequest{Path: r.URL.Path, Headers: r.Header.Clone(), Body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
	}))
	t.Cleanup(upstream.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "catalog unavailable", http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)
	router, err := NewRouter(RuntimeOptions{
		RouterKey:   "local-secret-at-least-24-bytes",
		OpenAIURL:   upstream.URL + "/openai",
		DeepSeekURL: upstream.URL + "/deepseek",
		CatalogURL:  server.URL + "/catalog",
	}, func() (string, error) { return "deepseek-secret", nil }, func(AuditRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	return router, server, upstream, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedRequest(nil), captured...)
	}
}

func postRouter(t *testing.T, router *Router, body string, mutators ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/backend-api/codex/responses", bytes.NewBufferString(body))
	req.Header.Set("Host", "127.0.0.1:49627")
	req.Header.Set("X-Codex-Router-Key", "local-secret-at-least-24-bytes")
	req.Header.Set("Authorization", "Bearer openai-secret")
	req.Header.Set("ChatGPT-Account-ID", "account-secret")
	req.Header.Set("Cookie", "session=browser-secret")
	for _, mutate := range mutators {
		if mutate != nil {
			mutate(req)
		}
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

func TestDeepSeekGetsOnlyItsCredential(t *testing.T) {
	router, _, _, captured := newTestRouter(t)
	response := postRouter(t, router, `{"model":"deepseek-flash","stream":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	requests := captured()
	if len(requests) != 1 || requests[0].Path != "/deepseek/responses" {
		t.Fatalf("unexpected upstream request: %#v", requests)
	}
	if got := requests[0].Headers.Get("Authorization"); got != "Bearer deepseek-secret" {
		t.Fatalf("wrong DeepSeek authorization: %q", got)
	}
	if requests[0].Headers.Get("ChatGPT-Account-ID") != "" || requests[0].Headers.Get("Cookie") != "" ||
		requests[0].Headers.Get("X-Codex-Router-Key") != "" {
		t.Fatalf("foreign credentials reached DeepSeek: %#v", requests[0].Headers)
	}
}

func TestOpenAIPreservesAuthAndDropsForeignReasoning(t *testing.T) {
	router, _, _, captured := newTestRouter(t)
	body := `{"model":"gpt-6.1-sol","input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"foreign"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`
	if response := postRouter(t, router, body); response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	requests := captured()
	if len(requests) != 1 || requests[0].Path != "/openai/responses" {
		t.Fatalf("unexpected upstream request: %#v", requests)
	}
	if requests[0].Headers.Get("Authorization") != "Bearer openai-secret" ||
		requests[0].Headers.Get("ChatGPT-Account-ID") != "account-secret" {
		t.Fatalf("OpenAI auth missing: %#v", requests[0].Headers)
	}
	input := requests[0].Body["input"].([]any)
	if len(input) != 1 || input[0].(map[string]any)["type"] != "message" {
		t.Fatalf("foreign reasoning was not dropped: %#v", input)
	}
}

func TestRouterRejectsUnknownModelsAndBrowserOrigins(t *testing.T) {
	router, _, _, captured := newTestRouter(t)
	if response := postRouter(t, router, `{"model":"unknown"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown model status=%d", response.Code)
	}
	if response := postRouter(t, router, `{"model":"gpt-6.1-sol"}`, func(r *http.Request) {
		r.Header.Set("Origin", "https://example.com")
	}); response.Code != http.StatusForbidden {
		t.Fatalf("browser origin status=%d", response.Code)
	}
	if len(captured()) != 0 {
		t.Fatal("rejected requests reached upstream")
	}
}

func TestOversizedGzipIsRejected(t *testing.T) {
	router, _, _, captured := newTestRouter(t)
	var compressed bytes.Buffer
	zw := newGzipWriter(&compressed)
	_, _ = zw.Write([]byte(strings.Repeat(" ", maxBodyBytes+1)))
	_ = zw.Close()
	response := postRouter(t, router, compressed.String(), func(r *http.Request) {
		r.Header.Set("Content-Encoding", "gzip")
	})
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if len(captured()) != 0 {
		t.Fatal("oversized body reached upstream")
	}
}

func TestCatalogMergesAuthenticatedOpenAIAndDeepSeekModels(t *testing.T) {
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("client_version") != "0.166.0" {
			t.Errorf("client version not forwarded: %s", r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer openai-secret" ||
			r.Header.Get("ChatGPT-Account-ID") != "account-secret" {
			t.Errorf("catalog auth not forwarded: %#v", r.Header)
		}
		if r.Header.Get("X-Codex-Router-Key") != "" || r.Header.Get("Referer") != "" || strings.Contains(r.RequestURI, "local-secret") {
			t.Error("local credential reached catalog upstream")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-6.1-sol","display_name":"GPT-6.1-Sol","priority":1},{"slug":"gpt-6-astra","display_name":"GPT-6-Astra","priority":1000}]}`))
	}))
	defer catalog.Close()
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	router, err := NewRouter(RuntimeOptions{
		RouterKey: "local-secret-at-least-24-bytes", CatalogURL: catalog.URL,
		OpenAIURL: upstream.URL, DeepSeekURL: upstream.URL,
	}, func() (string, error) { return "deepseek-secret", nil }, func(AuditRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/backend-api/codex/models?client_version=0.166.0", nil)
	request.Header.Set("Host", "127.0.0.1")
	request.Header.Set("X-Codex-Router-Key", "local-secret-at-least-24-bytes")
	request.Header.Set("Authorization", "Bearer openai-secret")
	request.Header.Set("ChatGPT-Account-ID", "account-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{"gpt-6.1-sol", "gpt-6-astra", "deepseek-flash", "deepseek-v4-pro"}
	if len(body.Models) != len(wantOrder) {
		t.Fatalf("expected %d models, got %d", len(wantOrder), len(body.Models))
	}
	lastPriority := 0.0
	for i, want := range wantOrder {
		model := body.Models[i]
		if model["slug"] != want {
			t.Fatalf("model %d = %v, want %s", i, model["slug"], want)
		}
		priority, ok := model["priority"].(float64)
		if !ok || priority <= lastPriority {
			t.Fatalf("%s priority = %v, want greater than %v", want, model["priority"], lastPriority)
		}
		lastPriority = priority
	}
	request.URL.Path = "/__codex_router/" + router.Options.RouterKey + "/backend-api/codex/models"
	request.Header.Del("X-Codex-Router-Key")
	request.Header.Set("Referer", request.URL.String())
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("path-authenticated catalog status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPathCredentialAcceptsEscapedKey(t *testing.T) {
	router, _, _, captured := newTestRouter(t)
	router.Options.RouterKey = "local-secret/with?reserved%characters#"
	request := httptest.NewRequest(http.MethodPost,
		"http://127.0.0.1/__codex_router/"+url.PathEscape(router.Options.RouterKey)+"/backend-api/codex/responses",
		strings.NewReader(`{"model":"gpt-6.1-sol"}`))
	request.Header.Set("Authorization", "Bearer openai-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("escaped-key request status=%d", response.Code)
	}
	requests := captured()
	if len(requests) != 1 || requests[0].Path != "/openai/responses" {
		t.Fatalf("unexpected upstream request: %#v", requests)
	}
}

func TestPathCredentialRoutesWithoutLeakingCredential(t *testing.T) {
	for _, test := range []struct {
		name, model, endpoint, upstream string
		status                          int
	}{
		{"OpenAI", "gpt-6.1-sol", "/responses", "/openai/responses", http.StatusOK},
		{"DeepSeek", "deepseek-flash", "/responses", "/deepseek/responses", http.StatusOK},
		{"OpenAI compact", "gpt-6.1-sol", "/responses/compact", "/openai/responses/compact", http.StatusOK},
		{"DeepSeek compact", "deepseek-flash", "/responses/compact", "", http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			router, _, _, captured := newTestRouter(t)
			response := postRouter(t, router, `{"model":"`+test.model+`"}`, func(r *http.Request) {
				r.URL.Path = "/__codex_router/" + router.Options.RouterKey + "/backend-api/codex" + test.endpoint
				r.RequestURI = r.URL.RequestURI()
				r.Header.Del("X-Codex-Router-Key")
				r.Header.Set("Referer", r.URL.String())
			})
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			requests := captured()
			if test.upstream == "" {
				if len(requests) != 0 {
					t.Fatal("rejected request reached upstream")
				}
				return
			}
			if len(requests) != 1 || requests[0].Path != test.upstream {
				t.Fatalf("unexpected upstream request: %#v", requests)
			}
			raw, err := json.Marshal(requests[0])
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte(router.Options.RouterKey)) || strings.Contains(response.Body.String(), router.Options.RouterKey) {
				t.Fatal("local credential escaped the routing boundary")
			}
		})
	}
}

func TestPathCredentialRejectsInvalidRequests(t *testing.T) {
	key := "local-secret-at-least-24-bytes"
	for _, test := range []struct {
		name, path, host, origin string
		status                   int
	}{
		{"missing", "/__codex_router/", "127.0.0.1", "", http.StatusUnauthorized},
		{"wrong", "/__codex_router/wrong/backend-api/codex/responses", "127.0.0.1", "", http.StatusUnauthorized},
		{"empty", "/__codex_router//backend-api/codex/responses", "127.0.0.1", "", http.StatusUnauthorized},
		{"no endpoint", "/__codex_router/" + key, "127.0.0.1", "", http.StatusUnauthorized},
		{"encoded separator", "/__codex_router/" + key + "%2Fbackend-api/codex/responses", "127.0.0.1", "", http.StatusUnauthorized},
		{"encoded endpoint", "/__codex_router/" + key + "/backend-api%2Fcodex/responses", "127.0.0.1", "", http.StatusNotFound},
		{"double separator", "/__codex_router/" + key + "//backend-api/codex/responses", "127.0.0.1", "", http.StatusNotFound},
		{"traversal", "/__codex_router/" + key + "/../backend-api/codex/responses", "127.0.0.1", "", http.StatusNotFound},
		{"external host", "/__codex_router/" + key + "/backend-api/codex/responses", "example.com", "", http.StatusForbidden},
		{"browser origin", "/__codex_router/" + key + "/backend-api/codex/responses", "127.0.0.1", "https://example.com", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			router, _, _, captured := newTestRouter(t)
			request := httptest.NewRequest(http.MethodPost, "http://"+test.host+test.path, strings.NewReader(`{"model":"gpt-6.1-sol"}`))
			request.Header.Set("Authorization", "Bearer openai-secret")
			request.Header.Set("Origin", test.origin)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d want=%d", response.Code, test.status)
			}
			if len(captured()) != 0 || strings.Contains(response.Body.String(), key) {
				t.Fatal("invalid request escaped the routing boundary")
			}
		})
	}
}
