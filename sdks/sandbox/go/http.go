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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"
)

// defaultTimeout is 0 (no global timeout) because a non-zero value kills
// long-lived SSE streaming connections. Use per-request context deadlines
// instead to control individual call timeouts.
const defaultTimeout = 0

// Client is the base HTTP client shared by LifecycleClient and EgressClient.
type Client struct {
	baseURL    string
	apiKey     string
	authHeader string
	httpClient *http.Client
	// streamClient shares the same Transport as httpClient but always has
	// Timeout=0. It is used by doStreamRequest so that a caller-configured
	// per-request timeout on httpClient does not kill long-lived SSE
	// connections. See the comment on defaultTimeout above.
	streamClient *http.Client
	timeout      *time.Duration // stored separately, applied after all options
	headers      map[string]string
	retry        *RetryConfig
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets a custom http.Client.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) {
		cl.httpClient = c
	}
}

// WithTimeout sets the HTTP client timeout. The timeout is applied after all
// options, so it is safe to combine with WithHTTPClient in any order.
func WithTimeout(d time.Duration) Option {
	return func(cl *Client) {
		cl.timeout = &d
	}
}

// WithHeaders adds custom HTTP headers to all requests. These are applied
// before the auth and content-type headers, so they cannot override those.
func WithHeaders(headers map[string]string) Option {
	return func(cl *Client) {
		if cl.headers == nil {
			cl.headers = make(map[string]string, len(headers))
		}
		for k, v := range headers {
			cl.headers[k] = v
		}
	}
}

// WithAuthHeader overrides the default auth header name. Use this when the
// server expects a different header (e.g. "X-API-Key" instead of
// "OPEN-SANDBOX-API-KEY").
func WithAuthHeader(header string) Option {
	return func(cl *Client) {
		cl.authHeader = header
	}
}

// NewClient creates a new base Client. The authHeader parameter specifies
// which HTTP header carries the API key (e.g. "OPEN-SANDBOX-API-KEY" for
// lifecycle, "OPENSANDBOX-EGRESS-AUTH" for egress).
func NewClient(baseURL, apiKey, authHeader string, opts ...Option) *Client {
	c := &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		authHeader: authHeader,
		httpClient: &http.Client{
			Timeout:   defaultTimeout,
			Transport: DefaultTransport(),
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{
			Timeout:   defaultTimeout,
			Transport: DefaultTransport(),
		}
	} else if c.httpClient.Transport == nil {
		// Clone the caller's client to avoid mutating shared instances
		// (e.g. http.DefaultClient) which would leak the SDK's transport
		// settings into unrelated traffic in the same process.
		cloned := *c.httpClient
		cloned.Transport = DefaultTransport()
		c.httpClient = &cloned
	}
	// Apply deferred timeout after all options so it works regardless of
	// WithHTTPClient ordering and guards against a nil httpClient.
	if c.timeout != nil {
		c.httpClient.Timeout = *c.timeout
	}
	// Build a stream client that shares the same Transport (and therefore
	// the same connection pool) as httpClient but never sets an overall
	// http.Client.Timeout. A non-zero Timeout on an SSE request kills the
	// connection at the deadline mid-stream, regardless of activity;
	// long-running commands and metric watches must control their total
	// duration via the caller's context instead.
	streamClient := *c.httpClient
	streamClient.Timeout = 0
	c.streamClient = &streamClient
	return c
}

// doRequest executes an HTTP request with JSON encoding and auth headers,
// retrying on transient errors if a RetryConfig is set.
// If body is nil, no request body is sent. If result is non-nil, the
// response body is decoded into it.
func (c *Client) doRequest(ctx context.Context, method, path string, body any, result any) error {
	return c.withRetry(ctx, func() error {
		return c.doRequestOnce(ctx, method, path, body, result)
	})
}

