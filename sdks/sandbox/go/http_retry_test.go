// Copyright 2026 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package opensandbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestStreamClient_NoOverallTimeoutOnLongStream verifies that the streaming
// path is not killed by http.Client.Timeout. Historically, the SDK applied
// the same 30s (or user-configured) http.Client.Timeout to both the
// non-streaming lifecycle client and the streaming execd client, cutting off
// any SSE stream that ran longer than the timeout mid-flight. The fix routes
// SSE requests through a stream client that shares the same Transport but
// keeps Timeout=0; per-call duration is controlled by the caller's context.
func TestStreamClient_NoOverallTimeoutOnLongStream(t *testing.T) {
	// SSE server that emits three events over ~600ms, well beyond the tiny
	// http.Client.Timeout we install below.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("test server: response writer does not support flushing")
			return
		}
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: event-%d\n\n", i)
			flusher.Flush()
			time.Sleep(200 * time.Millisecond)
		}
	}))
	defer srv.Close()

	// Deliberately absurd 100ms timeout on the non-streaming client. If the
	// stream client inherited this, the SSE connection would be killed
	// almost immediately after the first event and the test would fail.
	client := NewClient(srv.URL, "", "OPEN-SANDBOX-API-KEY", WithTimeout(100*time.Millisecond))

	var received []string
	handler := func(event StreamEvent) error {
		received = append(received, event.Data)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.doStreamRequest(ctx, http.MethodGet, "/", nil, handler)
	require.NoError(t, err)
	require.Equal(t, 3, len(received))
	require.Equal(t, "event-0", received[0])
	require.Equal(t, "event-2", received[2])
}

// TestStreamClient_SharesTransportWithHTTPClient documents the invariant
// that the stream client and the regular http client must share the same
// Transport, so that connection pool state is not split into two disjoint
// pools (which would defeat keep-alive and waste connections).
func TestStreamClient_SharesTransportWithHTTPClient(t *testing.T) {
	client := NewClient("https://example.com", "", "OPEN-SANDBOX-API-KEY")
	require.NotNil(t, client.httpClient)
	require.NotNil(t, client.streamClient)
	if client.httpClient.Transport != client.streamClient.Transport {
		assert.Fail(t, "streamClient must share Transport with httpClient")
	}
	// Regardless of what Timeout is set on httpClient, the stream client
	// must always have Timeout=0 so it never kills long-lived SSE streams.
	if client.streamClient.Timeout != 0 {
		assert.Fail(t, fmt.Sprintf("streamClient.Timeout = %v, want 0", client.streamClient.Timeout))
	}
}

// TestStreamClient_TimeoutOverrideStillYieldsZeroOnStreamClient verifies the
// stream client is unaffected even when the user configures a non-zero
// timeout via WithTimeout.
func TestStreamClient_TimeoutOverrideStillYieldsZeroOnStreamClient(t *testing.T) {
	client := NewClient("https://example.com", "", "OPEN-SANDBOX-API-KEY", WithTimeout(15*time.Second))
	require.Equal(t, 15*time.Second, client.httpClient.Timeout)
	if client.streamClient.Timeout != 0 {
		assert.Fail(t, fmt.Sprintf("streamClient.Timeout = %v, want 0", client.streamClient.Timeout))
	}
}

// TestStreamRequest_ResponseHeaderTimeoutBoundsPreStreamHang verifies the
// PR-review fix for "keep a setup timeout for SSE handshakes": even though
// the stream client has Timeout=0 to allow arbitrarily long response bodies,
// the underlying Transport's ResponseHeaderTimeout must bound the wait for
// the initial SSE response headers. Without this, a streaming endpoint or
// load balancer that accepts the connection but never sends headers would
// make RunCommand / ExecuteCode / WatchMetrics hang indefinitely.
func TestStreamRequest_ResponseHeaderTimeoutBoundsPreStreamHang(t *testing.T) {
	blackHole := newHijackBlackHoleHandler()
	defer blackHole.release()

	srv := httptest.NewServer(http.HandlerFunc(blackHole.ServeHTTP))
	defer srv.Close()

	// Custom transport with a tight ResponseHeaderTimeout so the test
	// completes fast. All other defaults preserved.
	tc := DefaultTransportConfig()
	tc.ResponseHeaderTimeout = 300 * time.Millisecond
	client := NewClient(srv.URL, "", "OPEN-SANDBOX-API-KEY",
		WithHTTPClient(&http.Client{Transport: tc.NewTransport()}),
	)

	handler := func(event StreamEvent) error { return nil }
	// Use a caller context deadline far larger than the transport timeout.
	// If the transport-level guard is missing, this test hangs until the
	// caller ctx fires; if the guard works, we get a header-phase timeout
	// error well before the ctx deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	err := client.doStreamRequest(ctx, http.MethodGet, "/", nil, handler)
	elapsed := time.Since(start)

	require.Error(t, err)
	if !strings.Contains(err.Error(), "timeout awaiting response headers") {
		assert.Fail(t, fmt.Sprintf("expected response-header timeout, got: %v", err))
	}
	if elapsed > 3*time.Second {
		assert.Fail(t, fmt.Sprintf("expected header-phase guard to fire quickly (<3s), took %v", elapsed))
	}
}

