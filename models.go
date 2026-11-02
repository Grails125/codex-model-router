package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

//go:embed deepseek_models.json
var embeddedDeepSeekModels []byte

type ModelCatalog struct {
	Models []map[string]any `json:"models"`
}

type modelRegistry struct {
	mu          sync.RWMutex
	deepSeek    map[string]struct{}
	openAISlugs map[string]struct{}
}

func newModelRegistry() (*modelRegistry, []map[string]any, error) {
	var catalog ModelCatalog
	decoder := json.NewDecoder(bytes.NewReader(embeddedDeepSeekModels))
	decoder.UseNumber()
	if err := decoder.Decode(&catalog); err != nil {
		return nil, nil, fmt.Errorf("decode embedded DeepSeek catalog: %w", err)
	}
	deepSeek := make(map[string]struct{}, len(catalog.Models))
	for _, model := range catalog.Models {
		slug, ok := model["slug"].(string)
		if !ok || slug == "" {
			return nil, nil, fmt.Errorf("invalid embedded model definition")
		}
		deepSeek[slug] = struct{}{}
		model["prefer_websockets"] = false
	}
	return &modelRegistry{deepSeek: deepSeek, openAISlugs: make(map[string]struct{})}, catalog.Models, nil
}

func (m *modelRegistry) rememberOpenAI(models []map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, model := range models {
		if slug, ok := model["slug"].(string); ok && slug != "" {
			m.openAISlugs[slug] = struct{}{}
		}
	}
}

func (m *modelRegistry) isDeepSeek(slug string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.deepSeek[slug]
	return ok
}

func (m *modelRegistry) isOpenAI(slug string) bool {
	m.mu.RLock()
	if _, ok := m.openAISlugs[slug]; ok {
		m.mu.RUnlock()
		return true
	}
	m.mu.RUnlock()
	// The remote catalog remains authoritative for access. These prefixes allow
	// newly rolled-out OpenAI models to work before the catalog cache refreshes.
	return len(slug) > 4 && (slug[:4] == "gpt-" || len(slug) > 6 && slug[:6] == "codex-")
}

func (m *modelRegistry) count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.deepSeek) + len(m.openAISlugs)
}
