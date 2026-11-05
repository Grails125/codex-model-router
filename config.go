package main

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var topLevelSetting = regexp.MustCompile(`^\s*(model|model_provider|model_catalog_json|openai_base_url)\s*=\s*(.*?)\s*$`)
var tomlStringValue = regexp.MustCompile(`^("(?:\\.|[^"\\])*"|'[^']*')\s*(?:#.*)?$`)

const featuresKey = `(?:features|"features"|'features')`
const requestCompressionKey = `(?:enable_request_compression|"enable_request_compression"|'enable_request_compression')`

var featuresTable = regexp.MustCompile(`^\[\s*` + featuresKey + `\s*\]\s*(?:#.*)?$`)
var compressionDottedKey = regexp.MustCompile(`^` + featuresKey + `\s*\.\s*` + requestCompressionKey + `$`)
var compressionTableKey = regexp.MustCompile(`^` + requestCompressionKey + `$`)
var compressionBoolValue = regexp.MustCompile(`^(\s*)(true|false)(\s*(?:#.*)?)$`)

type InstallOptions struct {
	Port        int
	RouterKey   string
	Previous    *InstallReceipt
	InstalledAt time.Time
}

type InstallReceipt struct {
	Version                    string    `json:"version"`
	OS                         string    `json:"os"`
	Arch                       string    `json:"arch"`
	ConfigPath                 string    `json:"config_path"`
	BackupPath                 string    `json:"backup_path"`
	OriginalProvider           string    `json:"original_provider"`
	OriginalCatalog            string    `json:"original_catalog"`
	OriginalOpenAIBaseURL      *string   `json:"original_openai_base_url,omitempty"`
	OriginalRequestCompression *string   `json:"original_request_compression,omitempty"`
	RouterPort                 int       `json:"router_port"`
	InstalledAt                time.Time `json:"installed_at"`
}

func nativeOpenAIBaseURL(port int, key string) string {
	return fmt.Sprintf("http://127.0.0.1:%d/__codex_router/%s/backend-api/codex", port, url.PathEscape(key))
}

// Return the setting line and the existing features table, supporting both
// table entries and top-level dotted keys without rewriting other features.
func requestCompressionLocation(lines []string) (setting, table int, err error) {
	setting, table = -1, -1
	inFeatures, topLevel := false, true
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			topLevel = false
			inFeatures = featuresTable.MatchString(trimmed)
			if inFeatures {
				table = index
			}
			continue
		}
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found {
			continue
		}
		if topLevel && (key == "features" || key == `"features"` || key == "'features'") {
			return -1, -1, errors.New("top-level features must use a [features] table or dotted keys")
		}
		if !((inFeatures && compressionTableKey.MatchString(key)) || (topLevel && compressionDottedKey.MatchString(key))) {
			continue
		}
		if setting >= 0 || !compressionBoolValue.MatchString(value) {
			return -1, -1, errors.New("invalid or duplicate request compression setting")
		}
		setting = index
	}
	return setting, table, nil
}

func disableRequestCompression(lines []string) ([]string, string, error) {
	setting, table, err := requestCompressionLocation(lines)
	if err != nil {
		return nil, "", err
	}
	if setting >= 0 {
		original := lines[setting]
		separator := strings.Index(original, "=")
		value := compressionBoolValue.FindStringSubmatch(original[separator+1:])
		lines[setting] = original[:separator+1] + value[1] + "false" + value[3]
		return lines, original, nil
	}
	insert := firstTable(lines)
	line := "features.enable_request_compression = false"
	if table >= 0 {
		insert = table + 1
		line = "enable_request_compression = false"
	}
	lines = append(lines[:insert], append([]string{line}, lines[insert:]...)...)
	return lines, "", nil
}

