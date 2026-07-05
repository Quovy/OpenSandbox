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
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const httpSourceDefaultTimeout = 10 * time.Second

type httpSource struct {
	mu sync.RWMutex
	sf singleflight.Group

	initialURL     string
	initialMethod  string
	initialHeaders map[string]string

	nextURL     string
	nextHeaders map[string]string

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
	headers := s.nextHeaders
	if len(headers) == 0 {
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
	if result.URL != "" {
		s.nextURL = result.URL
	}
	if len(result.Headers) > 0 {
		s.nextHeaders = result.Headers
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
	if cfg.Method == "" {
		cfg.Method = http.MethodGet
	}
	return &httpSource{
		initialURL:     cfg.URL,
		initialMethod:  cfg.Method,
		initialHeaders: cfg.Headers,
		client:         &http.Client{Timeout: httpSourceDefaultTimeout},
	}, nil
}
