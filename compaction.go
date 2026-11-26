package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	compactedContextPrefix = "codex-router.compaction.v1:"
	compactedSummaryPrefix = "Another language model started to solve this problem and produced a summary of its thinking process. You also have access to the state of the tools that were used by that language model. Use this to build on the work that has already been done and avoid duplicating work. Here is the summary produced by the other language model, use the information in this summary to assist with your own analysis:"
	maxCompactionBodyBytes = 16 << 20
)

func compactionRequested(r *http.Request, payload map[string]any) bool {
	if requestKindIsCompaction(r.Header.Get("X-Codex-Turn-Metadata")) {
		return true
	}
	if metadata, ok := payload["client_metadata"].(map[string]any); ok {
		if value, ok := metadata["x-codex-turn-metadata"].(string); ok {
			return requestKindIsCompaction(value)
		}
	}
	return false
}

func requestKindIsCompaction(raw string) bool {
	if raw == "" {
		return false
	}
	var metadata map[string]any
	if json.Unmarshal([]byte(raw), &metadata) != nil {
		return false
	}
	kind, _ := metadata["request_kind"].(string)
	compaction, _ := metadata["compaction"].(map[string]any)
	implementation, _ := compaction["implementation"].(string)
	return kind == "compaction" && implementation == "responses_compaction_v2"
}

func expandRouterCompaction(payload map[string]any) {
	input, ok := payload["input"].([]any)
	if !ok {
		return
	}
	for index, value := range input {
		item, ok := value.(map[string]any)
		if !ok || item["type"] != "compaction" {
			continue
		}
		encrypted, _ := item["encrypted_content"].(string)
		summary, ok := decodeRouterCompaction(encrypted)
		if !ok {
			continue
		}
		input[index] = map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": compactedSummaryPrefix + "\n" + summary,
			}},
		}
	}
	payload["input"] = input
}

func encodeRouterCompaction(summary string) string {
	return compactedContextPrefix + base64.RawURLEncoding.EncodeToString([]byte(summary))
}

func decodeRouterCompaction(value string) (string, bool) {
	if !strings.HasPrefix(value, compactedContextPrefix) {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, compactedContextPrefix))
	if err != nil || len(decoded) == 0 {
		return "", false
	}
	return string(decoded), true
}

type sseFrame struct {
	raw   []byte
	event string
	data  []byte
}

func readSSEFrame(reader *bufio.Reader) (sseFrame, error) {
	var raw bytes.Buffer
	var event string
	var dataLines [][]byte
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			raw.Write(line)
			trimmed := bytes.TrimRight(line, "\r\n")
			switch {
			case len(trimmed) == 0:
				return sseFrame{
					raw: raw.Bytes(), event: event,
					data: bytes.Join(dataLines, []byte("\n")),
				}, nil
			case bytes.HasPrefix(trimmed, []byte("event:")):
				event = strings.TrimSpace(string(bytes.TrimPrefix(trimmed, []byte("event:"))))
			case bytes.HasPrefix(trimmed, []byte("data:")):
				dataLines = append(dataLines, bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:"))))
			}
		}
		if err != nil {
			if raw.Len() > 0 {
				return sseFrame{
					raw: raw.Bytes(), event: event,
					data: bytes.Join(dataLines, []byte("\n")),
				}, nil
			}
			return sseFrame{}, err
		}
	}
}

func adaptThirdPartyCompaction(raw []byte) ([]byte, error) {
	reader := bufio.NewReader(bytes.NewReader(raw))
	var output bytes.Buffer
	var textDeltas strings.Builder
	var completedText strings.Builder
	compactionCount := 0
	nextOutputIndex := 0
	completed := false

	for {
		frame, err := readSSEFrame(reader)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read upstream compaction stream: %w", err)
		}
		event := parseSSEEvent(frame)
		kind := sseEventType(frame, event)
		switch kind {
		case "response.output_text.delta":
			if delta, ok := event["delta"].(string); ok {
				textDeltas.WriteString(delta)
			}
		case "response.output_text.done":
			if text, ok := event["text"].(string); ok && textDeltas.Len() == 0 {
				completedText.WriteString(text)
			}
		case "response.output_item.done":
			item, _ := event["item"].(map[string]any)
			if itemType, _ := item["type"].(string); itemType == "compaction" {
				compactionCount++
			} else if itemType == "message" {
				completedText.WriteString(messageItemText(item))
			}
		case "response.completed":
			if !completed && compactionCount == 0 {
				summary := textDeltas.String()
				if strings.TrimSpace(summary) == "" {
					summary = completedText.String()
				}
				if strings.TrimSpace(summary) == "" {
					return nil, errors.New("third-party compaction completed without a summary")
				}
				item := map[string]any{
					"id":                compactionItemID(raw, summary),
					"type":              "compaction",
					"encrypted_content": encodeRouterCompaction(strings.TrimSpace(summary)),
				}
				payload := map[string]any{
					"type":         "response.output_item.done",
					"item":         item,
					"output_index": nextOutputIndex,
				}
				encoded, encodeErr := json.Marshal(payload)
				if encodeErr != nil {
					return nil, encodeErr
				}
				fmt.Fprintf(&output, "event: response.output_item.done\ndata: %s\n\n", encoded)
			}
			completed = true
		}
		if kind == "response.output_item.done" {
			nextOutputIndex++
		}
		output.Write(frame.raw)
	}
	if !completed {
		return nil, errors.New("third-party compaction stream ended without response.completed")
	}
	return output.Bytes(), nil
}

func parseSSEEvent(frame sseFrame) map[string]any {
	var event map[string]any
	if json.Unmarshal(frame.data, &event) != nil {
		return nil
	}
	return event
}

func eventType(event map[string]any) string {
	value, _ := event["type"].(string)
	return value
}

func sseEventType(frame sseFrame, event map[string]any) string {
	if value := eventType(event); value != "" {
		return value
	}
	return frame.event
}

func messageItemText(item map[string]any) string {
	content, _ := item["content"].([]any)
	var text strings.Builder
	for _, value := range content {
		part, _ := value.(map[string]any)
		if itemText, ok := part["text"].(string); ok {
			text.WriteString(itemText)
		}
	}
	return text.String()
}

func compactionItemID(raw []byte, summary string) string {
	hash := sha256.New()
	_, _ = hash.Write(raw)
	_, _ = hash.Write([]byte(summary))
	digest := hash.Sum(nil)
	return "cmp_" + hex.EncodeToString(digest[:8])
}
