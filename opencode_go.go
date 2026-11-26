package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const openCodeGoProvider = "opencode-go"
const openCodeGoPrefix = openCodeGoProvider + "/"

var openCodeGoModelID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

func setOpenCodeGoSession(headers, incoming http.Header, payload map[string]any) {
	for _, name := range []string{"session_id", "x-opencode-session"} {
		if value := incoming.Get(name); value != "" {
			headers.Set(name, value)
		}
	}
	if headers.Get("session_id") == "" && headers.Get("x-opencode-session") == "" {
		// Some Codex versions send only the conversation's prompt cache key.
		if key, ok := payload["prompt_cache_key"].(string); ok && key != "" {
			hash := sha256.Sum256([]byte(key))
			headers.Set("x-opencode-session", "ses_"+hex.EncodeToString(hash[:16]))
		}
	}
}

type openCodeGoModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Limit       struct {
		Context int `json:"context"`
		Input   int `json:"input"`
	} `json:"limit"`
	Modalities struct {
		Input []string `json:"input"`
	} `json:"modalities"`
	Capabilities struct {
		Reasoning struct {
			Effort []string `json:"effort"`
		} `json:"reasoning"`
	} `json:"capabilities"`
}

func (router *Router) openCodeGoModels(ctx context.Context) []map[string]any {
	if router.OpenCodeGoKey == nil {
		return nil
	}
	key, err := router.OpenCodeGoKey()
	if err != nil || key == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	endpoint := strings.TrimRight(router.Options.OpenCodeGoURL, "/") + "/v1/models"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("User-Agent", "codex-model-router/"+router.Options.Version)
	request.Header.Set("Accept", "application/json")
	response, err := router.Client.Do(request)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil
	}
	const maxCatalogBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCatalogBytes+1))
	if err != nil || len(body) > maxCatalogBytes {
		return nil
	}
	var list struct {
		Data []openCodeGoModel `json:"data"`
	}
	if json.Unmarshal(body, &list) != nil {
		return nil
	}
	models := make([]map[string]any, 0, len(list.Data))
	for _, model := range list.Data {
		if model.Type == "chat" && openCodeGoModelID.MatchString(model.ID) {
			models = append(models, model.codexEntry(1<<30+len(models)))
		}
	}
	return models
}

func (model openCodeGoModel) codexEntry(priority int) map[string]any {
	name := model.Name
	if name == "" {
		name = model.ID
	}
	modalities := []string{"text"}
	for _, input := range model.Modalities.Input {
		if input == "image" || strings.HasPrefix(input, "image/") {
			modalities = append(modalities, "image")
			break
		}
	}
	levels := make([]map[string]string, 0, len(model.Capabilities.Reasoning.Effort))
	var defaultEffort any
	for _, effort := range model.Capabilities.Reasoning.Effort {
		switch effort {
		case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
			levels = append(levels, map[string]string{"effort": effort, "description": effort})
			if defaultEffort == nil || effort == "medium" {
				defaultEffort = effort
			}
		}
	}
	entry := map[string]any{
		"slug":                                 openCodeGoPrefix + model.ID,
		"display_name":                         name + " (OpenCode Go)",
		"description":                          model.Description,
		"visibility":                           "list",
		"priority":                             priority,
		"supported_in_api":                     true,
		"shell_type":                           "shell_command",
		"base_instructions":                    "You are Codex, a coding agent. Help the user with their task in the current workspace.",
		"input_modalities":                     modalities,
		"truncation_policy":                    map[string]any{"mode": "tokens", "limit": 10000},
		"default_reasoning_level":              defaultEffort,
		"supported_reasoning_levels":           levels,
		"default_reasoning_summary":            "none",
		"supports_reasoning_summary_parameter": false,
		"supports_reasoning_summaries":         false,
		"support_verbosity":                    false,
		"prefer_websockets":                    false,
		"experimental_supported_tools":         []string{},
	}
	window := model.Limit.Context
	if model.Limit.Input > 0 && (window <= 0 || model.Limit.Input < window) {
		window = model.Limit.Input
	}
	if window > 0 {
		entry["context_window"] = window
		entry["max_context_window"] = window
	}
	return entry
}
