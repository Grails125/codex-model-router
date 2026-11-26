package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestDeepSeekCompactionReturnsCodexCompactionItemAndReplaysSummary(t *testing.T) {
	var mu sync.Mutex
	var captured []capturedRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		captured = append(captured, capturedRequest{Path: r.URL.Path, Headers: r.Header.Clone(), Body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\n"+
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"The task is half complete.\"}\n\n"+
			"event: response.completed\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_compact\",\"status\":\"completed\"}}\n\n")
	}))
	defer upstream.Close()
	catalog := httptest.NewServer(http.NotFoundHandler())
	defer catalog.Close()
	router, err := NewRouter(RuntimeOptions{
		RouterKey:   "local-secret-at-least-24-bytes",
		OpenAIURL:   upstream.URL + "/openai",
		DeepSeekURL: upstream.URL + "/deepseek",
		CatalogURL:  catalog.URL,
	}, func() (string, error) { return "deepseek-secret", nil }, nil)
	if err != nil {
		t.Fatal(err)
	}

	compactionRequest := `{"model":"deepseek-flash","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Summarize the task."}]}]}`
	response := postRouter(t, router, compactionRequest, func(r *http.Request) {
		r.Header.Set("X-Codex-Turn-Metadata", `{"request_kind":"compaction","compaction":{"trigger":"auto","reason":"context_limit","implementation":"responses_compaction_v2","phase":"pre_turn","strategy":"memento"}}`)
	})
	if response.Code != http.StatusOK {
		t.Fatalf("compaction status=%d body=%s", response.Code, response.Body.String())
	}
	compactionItem := findCompactionItem(t, response.Body.Bytes())
	if _, ok := decodeRouterCompaction(compactionItem["encrypted_content"].(string)); !ok {
		t.Fatal("router compaction payload was not decodable")
	}
	directReplay := map[string]any{"input": []any{map[string]any{
		"type": "compaction", "encrypted_content": compactionItem["encrypted_content"],
	}}}
	expandRouterCompaction(directReplay)
	if directReplay["input"].([]any)[0].(map[string]any)["type"] != "message" {
		t.Fatal("router compaction expansion failed in isolation")
	}

	replayBody, err := json.Marshal(map[string]any{
		"model":  "deepseek-flash",
		"stream": true,
		"input": []any{map[string]any{
			"id": compactionItem["id"], "type": "compaction",
			"encrypted_content": compactionItem["encrypted_content"],
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response = postRouter(t, router, string(replayBody))
	if response.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", response.Code, response.Body.String())
	}
	mu.Lock()
	requests := append([]capturedRequest(nil), captured...)
	mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("expected two upstream requests, got %d", len(requests))
	}
	input := requests[1].Body["input"].([]any)
	replayed := input[0].(map[string]any)
	if replayed["type"] != "message" || replayed["role"] != "user" {
		t.Fatalf("compaction item was not replayed as summary context; first=%#v second=%#v", requests[0].Body["input"], requests[1].Body["input"])
	}
	content := replayed["content"].([]any)[0].(map[string]any)
	if content["text"] != compactedSummaryPrefix+"\nThe task is half complete." {
		t.Fatalf("unexpected replayed summary: %#v", content)
	}
}

func TestCompactionRequestMayComeFromClientMetadata(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/responses", strings.NewReader("{}"))
	payload := map[string]any{"client_metadata": map[string]any{
		"x-codex-turn-metadata": `{"request_kind":"compaction","compaction":{"implementation":"responses_compaction_v2"}}`,
	}}
	if !compactionRequested(request, payload) {
		t.Fatal("compaction request in client_metadata was not detected")
	}
}

func TestLocalCompactionIsNotConvertedToRemoteCompaction(t *testing.T) {
	if requestKindIsCompaction(`{"request_kind":"compaction","compaction":{"implementation":"responses"}}`) {
		t.Fatal("local Responses compaction was classified as remote compaction v2")
	}
}

func TestOpenCodeGoCompactionUsesTheSameResponseAdapter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/go/openai/v1/responses" {
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\n"+
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"OpenCode summary\"}\n\n"+
			"event: response.completed\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_compact\",\"status\":\"completed\"}}\n\n")
	}))
	defer upstream.Close()
	router, err := NewRouter(RuntimeOptions{
		RouterKey:     "local-secret-at-least-24-bytes",
		OpenAIURL:     upstream.URL + "/openai",
		DeepSeekURL:   upstream.URL + "/deepseek",
		OpenCodeGoURL: upstream.URL + "/go",
		CatalogURL:    upstream.URL + "/catalog",
	}, func() (string, error) { return "deepseek-secret", nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	router.OpenCodeGoKey = func() (string, error) { return "opencode-secret", nil }
	response := postRouter(t, router, `{"model":"opencode-go/kimi-k3","stream":true,"input":"Summarize"}`, func(r *http.Request) {
		r.Header.Set("X-Codex-Turn-Metadata", `{"request_kind":"compaction","compaction":{"implementation":"responses_compaction_v2"}}`)
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	item := findCompactionItem(t, response.Body.Bytes())
	summary, ok := decodeRouterCompaction(item["encrypted_content"].(string))
	if !ok || summary != "OpenCode summary" {
		t.Fatalf("unexpected OpenCode compaction item: %#v", item)
	}
}

func TestThirdPartyCompactionDoesNotDuplicateAnUpstreamCompactionItem(t *testing.T) {
	raw := "event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"cmp_upstream\",\"type\":\"compaction\",\"encrypted_content\":\"opaque\"}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_compact\",\"status\":\"completed\"}}\n\n"
	adapted, err := adaptThirdPartyCompaction([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(adapted), `"type":"compaction"`); count != 1 {
		t.Fatalf("expected one upstream compaction item, got %d:\n%s", count, adapted)
	}
}

func TestThirdPartyCompactionRejectsMissingSummary(t *testing.T) {
	raw := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_compact\",\"status\":\"completed\"}}\n\n"
	if _, err := adaptThirdPartyCompaction([]byte(raw)); err == nil {
		t.Fatal("expected an empty compaction response to fail")
	}
}

func findCompactionItem(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	reader := bufio.NewReader(bytes.NewReader(raw))
	for {
		frame, err := readSSEFrame(reader)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		event := parseSSEEvent(frame)
		if eventType(event) != "response.output_item.done" {
			continue
		}
		item, _ := event["item"].(map[string]any)
		if item["type"] == "compaction" {
			return item
		}
	}
	t.Fatal("response did not contain a compaction item")
	return nil
}
