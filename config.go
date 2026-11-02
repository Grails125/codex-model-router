package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var topLevelSetting = regexp.MustCompile(`^\s*(model|model_provider|model_catalog_json)\s*=\s*(.*?)\s*$`)

type InstallOptions struct {
	Port        int
	RouterKey   string
	Previous    *InstallReceipt
	InstalledAt time.Time
}

type InstallReceipt struct {
	Version          string    `json:"version"`
	OS               string    `json:"os"`
	Arch             string    `json:"arch"`
	ConfigPath       string    `json:"config_path"`
	BackupPath       string    `json:"backup_path"`
	OriginalProvider string    `json:"original_provider"`
	OriginalCatalog  string    `json:"original_catalog"`
	RouterPort       int       `json:"router_port"`
	InstalledAt      time.Time `json:"installed_at"`
}

func splitLinesPreserve(text string) (lines []string, newline string) {
	newline = "\n"
	if strings.Contains(text, "\r\n") {
		newline = "\r\n"
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.Split(text, "\n"), newline
}

func firstTable(lines []string) int {
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			return i
		}
	}
	return len(lines)
}

func isRouterTable(line string) bool {
	name := strings.TrimSpace(line)
	return name == "[model_providers.codex-router]" ||
		strings.HasPrefix(name, "[model_providers.codex-router.")
}

func removeRouterTables(lines []string) []string {
	out := make([]string, 0, len(lines))
	skipping := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			skipping = isRouterTable(line)
		}
		if !skipping {
			out = append(out, line)
		}
	}
	return out
}

func topLevelValue(lines []string, key string) (string, error) {
	limit := firstTable(lines)
	found := ""
	for i := 0; i < limit; i++ {
		match := topLevelSetting.FindStringSubmatch(lines[i])
		if match == nil || match[1] != key {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("duplicate top-level %s", key)
		}
		value, err := strconv.Unquote(strings.TrimSpace(match[2]))
		if err != nil {
			return "", fmt.Errorf("invalid %s value", key)
		}
		found = value
	}
	for i := limit; i < len(lines); i++ {
		match := topLevelSetting.FindStringSubmatch(lines[i])
		if match != nil && match[1] == key {
			return "", fmt.Errorf("%s appears after a table header", key)
		}
	}
	return found, nil
}

func setTopLevel(lines []string, key, value string) ([]string, error) {
	limit := firstTable(lines)
	replacement := key + " = " + strconv.Quote(value)
	found := -1
	for i := 0; i < limit; i++ {
		match := topLevelSetting.FindStringSubmatch(lines[i])
		if match == nil || match[1] != key {
			continue
		}
		if found >= 0 {
			return nil, fmt.Errorf("duplicate top-level %s", key)
		}
		found = i
	}
	if found >= 0 {
		lines[found] = replacement
		return lines, nil
	}
	insert := limit
	if insert == len(lines) && (insert == 0 || strings.TrimSpace(lines[insert-1]) != "") {
		lines = append(lines, "")
	} else if insert < len(lines) {
		lines = append(lines[:insert], append([]string{replacement}, lines[insert:]...)...)
		return lines, nil
	}
	lines = append(lines, replacement)
	return lines, nil
}

func removeTopLevel(lines []string, key string) []string {
	limit := firstTable(lines)
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		match := topLevelSetting.FindStringSubmatch(line)
		if i < limit && match != nil && match[1] == key {
			continue
		}
		out = append(out, line)
	}
	return out
}

func trimBlankEnd(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func ActivateConfig(original []byte, options InstallOptions) ([]byte, InstallReceipt, error) {
	if options.Port < 1024 || options.Port > 65535 {
		return nil, InstallReceipt{}, errors.New("port must be between 1024 and 65535")
	}
	if len(options.RouterKey) < 24 {
		return nil, InstallReceipt{}, errors.New("router key must contain at least 24 characters")
	}
	lines, newline := splitLinesPreserve(string(original))
	currentProvider, err := topLevelValue(lines, "model_provider")
	if err != nil {
		return nil, InstallReceipt{}, err
	}
	currentCatalog, err := topLevelValue(lines, "model_catalog_json")
	if err != nil {
		return nil, InstallReceipt{}, err
	}
	receipt := InstallReceipt{
		OriginalProvider: currentProvider,
		OriginalCatalog:  currentCatalog,
		RouterPort:       options.Port,
	}
	if options.Previous != nil {
		receipt.OriginalProvider = options.Previous.OriginalProvider
		receipt.OriginalCatalog = options.Previous.OriginalCatalog
	}
	lines = removeRouterTables(lines)
	lines = removeTopLevel(lines, "model_catalog_json")
	lines, err = setTopLevel(lines, "model_provider", "codex-router")
	if err != nil {
		return nil, InstallReceipt{}, err
	}
	lines = trimBlankEnd(lines)
	lines = append(lines,
		"",
		"[model_providers.codex-router]",
		`name = "OpenAI + DeepSeek"`,
		fmt.Sprintf(`base_url = "http://127.0.0.1:%d/backend-api/codex"`, options.Port),
		`wire_api = "responses"`,
		`requires_openai_auth = true`,
		`supports_websockets = false`,
		`http_headers = { "X-Codex-Router-Key" = `+strconv.Quote(options.RouterKey)+` }`,
		"",
	)
	return []byte(strings.Join(lines, newline)), receipt, nil
}

func RestoreConfig(active []byte, receipt InstallReceipt) ([]byte, error) {
	activeText := string(active)
	lines, newline := splitLinesPreserve(activeText)
	lines = removeRouterTables(lines)
	lines = removeTopLevel(lines, "model_catalog_json")
	var err error
	if receipt.OriginalProvider != "" {
		lines, err = setTopLevel(lines, "model_provider", receipt.OriginalProvider)
		if err != nil {
			return nil, err
		}
	} else {
		lines = removeTopLevel(lines, "model_provider")
	}
	if receipt.OriginalCatalog != "" {
		lines, err = setTopLevel(lines, "model_catalog_json", receipt.OriginalCatalog)
		if err != nil {
			return nil, err
		}
	}
	restored := strings.Join(trimBlankEnd(lines), newline)
	if strings.HasSuffix(activeText, newline) {
		restored += newline
	}
	return []byte(restored), nil
}
