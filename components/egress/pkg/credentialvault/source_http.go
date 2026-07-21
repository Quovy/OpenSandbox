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

package credentialvault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const httpSourceDefaultTimeout = 10 * time.Second

func defaultTransportDialer() *net.Dialer {
	return &net.Dialer{Timeout: 5 * time.Second}
}

type httpSource struct {
	mu sync.RWMutex
	sf singleflight.Group

	initialURL     string
	initialMethod  string
	initialHeaders map[string]string

	nextURL        string
	nextHeaders    map[string]string
	headersRotated bool

	cachedValue string
	expiresAt   time.Time
	fetched     bool

	client *http.Client
}

type httpSourceResponse struct {
	Value   string            `json:"value"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	TTL     *int              `json:"ttl,omitempty"`
}

func (s *httpSource) Type() string { return SourceTypeHTTP }

func (s *httpSource) cachedValid() (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.fetched && (s.expiresAt.IsZero() || time.Now().Before(s.expiresAt)) {
		return s.cachedValue, true
	}
	return "", false
}

func (s *httpSource) Resolve(ctx context.Context) (string, error) {
	if val, ok := s.cachedValid(); ok {
		return val, nil
	}

	v, err, _ := s.sf.Do("fetch", func() (any, error) {
		if val, ok := s.cachedValid(); ok {
			return val, nil
		}
		return s.fetch(ctx)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

func (s *httpSource) fetch(ctx context.Context) (string, error) {
	s.mu.RLock()
	url := s.nextURL
	if url == "" {
		url = s.initialURL
	}
	var headers map[string]string
	if s.headersRotated {
		headers = s.nextHeaders
	} else {
		headers = s.initialHeaders
	}
	s.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, s.initialMethod, url, nil)
	if err != nil {
		return "", fmt.Errorf("http credential source: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("http credential source: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http credential source: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCredentialVaultBodyBytes))
	if err != nil {
		return "", fmt.Errorf("http credential source: read body: %w", err)
	}

	var result httpSourceResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("http credential source: parse response: %w", err)
	}
	if result.Value == "" {
		return "", fmt.Errorf("http credential source: response value is empty")
	}

	s.mu.Lock()
	s.cachedValue = result.Value
	s.fetched = true
	if result.TTL != nil {
		s.expiresAt = time.Now().Add(time.Duration(*result.TTL) * time.Second)
	} else {
		s.expiresAt = time.Time{}
	}
	urlValid := result.URL == "" || validateHTTPSourceURL(result.URL) == nil
	headersValid := result.Headers == nil || validateHTTPSourceHeaders(result.Headers) == nil
	if urlValid && headersValid {
		// Determine whether the response actually rotates the URL. Providers
		// that echo the same refresh endpoint should not implicitly clear
		// bootstrap headers, otherwise the next fetch would go to the same
		// endpoint without auth headers. Only an explicit `headers: {}` (or a
		// non-nil rotated map) may clear or replace headers.
		currentURL := s.nextURL
		if currentURL == "" {
			currentURL = s.initialURL
		}
		urlChanged := result.URL != "" && result.URL != currentURL
		if urlChanged {
			s.nextURL = result.URL
			if result.Headers == nil {
				s.nextHeaders = nil
				s.headersRotated = true
			}
		}
		if result.Headers != nil {
			s.nextHeaders = result.Headers
			s.headersRotated = true
		}
	}
	s.mu.Unlock()

	return result.Value, nil
}

func httpSourceFactory(raw json.RawMessage) (CredentialSource, error) {
	var cfg struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Method  string            `json:"method,omitempty"`
		Headers map[string]string `json:"headers,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse http credential source: %w", err)
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("http credential source url cannot be empty")
	}
	if err := validateHTTPSourceURL(cfg.URL); err != nil {
		return nil, err
	}
	if cfg.Method == "" {
		cfg.Method = http.MethodGet
	}
	if _, err := http.NewRequest(cfg.Method, cfg.URL, nil); err != nil {
		return nil, fmt.Errorf("http credential source: invalid method or url: %w", err)
	}
	if err := validateHTTPSourceHeaders(cfg.Headers); err != nil {
		return nil, err
	}
	return &httpSource{
		initialURL:     cfg.URL,
		initialMethod:  cfg.Method,
		initialHeaders: cfg.Headers,
		client: &http.Client{
			Timeout:   httpSourceDefaultTimeout,
			Transport: httpSourceTransport(),
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return errors.New("http credential source: redirects are not allowed")
			},
		},
	}, nil
}

func validateHTTPSourceHeaders(headers map[string]string) error {
	for name, value := range headers {
		if !headerFieldNamePattern.MatchString(name) {
			return fmt.Errorf("http credential source: invalid header name %q", name)
		}
		for i := range value {
			b := value[i]
			if b < 0x20 || b == 0x7f {
				return fmt.Errorf("http credential source: header %q value contains invalid character 0x%02x", name, b)
			}
		}
	}
	return nil
}

func validateHTTPSourceURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("http credential source url: %w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return fmt.Errorf("http credential source url must use http or https scheme, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("http credential source url must include a host")
	}
	return nil
}