// doRequestOnce is the single-attempt implementation of doRequest from the
// perspective of the higher-level retry policy (withRetry). Internally, it
// may transparently retry once against a fresh TCP connection when a stale
// pooled connection appears to have been silently dropped by an intermediate
// load balancer; see shouldRetryStaleConnection for the trigger conditions.
func (c *Client) doRequestOnce(ctx context.Context, method, path string, body any, result any) error {
	var encodedBody []byte
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("opensandbox: marshal request: %w", err)
		}
		encodedBody = buf
	}

	var reusedIdle atomic.Bool
	err := c.sendJSONRequest(ctx, method, path, encodedBody, result, &reusedIdle)
	if err == nil {
		return nil
	}
	if !shouldRetryStaleConnection(method, err, reusedIdle.Load()) {
		return err
	}
	// The failure signature matches a reused-but-silently-dropped keep-alive
	// connection: we observed the http client picking up an idle pooled
	// connection, wrote the request locally, and then no response headers
	// arrived before the header-phase timeout fired. Evict idle connections
	// in the pool and retry exactly once on a freshly dialed connection.
	// Only safe for idempotent methods; see shouldRetryStaleConnection.
	//
	// The retry uses the same caller ctx and http.Client.Timeout as the
	// first attempt. That matches the "one bounded retry within the
	// caller's budget" semantics: the total wait a caller can observe is
	// bounded by roughly two http.Client.Timeout windows, but the retry
	// only fires when we can prove (via httptrace) that the first attempt
	// reused an idle pooled connection — which never happens on a genuinely
	// slow-but-alive server that just accepted a fresh connection.
	if tr, ok := c.httpClient.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	return c.sendJSONRequest(ctx, method, path, encodedBody, result, nil)
}

