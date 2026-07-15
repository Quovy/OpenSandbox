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

// TestStaleConnectionRetry_GETRetriesOnFreshConnection verifies that a GET
// that hangs waiting for response headers (because a pooled connection was
// silently dropped by an LB) is transparently retried once on a fresh TCP
// connection. This is the core mitigation for the intermittent
// "Client.Timeout exceeded while awaiting headers" failures observed
// against enterprise load balancers.
func TestStaleConnectionRetry_GETRetriesOnFreshConnection(t *testing.T) {
	// Mux: first request black-holes (hijack, never respond); subsequent
	// requests reply 200 with a JSON body. We use a request counter to
	// switch behavior.
	var reqCount atomic.Int32
	blackHole := newHijackBlackHoleHandler()
	defer blackHole.release()

	mux := http.NewServeMux()
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		n := reqCount.Add(1)
		if n == 1 {
			blackHole.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Tight http.Client.Timeout so the black-holed first request surfaces
	// the stall quickly.
	client := NewClient(srv.URL, "", "OPEN-SANDBOX-API-KEY", WithTimeout(300*time.Millisecond))

	var result map[string]bool
	start := time.Now()
	err := client.doRequest(context.Background(), http.MethodGet, "/probe", nil, &result)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Equal(t, true, result["ok"])
	if reqCount.Load() < 2 {
		assert.Fail(t, fmt.Sprintf("expected at least 2 server hits (first black-holed, retry succeeds), got %d", reqCount.Load()))
	}
	// Sanity: total time should be roughly one timeout window plus the
	// retry. Give generous headroom for CI slowness.
	if elapsed > 5*time.Second {
		assert.Fail(t, fmt.Sprintf("retry took too long: %v", elapsed))
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		blackHole.ServeHTTP(w, r)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "", "OPEN-SANDBOX-API-KEY", WithTimeout(300*time.Millisecond))

	err := client.doRequest(context.Background(), http.MethodPost, "/anything", map[string]string{"a": "b"}, nil)
	require.Error(t, err)
	if !strings.Contains(err.Error(), "Client.Timeout exceeded while awaiting headers") {
		assert.Fail(t, fmt.Sprintf("expected client-timeout error, got: %v", err))
	}
	if reqCount.Load() != 1 {
		assert.Fail(t, fmt.Sprintf("POST must not be retried on stale-connection failure; got %d hits", reqCount.Load()))
	}
}

// TestShouldRetryStaleConnection_TableDriven documents the precise trigger
// conditions of the stale-connection retry decision, so future changes to
// error mapping do not accidentally broaden or narrow the policy.
func TestShouldRetryStaleConnection_TableDriven(t *testing.T) {
	awaitHeadersErr := errors.New(`Get "http://x": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`)
	plainCtxDeadline := context.DeadlineExceeded
	plainCtxCanceled := context.Canceled
	apiErr := &APIError{StatusCode: 502}
	randomErr := errors.New("boom")

	cases := []struct {
		name   string
		method string
		err    error
		want   bool
	}{
		{"GET awaiting-headers timeout retries", http.MethodGet, awaitHeadersErr, true},
		{"HEAD awaiting-headers timeout retries", http.MethodHead, awaitHeadersErr, true},
		{"OPTIONS awaiting-headers timeout retries", http.MethodOptions, awaitHeadersErr, true},
		{"POST awaiting-headers timeout does not retry", http.MethodPost, awaitHeadersErr, false},
		{"PUT awaiting-headers timeout does not retry", http.MethodPut, awaitHeadersErr, false},
		{"PATCH awaiting-headers timeout does not retry", http.MethodPatch, awaitHeadersErr, false},
		{"DELETE awaiting-headers timeout does not retry", http.MethodDelete, awaitHeadersErr, false},
		{"caller ctx deadline does not retry", http.MethodGet, plainCtxDeadline, false},
		{"caller ctx canceled does not retry", http.MethodGet, plainCtxCanceled, false},
		{"APIError does not retry (headers already received)", http.MethodGet, apiErr, false},
		{"generic non-timeout error does not retry", http.MethodGet, randomErr, false},
		{"nil error does not retry", http.MethodGet, nil, false},
	}
	for _, tc := range cases {
		if got := shouldRetryStaleConnection(tc.method, tc.err); got != tc.want {
			assert.Fail(t, fmt.Sprintf("%s: shouldRetryStaleConnection = %v, want %v", tc.name, got, tc.want))
		}
	}
}
