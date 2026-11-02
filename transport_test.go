package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func transportProbeRouter(t *testing.T, upstream string, audit func(AuditRecord)) *Router {
	t.Helper()
	router, err := NewRouter(RuntimeOptions{
		RouterKey: "local-secret-at-least-24-bytes", OpenAIURL: upstream, DeepSeekURL: upstream,
	}, func() (string, error) { return "deepseek-secret", nil }, audit)
	if err != nil {
		t.Fatal(err)
	}
	transport := router.Client.Transport.(*http.Transport)
	transport.Proxy = nil
	t.Cleanup(transport.CloseIdleConnections)
	return router
}

func transportProbeRequest(t *testing.T, endpoint string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{"model":"gpt-test"}`))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTransportRetriesOnlyBeforePostCanBeSent(t *testing.T) {
	var received atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "complete")
	}))
	defer upstream.Close()
	for _, recover := range []bool{true, false} {
		t.Run(fmt.Sprintf("recovered=%t", recover), func(t *testing.T) {
			router := transportProbeRouter(t, upstream.URL, nil)
			transport := router.Client.Transport.(*http.Transport)
			var dials atomic.Int32
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if dials.Add(1) == 1 || !recover {
					return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
				}
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
			before := received.Load()
			response, diagnostic, err := router.doUpstream(transportProbeRequest(t, upstream.URL))
			if response != nil {
				_, _ = io.Copy(io.Discard, response.Body)
				response.Body.Close()
			}
			if (err == nil) != recover || dials.Load() != 2 || diagnostic.Attempts != 2 {
				t.Fatalf("err=%v dials=%d diagnostic=%+v", err, dials.Load(), diagnostic)
			}
			wantPosts := int32(0)
			if recover {
				wantPosts = 1
			}
			if received.Load()-before != wantPosts || diagnostic.RetryErrorClass != "connection_refused" {
				t.Fatalf("posts=%d diagnostic=%+v", received.Load()-before, diagnostic)
			}
		})
	}
}

func TestTransportDoesNotReplayPostAfterConnectionObtained(t *testing.T) {
	for _, key := range []string{"", "same-generation-idempotency-key"} {
		t.Run("idempotency_key="+key, func(t *testing.T) {
			var received atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if received.Add(1) != 2 {
					_, _ = io.WriteString(w, "complete")
					return
				}
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				connection.Close()
			}))
			defer upstream.Close()
			router := transportProbeRouter(t, upstream.URL, nil)
			warm, _, err := router.doUpstream(transportProbeRequest(t, upstream.URL))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, warm.Body)
			warm.Body.Close()
			request := transportProbeRequest(t, upstream.URL)
			if key != "" {
				request.Header.Set("Idempotency-Key", key)
			}
			response, diagnostic, err := router.doUpstream(request)
			if response != nil {
				response.Body.Close()
			}
			if err == nil || received.Load() != 2 || diagnostic.Attempts != 1 {
				t.Fatalf("POST was not safely bounded: err=%v posts=%d diagnostic=%+v", err, received.Load(), diagnostic)
			}
		})
	}
}

func TestTransportCancellationDoesNotRetry(t *testing.T) {
	router := transportProbeRouter(t, "http://127.0.0.1:1", nil)
	var dials atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	router.Client.Transport.(*http.Transport).DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		dials.Add(1)
		close(entered)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, context.Canceled
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := transportProbeRequest(t, "http://127.0.0.1:1").WithContext(ctx)
	go func() { <-entered; cancel() }()
	_, diagnostic, err := router.doUpstream(request)
	if !errors.Is(err, context.Canceled) || dials.Load() != 1 || diagnostic.Attempts != 1 || diagnostic.ErrorClass != "cancelled" {
		t.Fatalf("cancellation was retried: err=%v dials=%d diagnostic=%+v", err, dials.Load(), diagnostic)
	}
}

func TestTransportHTTPErrorIsForwardedWithoutRetry(t *testing.T) {
	var received atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Retry-After", "2")
		http.Error(w, "upstream overloaded", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	var records []AuditRecord
	router := transportProbeRouter(t, upstream.URL, func(record AuditRecord) { records = append(records, record) })
	response := postRouter(t, router, `{"model":"gpt-test","stream":true}`)
	if response.Code != 503 || response.Header().Get("Retry-After") != "2" || received.Load() != 1 || len(records) != 1 || records[0].Attempts != 1 || records[0].Status != 503 {
		t.Fatalf("HTTP error was retried or hidden: status=%d posts=%d records=%+v", response.Code, received.Load(), records)
	}
}

func TestTransportProxyConnectFailureHasBoundedSafeRetry(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusProxyAuthRequired} {
		t.Run(fmt.Sprintf("proxy_status=%d", status), func(t *testing.T) {
			var connects atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connects.Add(1)
				if r.Method != http.MethodConnect {
					t.Errorf("proxy received an application request: %s", r.Method)
				}
				http.Error(w, "private-proxy-response-text", status)
			}))
			defer proxy.Close()
			proxyURL, err := url.Parse(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			router := transportProbeRouter(t, "https://example.invalid", nil)
			router.Client.Transport.(*http.Transport).Proxy = http.ProxyURL(proxyURL)
			_, diagnostic, err := router.doUpstream(transportProbeRequest(t, "https://example.invalid/responses"))
			wantAttempts := 1
			if status == http.StatusBadGateway {
				wantAttempts = 2
			}
			if err == nil || int(connects.Load()) != wantAttempts || diagnostic.Attempts != wantAttempts || diagnostic.ErrorClass != "proxy_connect" || diagnostic.Stage != "proxy_connect" || !diagnostic.ProxyUsed {
				t.Fatalf("proxy failure handling: err=%v connects=%d diagnostic=%+v", err, connects.Load(), diagnostic)
			}
			if strings.Contains(safeUpstreamError(diagnostic), "private-proxy-response-text") {
				t.Fatal("proxy response text leaked into diagnostic")
			}
		})
	}
}

func TestTransportPoolReusesSuccessfulConnection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "complete")
	}))
	defer upstream.Close()
	router := transportProbeRouter(t, upstream.URL, nil)
	for i := 0; i < 2; i++ {
		response, diagnostic, err := router.doUpstream(transportProbeRequest(t, upstream.URL))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if diagnostic.ConnectionReused != (i == 1) || diagnostic.Attempts != 1 {
			t.Fatalf("request %d did not preserve pooling: %+v", i, diagnostic)
		}
	}
}

type transportProbeFunc func(*http.Request) (*http.Response, error)

func (f transportProbeFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportDiagnosticDoesNotExposeRawErrorsOrCredentials(t *testing.T) {
	var calls atomic.Int32
	var record AuditRecord
	router := transportProbeRouter(t, "http://127.0.0.1:1", func(value AuditRecord) { record = value })
	router.Client.Transport = transportProbeFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("private-error-marker https://user:proxy-password@example.invalid/?token=secret-token")
	})
	response := postRouter(t, router, `{"model":"gpt-test","stream":true,"input":"private-prompt-marker"}`)
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	combined := string(raw) + response.Body.String()
	for _, secret := range []string{"private-error-marker", "proxy-password", "secret-token", "private-prompt-marker", "openai-secret", "account-secret", "local-secret-at-least-24-bytes"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("diagnostic leaked %q", secret)
		}
	}
	if response.Code != 502 || calls.Load() != 1 || record.Status != 502 || record.ErrorClass != "transport_error" || record.Attempts != 1 {
		t.Fatalf("unsafe unknown transport handling: response=%d calls=%d record=%+v", response.Code, calls.Load(), record)
	}
}

func TestTransportTruncatedStreamDoesNotBecomeCleanEOFOrReplay(t *testing.T) {
	var received atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
	}))
	defer upstream.Close()
	records := make(chan AuditRecord, 1)
	router := transportProbeRouter(t, upstream.URL, func(record AuditRecord) { records <- record })
	local := httptest.NewServer(router)
	defer local.Close()
	request := transportProbeRequest(t, local.URL+"/backend-api/codex/responses")
	request.Body = io.NopCloser(strings.NewReader(`{"model":"gpt-test","stream":true}`))
	request.ContentLength = int64(len(`{"model":"gpt-test","stream":true}`))
	request.Header.Set("X-Codex-Router-Key", "local-secret-at-least-24-bytes")
	request.Header.Set("Authorization", "Bearer openai-secret")
	response, err := local.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	select {
	case record := <-records:
		if record.Status != 502 || record.Stage != "response_body" || record.Attempts != 1 || received.Load() != 1 {
			t.Fatalf("stream truncation was hidden or retried: posts=%d record=%+v", received.Load(), record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("truncated stream was not audited")
	}
	if readErr == nil {
		t.Fatal("upstream truncation became clean downstream EOF")
	}
}
