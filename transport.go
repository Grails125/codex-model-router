package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"syscall"
	"time"
)

// Only fixed classifications and numeric metadata may leave this type. Network
// errors commonly include URLs, proxy credentials or response text.
type upstreamDiagnostics struct {
	Stage            string `json:"stage,omitempty"`
	ErrorClass       string `json:"error_class,omitempty"`
	Errno            int    `json:"errno,omitempty"`
	ElapsedMS        int64  `json:"elapsed_ms"`
	Attempts         int    `json:"attempts"`
	ConnectionReused bool   `json:"connection_reused"`
	ProxyUsed        bool   `json:"proxy_used"`
	RetryStage       string `json:"retry_stage,omitempty"`
	RetryErrorClass  string `json:"retry_error_class,omitempty"`
}

type upstreamTraceKey struct{}

type upstreamTrace struct {
	mu           sync.Mutex
	stage        string
	setupStarted bool
	gotConn      bool
	reused       bool
	proxyStatus  int
}

func (state *upstreamTrace) hooks() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn:           func(string) { state.mu.Lock(); state.stage = "connect"; state.setupStarted = true; state.mu.Unlock() },
		DNSStart:          func(httptrace.DNSStartInfo) { state.setStage("dns") },
		ConnectStart:      func(string, string) { state.setStage("connect") },
		TLSHandshakeStart: func() { state.setStage("tls") },
		GotConn: func(info httptrace.GotConnInfo) {
			state.mu.Lock()
			state.gotConn = true
			state.reused = info.Reused
			state.stage = "send"
			state.mu.Unlock()
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				state.setStage("response_headers")
			}
		},
	}
}

func (state *upstreamTrace) setStage(stage string) {
	state.mu.Lock()
	state.stage = stage
	state.mu.Unlock()
}

func (state *upstreamTrace) snapshot() (string, bool, bool, bool, int) {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.stage, state.setupStarted, state.gotConn, state.reused, state.proxyStatus
}

func (router *Router) doUpstream(request *http.Request) (*http.Response, upstreamDiagnostics, error) {
	started := time.Now()
	diagnostic := upstreamDiagnostics{}
	if transport, ok := router.Client.Transport.(*http.Transport); ok && transport.Proxy != nil {
		proxy, err := transport.Proxy(request)
		if err != nil {
			diagnostic.Stage, diagnostic.ErrorClass = "proxy", "proxy_configuration"
			diagnostic.ElapsedMS = time.Since(started).Milliseconds()
			return nil, diagnostic, err
		}
		diagnostic.ProxyUsed = proxy != nil
	}
	if request.Body != nil {
		defer request.Body.Close()
	}
	for attempt := 1; attempt <= 2; attempt++ {
		state := &upstreamTrace{stage: "connect"}
		ctx := context.WithValue(request.Context(), upstreamTraceKey{}, state)
		cloned := request.Clone(httptrace.WithClientTrace(ctx, state.hooks()))
		if request.GetBody != nil {
			body, err := request.GetBody()
			if err != nil {
				diagnostic.Stage, diagnostic.ErrorClass = "request_body", "request_body"
				diagnostic.ElapsedMS = time.Since(started).Milliseconds()
				return nil, diagnostic, err
			}
			cloned.Body = body
		}
		// The router owns the retry boundary. Keep the caller's idempotency
		// metadata, but do not let Transport replay a POST after it was sent.
		cloned.GetBody = nil
		diagnostic.Attempts = attempt
		response, err := router.Client.Do(cloned)
		stage, setupStarted, gotConn, reused, proxyStatus := state.snapshot()
		diagnostic.ConnectionReused = reused
		diagnostic.ElapsedMS = time.Since(started).Milliseconds()
		if err == nil {
			diagnostic.Stage, diagnostic.ErrorClass, diagnostic.Errno = "response_headers", "", 0
			return response, diagnostic, nil
		}
		// No callbacks from an unknown RoundTripper means no proof that it did
		// not send the POST. Such errors must never be retried.
		diagnostic.Stage = stage
		var transient bool
		diagnostic.ErrorClass, diagnostic.Errno, transient = classifyTransportError(err, stage, proxyStatus)
		if request.Context().Err() != nil {
			diagnostic.ErrorClass = "cancelled"
			if errors.Is(request.Context().Err(), context.DeadlineExceeded) {
				diagnostic.ErrorClass = "deadline"
			}
			return nil, diagnostic, err
		}
		replayableBody := request.Body == nil || request.GetBody != nil
		if attempt != 1 || !setupStarted || gotConn || !transient || !replayableBody {
			return nil, diagnostic, err
		}
		diagnostic.RetryStage, diagnostic.RetryErrorClass = diagnostic.Stage, diagnostic.ErrorClass
		timer := time.NewTimer(150 * time.Millisecond)
		select {
		case <-request.Context().Done():
			timer.Stop()
			diagnostic.ErrorClass = "cancelled"
			diagnostic.ElapsedMS = time.Since(started).Milliseconds()
			return nil, diagnostic, request.Context().Err()
		case <-timer.C:
		}
	}
	panic("unreachable transport attempt")
}

func classifyTransportError(err error, stage string, proxyStatus int) (string, int, bool) {
	if errors.Is(err, context.Canceled) {
		return "cancelled", 0, false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline", 0, false
	}
	if proxyStatus != 0 && proxyStatus != http.StatusOK {
		return "proxy_connect", 0, proxyStatus == 502 || proxyStatus == 503 || proxyStatus == 504
	}
	var certificate *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalidCertificate x509.CertificateInvalidError
	if errors.As(err, &certificate) || errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalidCertificate) {
		return "tls_certificate", 0, false
	}
	var record tls.RecordHeaderError
	if errors.As(err, &record) {
		return "tls_protocol", 0, false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns", 0, !dns.IsNotFound && (dns.IsTimeout || dns.IsTemporary)
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ECONNREFUSED:
			return "connection_refused", int(errno), true
		case syscall.ECONNRESET:
			return "connection_reset", int(errno), true
		case syscall.EPIPE:
			return "broken_pipe", int(errno), true
		case syscall.ETIMEDOUT:
			return "timeout", int(errno), true
		}
		return "network_error", int(errno), false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected_eof", 0, true
	}
	if errors.Is(err, io.EOF) {
		return "eof", 0, true
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return "timeout", 0, true
	}
	if stage == "tls" {
		return "tls_handshake", 0, false
	}
	return "transport_error", 0, false
}

func safeUpstreamError(diagnostic upstreamDiagnostics) string {
	return "Upstream connection failed (stage=" + diagnostic.Stage + "; class=" + diagnostic.ErrorClass + ")"
}
