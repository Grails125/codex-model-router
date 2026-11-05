package main

import (
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxBodyBytes = 64 << 20

type RuntimeOptions struct {
	Port        int    `json:"port"`
	RouterKey   string `json:"router_key"`
	OpenAIURL   string `json:"openai_url,omitempty"`
	DeepSeekURL string `json:"deepseek_url,omitempty"`
	CatalogURL  string `json:"catalog_url,omitempty"`
	Version     string `json:"version,omitempty"`
}

type AuditRecord struct {
	upstreamDiagnostics
	Time     string `json:"time"`
	Model    string `json:"model"`
	Upstream string `json:"upstream"`
	Status   int    `json:"status"`
	Stream   bool   `json:"stream"`
}

type Router struct {
	Options     RuntimeOptions
	Registry    *modelRegistry
	DeepSeek    []map[string]any
	DeepSeekKey func() (string, error)
	Audit       func(AuditRecord)
	Client      *http.Client
}

var hopHeaders = map[string]bool{
	"connection": true, "content-length": true, "keep-alive": true,
	"proxy-authenticate": true, "proxy-authorization": true, "te": true,
	"trailer": true, "transfer-encoding": true, "upgrade": true,
}

func normalizedRuntime(options RuntimeOptions) RuntimeOptions {
	if options.OpenAIURL == "" {
		options.OpenAIURL = "https://chatgpt.com/backend-api/codex"
	}
	if options.DeepSeekURL == "" {
		options.DeepSeekURL = "https://api.deepseek.com"
	}
	if options.CatalogURL == "" {
		options.CatalogURL = strings.TrimRight(options.OpenAIURL, "/") + "/models"
	}
	if options.Version == "" {
		options.Version = Version
	}
	return options
}

func NewRouter(options RuntimeOptions, key func() (string, error), audit func(AuditRecord)) (*Router, error) {
	registry, deepSeek, err := newModelRegistry()
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	transport.ResponseHeaderTimeout = 300 * time.Second
	transport.IdleConnTimeout = 90 * time.Second
	transport.MaxIdleConnsPerHost = 16
	transport.OnProxyConnectResponse = func(ctx context.Context, _ *url.URL, _ *http.Request, response *http.Response) error {
		if state, ok := ctx.Value(upstreamTraceKey{}).(*upstreamTrace); ok {
			state.mu.Lock()
			state.stage, state.proxyStatus = "proxy_connect", response.StatusCode
			state.mu.Unlock()
		}
		return nil
	}
	return &Router{
		Options: normalizedRuntime(options), Registry: registry, DeepSeek: deepSeek,
		DeepSeekKey: key, Audit: audit,
		Client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func (router *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !localHost(r.Host) {
		writeError(w, http.StatusForbidden, "Only loopback requests are accepted")
		return
	}
	const pathCredentialPrefix = "/__codex_router/"
	if strings.HasPrefix(r.URL.EscapedPath(), pathCredentialPrefix) {
		escapedKey, endpoint, found := strings.Cut(strings.TrimPrefix(r.URL.EscapedPath(), pathCredentialPrefix), "/")
		key, err := url.PathUnescape(escapedKey)
		if !found || err != nil || router.Options.RouterKey == "" || !constantTimeEqual(key, router.Options.RouterKey) {
			writeError(w, http.StatusUnauthorized, "Local router credential required")
			return
		}
		// Remove the local credential before endpoint handling or upstream requests.
		r = r.Clone(r.Context())
		r.URL.Path, r.URL.RawPath = "/"+endpoint, ""
		r.RequestURI = r.URL.RequestURI()
	} else if !router.authorized(r) {
		writeError(w, http.StatusUnauthorized, "Local router credential required")
		return
	}
	if r.Header.Get("Origin") != "" {
		writeError(w, http.StatusForbidden, "Browser-origin requests are not accepted")
		return
	}
	if r.URL.Path == "/health" && r.Method == http.MethodGet {
		challenge := r.URL.Query().Get("challenge")
		if len(challenge) < 24 || len(challenge) > 128 {
			writeError(w, http.StatusBadRequest, "Health challenge required")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"service": "codex-model-router", "version": router.Options.Version,
			"models": router.Registry.count(), "proof": healthProof(router.Options.RouterKey, challenge),
		})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/backend-api/codex/models" {
		router.models(w, r)
		return
	}
	if r.Method == http.MethodPost &&
		(r.URL.Path == "/backend-api/codex/responses" || r.URL.Path == "/backend-api/codex/responses/compact") {
		router.responses(w, r)
		return
	}
	writeError(w, http.StatusNotFound, "Unsupported endpoint")
}

func healthProof(key, challenge string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte("codex-model-router/health/" + challenge))
	return hex.EncodeToString(mac.Sum(nil))
}

func localHost(host string) bool {
	name := host
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		name = parsed
	}
	return name == "127.0.0.1" || name == "localhost" || name == "::1"
}