func routerProviderConfigured(raw []byte, options RuntimeOptions) bool {
	lines, _ := splitLinesPreserve(string(raw))
	provider, err := topLevelValue(lines, "model_provider")
	if err != nil {
		return false
	}
	if provider == "openai" || provider == "" {
		baseURL, err := topLevelValue(lines, "openai_base_url")
		if err != nil || baseURL != nativeOpenAIBaseURL(options.Port, options.RouterKey) {
			return false
		}
		setting, _, err := requestCompressionLocation(lines)
		if err != nil || setting < 0 {
			return false
		}
		_, value, _ := strings.Cut(lines[setting], "=")
		return compressionBoolValue.FindStringSubmatch(value)[2] == "false"
	}
	if provider != "codex-router" {
		return false
	}
	// Earlier installations used this generated table. Check its connection
	// and credential together so a stale port or key is not reported healthy.
	inRouterTable, matchingURL, matchingKey := false, false, false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inRouterTable = trimmed == "[model_providers.codex-router]"
			continue
		}
		if inRouterTable {
			matchingURL = matchingURL || trimmed == fmt.Sprintf(`base_url = "http://127.0.0.1:%d/backend-api/codex"`, options.Port)
			matchingKey = matchingKey || trimmed == `http_headers = { "X-Codex-Router-Key" = `+strconv.Quote(options.RouterKey)+` }`
		}
	}
	return matchingURL && matchingKey
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
		quoted := tomlStringValue.FindStringSubmatch(strings.TrimSpace(match[2]))
		if quoted == nil {
			return "", fmt.Errorf("invalid %s value", key)
		}
		value := quoted[1][1 : len(quoted[1])-1]
		if quoted[1][0] == '"' {
			var err error
			value, err = strconv.Unquote(quoted[1])
			if err != nil {
				return "", fmt.Errorf("invalid %s value", key)
			}
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
	currentOpenAIBaseURL, err := topLevelValue(lines, "openai_base_url")
	if err != nil {
		return nil, InstallReceipt{}, err
	}
	lines, originalRequestCompression, err := disableRequestCompression(lines)
	if err != nil {
		return nil, InstallReceipt{}, err
	}
	receipt := InstallReceipt{
		OriginalProvider:           currentProvider,
		OriginalCatalog:            currentCatalog,
		OriginalOpenAIBaseURL:      &currentOpenAIBaseURL,
		OriginalRequestCompression: &originalRequestCompression,
		RouterPort:                 options.Port,
	}
	if options.Previous != nil {
		receipt.OriginalProvider = options.Previous.OriginalProvider
		receipt.OriginalCatalog = options.Previous.OriginalCatalog
		// Older receipts did not manage this setting, so capture its current
		// value once when upgrading an existing installation.
		if options.Previous.OriginalOpenAIBaseURL != nil {
			receipt.OriginalOpenAIBaseURL = options.Previous.OriginalOpenAIBaseURL
		}
		if options.Previous.OriginalRequestCompression != nil {
			receipt.OriginalRequestCompression = options.Previous.OriginalRequestCompression
		}
	}
	lines = removeRouterTables(lines)
	lines = removeTopLevel(lines, "model_catalog_json")
	lines, err = setTopLevel(lines, "model_provider", "openai")
	if err != nil {
		return nil, InstallReceipt{}, err
	}
	lines, err = setTopLevel(lines, "openai_base_url", nativeOpenAIBaseURL(options.Port, options.RouterKey))
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
	if receipt.OriginalOpenAIBaseURL != nil {
		if *receipt.OriginalOpenAIBaseURL == "" {
			lines = removeTopLevel(lines, "openai_base_url")
		} else {
			lines, err = setTopLevel(lines, "openai_base_url", *receipt.OriginalOpenAIBaseURL)
			if err != nil {
				return nil, err
			}
		}
	}
	if receipt.OriginalRequestCompression != nil {
		setting, _, locateErr := requestCompressionLocation(lines)
		if locateErr != nil {
			return nil, locateErr
		}
		if setting >= 0 {
			if *receipt.OriginalRequestCompression == "" {
				lines = append(lines[:setting], lines[setting+1:]...)
			} else {
				lines[setting] = *receipt.OriginalRequestCompression
			}
		}
	}
	restored := strings.Join(trimBlankEnd(lines), newline)
	if strings.HasSuffix(activeText, newline) {
		restored += newline
	}
	return []byte(restored), nil
}