// hijackBlackHoleHandler simulates an intermediate load balancer that has
// silently dropped a keep-alive connection: it hijacks the TCP connection,
// discards the request, and never writes any response. The connection is
// held open until the test ends, mimicking a black-holed reused connection.
// A callback is invoked once so the test can observe how many times this
// path fires.
type hijackBlackHoleHandler struct {
	hits   atomic.Int32
	closed chan struct{}
}

func newHijackBlackHoleHandler() *hijackBlackHoleHandler {
	return &hijackBlackHoleHandler{closed: make(chan struct{})}
}

func (h *hijackBlackHoleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.hits.Add(1)
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	// Hold the connection open with no reply until the test tears down.
	go func() {
		<-h.closed
		_ = conn.Close()
	}()
}

func (h *hijackBlackHoleHandler) release() { close(h.closed) }

// TestStaleConnectionRetry_GETRetriesOnReusedPooledConn verifies that a GET
// that hangs waiting for response headers on a REUSED pooled connection
// (because an LB silently dropped it) is transparently retried once on a
// fresh TCP connection. This is the core mitigation for the intermittent
// "Client.Timeout exceeded while awaiting headers" failures observed
// against enterprise load balancers.
//
// The test primes the connection pool with a successful request first, so
// the second (black-holed) request actually reuses an idle pooled
// connection. This matches production reality and exercises the
// httptrace-driven "reused idle" gate.
func TestStaleConnectionRetry_GETRetriesOnReusedPooledConn(t *testing.T) {
	var reqCount atomic.Int32
	blackHole := newHijackBlackHoleHandler()
	defer blackHole.release()

	mux := http.NewServeMux()
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		n := reqCount.Add(1)
		// First call: prime the pool with a successful response.
		// Second call: black-hole (simulates LB-dropped keep-alive conn).
		// Third call (retry): reply normally.
		if n == 2 {
			blackHole.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewClient(srv.URL, "", "OPEN-SANDBOX-API-KEY", WithTimeout(300*time.Millisecond))

	// Prime the pool: this request completes and returns the conn to the
	// idle pool so the next request can reuse it.
	var priming map[string]bool
	require.NoError(t, client.doRequest(context.Background(), http.MethodGet, "/probe", nil, &priming))

	// Now the real test: request #2 will reuse the idle pooled conn and
	// hit the black-hole; the SDK must retry on a fresh connection and
	// succeed on request #3.
	var result map[string]bool
	start := time.Now()
	err := client.doRequest(context.Background(), http.MethodGet, "/probe", nil, &result)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Equal(t, true, result["ok"])
	if reqCount.Load() != 3 {
		assert.Fail(t, fmt.Sprintf("expected 3 total server hits (prime, black-hole, retry), got %d", reqCount.Load()))
	}
	if elapsed > 5*time.Second {
		assert.Fail(t, fmt.Sprintf("retry took too long: %v", elapsed))
	}
}

// TestStaleConnectionRetry_FreshConnDoesNotRetry verifies the critical
// guard from PR review: a slow server that never returns response headers
// on a FRESHLY DIALED connection must NOT be retried. Retrying would
// double the caller's timeout budget for no benefit and would issue the
// same GET twice against a server that is simply overloaded.
func TestStaleConnectionRetry_FreshConnDoesNotRetry(t *testing.T) {
	var reqCount atomic.Int32
	blackHole := newHijackBlackHoleHandler()
	defer blackHole.release()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		blackHole.ServeHTTP(w, r)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", "OPEN-SANDBOX-API-KEY", WithTimeout(300*time.Millisecond))

	// A brand-new client with an empty pool: the request must dial fresh.
	// Server never returns headers -> Client.Timeout fires. Because this
	// is NOT a reused-idle-conn scenario, the SDK must respect the
	// caller's timeout budget and not retry.
	start := time.Now()
	err := client.doRequest(context.Background(), http.MethodGet, "/anything", nil, nil)
	elapsed := time.Since(start)

	require.Error(t, err)
	if !strings.Contains(err.Error(), "Client.Timeout exceeded while awaiting headers") {
		assert.Fail(t, fmt.Sprintf("expected client-timeout error, got: %v", err))
	}
	if reqCount.Load() != 1 {
		assert.Fail(t, fmt.Sprintf("fresh-dial slow-server must not be retried; got %d hits", reqCount.Load()))
	}
	// The elapsed time should be roughly one timeout window, not two.
	if elapsed > 2*time.Second {
		assert.Fail(t, fmt.Sprintf("elapsed=%v suggests the SDK retried and burned two timeout budgets", elapsed))
	}
}

// TestStaleConnectionRetry_POSTNotRetried verifies that non-idempotent
// methods are NEVER retried by the stale-connection fallback, even when the
// error signature matches, because retrying a POST that may have been
// applied server-side is unsafe.
func TestStaleConnectionRetry_POSTNotRetried(t *testing.T) {
	var reqCount atomic.Int32
	blackHole := newHijackBlackHoleHandler()
	defer blackHole.release()

	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/anything", func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		blackHole.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewClient(srv.URL, "", "OPEN-SANDBOX-API-KEY", WithTimeout(300*time.Millisecond))

	// Prime the pool with a successful GET so the follow-up POST reuses
	// an idle pooled connection. This confirms POST is refused even when
	// the reused-idle guard would otherwise allow a retry.
	require.NoError(t, client.doRequest(context.Background(), http.MethodGet, "/ok", nil, nil))
	priming := reqCount.Load()

	err := client.doRequest(context.Background(), http.MethodPost, "/anything", map[string]string{"a": "b"}, nil)
	require.Error(t, err)
	if !strings.Contains(err.Error(), "Client.Timeout exceeded while awaiting headers") {
		assert.Fail(t, fmt.Sprintf("expected client-timeout error, got: %v", err))
	}
	if got := reqCount.Load() - priming; got != 1 {
		assert.Fail(t, fmt.Sprintf("POST must not be retried on stale-connection failure; got %d POST hits", got))
	}
}

// TestShouldRetryStaleConnection_TableDriven documents the precise trigger
// conditions of the stale-connection retry decision, so future changes to
// error mapping do not accidentally broaden or narrow the policy.
func TestShouldRetryStaleConnection_TableDriven(t *testing.T) {
	awaitHeadersErr := errors.New(`Get "http://x": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`)
	respHeaderTimeoutErr := errors.New(`Get "http://x": net/http: timeout awaiting response headers`)
	plainCtxDeadline := context.DeadlineExceeded
	plainCtxCanceled := context.Canceled
	apiErr := &APIError{StatusCode: 502}
	randomErr := errors.New("boom")

	cases := []struct {
		name       string
		method     string
		err        error
		reusedIdle bool
		want       bool
	}{
		// Positive: idempotent method + observed idle-conn reuse + header-phase timeout.
		{"GET awaiting-headers on reused idle conn retries", http.MethodGet, awaitHeadersErr, true, true},
		{"HEAD awaiting-headers on reused idle conn retries", http.MethodHead, awaitHeadersErr, true, true},
		{"OPTIONS awaiting-headers on reused idle conn retries", http.MethodOptions, awaitHeadersErr, true, true},
		{"GET response-header timeout on reused idle conn retries", http.MethodGet, respHeaderTimeoutErr, true, true},

		// The critical guard: fresh-conn slow server MUST NOT be retried.
		// This protects the caller's timeout budget for the "genuinely slow
		// server" scenario raised in PR review.
		{"GET awaiting-headers on FRESH conn does not retry", http.MethodGet, awaitHeadersErr, false, false},
		{"HEAD awaiting-headers on FRESH conn does not retry", http.MethodHead, awaitHeadersErr, false, false},
		{"GET response-header timeout on FRESH conn does not retry", http.MethodGet, respHeaderTimeoutErr, false, false},

		// Non-idempotent methods are never retried even on reused idle conn.
		{"POST awaiting-headers on reused idle conn does not retry", http.MethodPost, awaitHeadersErr, true, false},
		{"PUT awaiting-headers on reused idle conn does not retry", http.MethodPut, awaitHeadersErr, true, false},
		{"PATCH awaiting-headers on reused idle conn does not retry", http.MethodPatch, awaitHeadersErr, true, false},
		{"DELETE awaiting-headers on reused idle conn does not retry", http.MethodDelete, awaitHeadersErr, true, false},

		// Caller-driven and non-timeout errors are never retried.
		{"caller ctx deadline does not retry", http.MethodGet, plainCtxDeadline, true, false},
		{"caller ctx canceled does not retry", http.MethodGet, plainCtxCanceled, true, false},
		{"APIError does not retry (headers already received)", http.MethodGet, apiErr, true, false},
		{"generic non-timeout error does not retry", http.MethodGet, randomErr, true, false},
		{"nil error does not retry", http.MethodGet, nil, true, false},
	}
	for _, tc := range cases {
		if got := shouldRetryStaleConnection(tc.method, tc.err, tc.reusedIdle); got != tc.want {
			assert.Fail(t, fmt.Sprintf("%s: shouldRetryStaleConnection = %v, want %v", tc.name, got, tc.want))
		}
	}
}