func (router *Router) authorized(r *http.Request) bool {
	return router.Options.RouterKey != "" &&
		constantTimeEqual(r.Header.Get("X-Codex-Router-Key"), router.Options.RouterKey)
}

func constantTimeEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for i := range left {
		difference |= left[i] ^ right[i]
	}
	return difference == 0
}

func copyOpenAIHeaders(destination, source http.Header) {
	for key, values := range source {
		lower := strings.ToLower(key)
		if hopHeaders[lower] || lower == "cookie" || lower == "origin" || lower == "referer" ||
			lower == "x-codex-router-key" || lower == "content-encoding" {
			continue
		}
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func (router *Router) models(w http.ResponseWriter, r *http.Request) {
	catalogURL, err := url.Parse(router.Options.CatalogURL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Invalid catalog endpoint")
		return
	}
	query := catalogURL.Query()
	if value := r.URL.Query().Get("client_version"); value != "" {
		query.Set("client_version", value)
	}
	catalogURL.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, catalogURL.String(), nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Unable to create catalog request")
		return
	}
	copyOpenAIHeaders(request.Header, r.Header)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	if request.Header.Get("Authorization") == "" {
		writeError(w, http.StatusUnauthorized, "ChatGPT authentication is required")
		return
	}
	response, err := router.Client.Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Unable to fetch OpenAI model catalog")
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		writeError(w, response.StatusCode, "OpenAI model catalog request failed")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		writeError(w, http.StatusBadGateway, "Invalid OpenAI model catalog")
		return
	}
	var upstream map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&upstream); err != nil {
		writeError(w, http.StatusBadGateway, "Invalid OpenAI model catalog")
		return
	}
	models, ok := upstream["models"].([]any)
	if !ok {
		writeError(w, http.StatusBadGateway, "Invalid OpenAI model catalog")
		return
	}
	seen := make(map[string]bool)
	normalizedOpenAI := make([]map[string]any, 0, len(models))
	for _, value := range models {
		model, ok := value.(map[string]any)
		if !ok {
			continue
		}
		slug, _ := model["slug"].(string)
		if slug == "" {
			continue
		}
		seen[slug] = true
		normalizedOpenAI = append(normalizedOpenAI, model)
	}
	combined := make([]any, 0, len(models)+len(router.DeepSeek))
	for _, value := range models {
		combined = append(combined, value)
	}
	for _, model := range router.DeepSeek {
		slug, _ := model["slug"].(string)
		if slug != "" && !seen[slug] {
			combined = append(combined, model)
		}
	}
	upstream["models"] = combined
	router.Registry.rememberOpenAI(normalizedOpenAI)
	writeJSON(w, http.StatusOK, upstream)
}

func (router *Router) responses(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	body, payload, err := readRequestBody(r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, err.Error())
		return
	}
	model, _ := payload["model"].(string)
	if model == "" {
		writeError(w, http.StatusBadRequest, "Responses request requires a model")
		return
	}
	provider := ""
	if router.Registry.isDeepSeek(model) {
		provider = "deepseek"
	} else if router.Registry.isOpenAI(model) {
		provider = "openai"
	} else {
		writeError(w, http.StatusBadRequest, "Select a model from the authenticated catalog")
		return
	}
	if provider == "deepseek" && strings.HasSuffix(r.URL.Path, "/compact") {
		writeError(w, http.StatusBadRequest, "DeepSeek uses local conversation compaction")
		return
	}
	upstreamURL := router.Options.OpenAIURL
	headers := make(http.Header)
	if provider == "deepseek" {
		upstreamURL = router.Options.DeepSeekURL
		key, err := router.DeepSeekKey()
		if err != nil || key == "" {
			writeError(w, http.StatusServiceUnavailable, "DeepSeek credential unavailable")
			return
		}
		headers.Set("Authorization", "Bearer "+key)
		headers.Set("Content-Type", "application/json")
		headers.Set("Accept", "text/event-stream")
	} else {
		copyOpenAIHeaders(headers, r.Header)
		if !strings.HasPrefix(headers.Get("Authorization"), "Bearer ") {
			writeError(w, http.StatusUnauthorized, "ChatGPT authentication is required")
			return
		}
		removeForeignReasoning(payload)
		body, err = json.Marshal(payload)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid Responses request")
			return
		}
	}
	headers.Set("Accept-Encoding", "identity")
	suffix := "/responses"
	if strings.HasSuffix(r.URL.Path, "/compact") {
		suffix = "/responses/compact"
	}
	upstreamURL = strings.TrimRight(upstreamURL, "/") + suffix
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, strings.NewReader(string(body)))
	if err != nil {
		writeError(w, http.StatusBadGateway, "Unable to create upstream request")
		return
	}
	request.Header = headers
	started := time.Now()
	response, diagnostic, err := router.doUpstream(request)
	if err != nil {
		status := http.StatusBadGateway
		if r.Context().Err() != nil {
			status = 499
		}
		writeError(w, status, safeUpstreamError(diagnostic))
		router.audit(model, provider, status, payload, diagnostic)
		return
	}
	defer response.Body.Close()
	for _, key := range []string{"Content-Type", "Retry-After", "X-Request-ID", "Content-Encoding"} {
		if value := response.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(response.StatusCode)
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32<<10)
	for {
		count, readErr := response.Body.Read(buffer)
		if count > 0 {
			if _, err := w.Write(buffer[:count]); err != nil {
				diagnostic.Stage, diagnostic.ErrorClass = "client_write", "client_disconnected"
				diagnostic.ElapsedMS = time.Since(started).Milliseconds()
				router.audit(model, provider, 499, payload, diagnostic)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			diagnostic.Stage = "response_body"
			diagnostic.ErrorClass, diagnostic.Errno, _ = classifyTransportError(readErr, diagnostic.Stage, 0)
			diagnostic.ElapsedMS = time.Since(started).Milliseconds()
			status := http.StatusBadGateway
			if r.Context().Err() != nil {
				status = 499
				diagnostic.ErrorClass = "cancelled"
			}
			router.audit(model, provider, status, payload, diagnostic)
			// Headers may already advertise success. Abort the HTTP stream so
			// a truncated upstream response cannot become a clean downstream EOF.
			panic(http.ErrAbortHandler)
		}
	}
	diagnostic.Stage = "completed"
	diagnostic.ElapsedMS = time.Since(started).Milliseconds()
	router.audit(model, provider, response.StatusCode, payload, diagnostic)
}