// sendJSONRequest builds and executes a single HTTP request. encodedBody is
// the pre-marshaled JSON payload (or nil for GET/DELETE-style requests). The
// caller is responsible for retry policy; this function performs exactly one
// round-trip.
//
// If reusedIdle is non-nil, it is set to true when the underlying http
// transport handed the request an already-idle pooled connection. This is
// the signal used to distinguish "server is genuinely slow" (fresh dial,
// reusedIdle=false) from "we reused a black-holed pooled connection"
// (reusedIdle=true); see doRequestOnce for how it is used.
func (c *Client) sendJSONRequest(ctx context.Context, method, path string, encodedBody []byte, result any, reusedIdle *atomic.Bool) error {
	var bodyReader io.Reader
	if encodedBody != nil {
		bodyReader = bytes.NewReader(encodedBody)
	}

	if reusedIdle != nil {
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				if info.Reused && info.WasIdle {
					reusedIdle.Store(true)
				}
			},
		})
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("opensandbox: create request: %w", err)
	}

	req.Header.Set("User-Agent", "OpenSandbox-Go-SDK/"+Version)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if c.apiKey != "" {
		req.Header.Set(c.authHeader, c.apiKey)
	}
	if encodedBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("opensandbox: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return handleError(resp)
	}

	// No content (e.g. 204)
	if resp.StatusCode == http.StatusNoContent || result == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("opensandbox: decode response: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// shouldRetryStaleConnection reports whether err looks like a stale pooled
// connection failure that should be retried on a fresh connection. This
// targets the common case where an idle keep-alive connection has been
// silently dropped by an intermediate load balancer: the write succeeds
// locally but the response headers never arrive.
//
// The check is deliberately conservative and requires ALL of:
//
//   - reusedIdlePooledConn is true. The httptrace GotConn hook observed the
//     transport handing this request an already-idle pooled connection. If
//     the transport instead dialed a fresh connection and that fresh
//     connection is slow, this is not a stale-reuse case — the server or
//     network is genuinely slow, and retrying would just double the caller's
//     timeout budget for no benefit. This gates out the "slow server"
//     scenario raised in PR review.
//   - Method is idempotent (GET/HEAD/OPTIONS). Retrying a POST that may
//     have already been applied server-side is unsafe.
//   - The error is not an APIError (that would mean we received response
//     headers; response-body-side failures are out of scope).
//   - The error signature matches the header-phase timeout: either the Go
//     net/http "Client.Timeout exceeded while awaiting headers" suffix, or
//     an http.Transport.ResponseHeaderTimeout firing, or a net.Error whose
//     Timeout() is true. All indicate we never got past the response-header
//     phase.
//   - The error is not a plain caller-driven context.Canceled /
//     context.DeadlineExceeded. Checked after the more specific timeout
//     signatures, because http.Client.Timeout wraps DeadlineExceeded and
//     would otherwise be misclassified as caller-driven.
func shouldRetryStaleConnection(method string, err error, reusedIdlePooledConn bool) bool {
	if err == nil {
		return false
	}
	// Without an observed idle-pool reuse, this cannot be a stale-connection
	// case: a freshly dialed connection that times out simply means the
	// remote is slow or unreachable, and retrying wastes the caller's
	// timeout budget. See the doc comment above.
	if !reusedIdlePooledConn {
		return false
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		return false
	}
	// APIError means we received response headers; not a stale-connection case.
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return false
	}
	// The Go net/http package appends this exact suffix when
	// http.Client.Timeout fires while waiting for response headers.
	if strings.Contains(err.Error(), "Client.Timeout exceeded while awaiting headers") {
		return true
	}
	// http.Transport.ResponseHeaderTimeout fires this specific error text
	// when the transport gives up waiting for response headers on a
	// specific connection. This is the SDK's primary header-phase guard
	// for streaming requests but also protects non-streaming requests.
	if strings.Contains(err.Error(), "timeout awaiting response headers") {
		return true
	}
	// Caller-driven cancellation/deadline: don't paper over it with a retry.
	// Checked AFTER the specific timeout signatures above, since
	// http.Client.Timeout wraps context.DeadlineExceeded.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Fall back to a generic timeout classification for other transports.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// doStreamRequest builds an HTTP request, executes it, and streams SSE events
// through handler. Connection setup is retried on transient errors; once
// streaming begins, errors are not retried (partial data may have been
// delivered to the handler).
func (c *Client) doStreamRequest(ctx context.Context, method, path string, body any, handler EventHandler) error {
	var resp *http.Response

	connectErr := c.withRetry(ctx, func() error {
		var bodyReader io.Reader
		if body != nil {
			buf, err := json.Marshal(body)
			if err != nil {
				return fmt.Errorf("opensandbox: marshal request: %w", err)
			}
			bodyReader = bytes.NewReader(buf)
		}

		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
		if err != nil {
			return fmt.Errorf("opensandbox: create request: %w", err)
		}

		req.Header.Set("User-Agent", "OpenSandbox-Go-SDK/"+Version)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		if c.apiKey != "" {
			req.Header.Set(c.authHeader, c.apiKey)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "text/event-stream")

		r, err := c.streamClient.Do(req)
		if err != nil {
			return fmt.Errorf("opensandbox: do request: %w", err)
		}

		if r.StatusCode >= 400 {
			defer r.Body.Close()
			return handleError(r)
		}

		resp = r
		return nil
	})
	if connectErr != nil {
		return connectErr
	}

	return streamSSE(ctx, resp, handler)
}

// handleError reads the response body and returns an *APIError.
// It captures the Retry-After header for use by the retry loop.
func handleError(resp *http.Response) error {
	apiErr := &APIError{
		StatusCode: resp.StatusCode,
		RequestID:  resp.Header.Get("X-Request-Id"),
		RetryAfter: parseRetryAfter(resp),
	}
	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		apiErr.Response = ErrorResponse{
			Code:    http.StatusText(resp.StatusCode),
			Message: fmt.Sprintf("failed to read error response body: %v", readErr),
		}
		return apiErr
	}

	// Try to decode as JSON ErrorResponse; fall back to raw body.
	if err := json.Unmarshal(data, &apiErr.Response); err != nil || apiErr.Response.Code == "" {
		apiErr.Response = ErrorResponse{
			Code:    http.StatusText(resp.StatusCode),
			Message: string(data),
		}
	}
	return apiErr
}