var errTooLarge = errors.New("request is too large")

func readRequestBody(r *http.Request) ([]byte, map[string]any, error) {
	if r.ContentLength <= 0 || r.ContentLength > maxBodyBytes {
		return nil, nil, errTooLarge
	}
	limited := io.LimitReader(r.Body, maxBodyBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, nil, errors.New("unable to read request")
	}
	if len(raw) > maxBodyBytes {
		return nil, nil, errTooLarge
	}
	switch strings.ToLower(r.Header.Get("Content-Encoding")) {
	case "", "identity":
	case "gzip":
		reader, err := gzip.NewReader(strings.NewReader(string(raw)))
		if err != nil {
			return nil, nil, errors.New("invalid gzip request")
		}
		defer reader.Close()
		raw, err = io.ReadAll(io.LimitReader(reader, maxBodyBytes+1))
		if err != nil || len(raw) > maxBodyBytes {
			return nil, nil, errTooLarge
		}
	case "deflate":
		reader, err := zlib.NewReader(strings.NewReader(string(raw)))
		if err != nil {
			return nil, nil, errors.New("invalid deflate request")
		}
		defer reader.Close()
		raw, err = io.ReadAll(io.LimitReader(reader, maxBodyBytes+1))
		if err != nil || len(raw) > maxBodyBytes {
			return nil, nil, errTooLarge
		}
	default:
		return nil, nil, errors.New("unsupported request compression")
	}
	var payload map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, nil, errors.New("invalid JSON request")
	}
	return raw, payload, nil
}

func removeForeignReasoning(payload map[string]any) {
	input, ok := payload["input"].([]any)
	if !ok {
		return
	}
	filtered := make([]any, 0, len(input))
	for _, value := range input {
		item, ok := value.(map[string]any)
		if !ok {
			filtered = append(filtered, value)
			continue
		}
		_, hasContent := item["content"]
		if item["type"] == "reasoning" && hasContent {
			continue
		}
		filtered = append(filtered, value)
	}
	payload["input"] = filtered
}

func (router *Router) audit(model, upstream string, status int, payload map[string]any, diagnostics ...upstreamDiagnostics) {
	if router.Audit == nil {
		return
	}
	stream, _ := payload["stream"].(bool)
	var diagnostic upstreamDiagnostics
	if len(diagnostics) != 0 {
		diagnostic = diagnostics[0]
	}
	router.Audit(AuditRecord{
		upstreamDiagnostics: diagnostic,
		Time:                time.Now().UTC().Format(time.RFC3339Nano), Model: model,
		Upstream: upstream, Status: status, Stream: stream,
	})
}

func writeError(w http.ResponseWriter, status int, message string) {
	if status < 400 {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"type": "router_error", "message": message},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func newGzipWriter(w io.Writer) *gzip.Writer {
	return gzip.NewWriter(w)
}

func withStatus(errorMessage string, status int) error {
	return fmt.Errorf("%s: %w", errorMessage, errors.New(http.StatusText(status)))
}
