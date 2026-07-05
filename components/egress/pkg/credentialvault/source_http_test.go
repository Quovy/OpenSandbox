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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHttpSourceResolveBasic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"value":"my-secret"}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  srv.URL,
	}))
	require.NoError(t, err)
	require.Equal(t, "http", src.Type())

	val, err := src.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "my-secret", val)
}

func TestHttpSourceCachesWithNilTTL(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprintf(w, `{"value":"cached-forever"}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  srv.URL,
	}))
	require.NoError(t, err)

	for i := 0; i < 5; i++ {
		val, err := src.Resolve(context.Background())
		require.NoError(t, err)
		require.Equal(t, "cached-forever", val)
	}
	require.Equal(t, int32(1), calls.Load())
}

func TestHttpSourceRefetchesAfterTTLExpires(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		fmt.Fprintf(w, `{"value":"v%d","ttl":0}`, n)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  srv.URL,
	}))
	require.NoError(t, err)

	v1, err := src.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v1", v1)

	v2, err := src.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v2", v2)

	require.Equal(t, int32(2), calls.Load())
}

func TestHttpSourceDynamicURLAndHeaders(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			require.Equal(t, "/initial", r.URL.Path)
			require.Equal(t, "boot", r.Header.Get("X-Auth"))
			resp := httpSourceResponse{
				Value:   "first",
				URL:     fmt.Sprintf("http://%s/refreshed", r.Host),
				Headers: map[string]string{"X-Auth": "renewed"},
				TTL:     intPtr(0),
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		require.Equal(t, "/refreshed", r.URL.Path)
		require.Equal(t, "renewed", r.Header.Get("X-Auth"))
		fmt.Fprintf(w, `{"value":"second","ttl":0}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]any{
		"type":    "http",
		"url":     srv.URL + "/initial",
		"headers": map[string]string{"X-Auth": "boot"},
	}))
	require.NoError(t, err)

	v1, err := src.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "first", v1)

	v2, err := src.Resolve(context.Background())
	require.NoError(t, err)
	require.Equal(t, "second", v2)
}

func TestHttpSourceReturnsErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  srv.URL,
	}))
	require.NoError(t, err)

	_, err = src.Resolve(context.Background())
	require.ErrorContains(t, err, "status 403")
}

func TestHttpSourceReturnsErrorOnEmptyValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"value":""}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  srv.URL,
	}))
	require.NoError(t, err)

	_, err = src.Resolve(context.Background())
	require.ErrorContains(t, err, "response value is empty")
}

func TestHttpSourceFactoryRejectsEmptyURL(t *testing.T) {
	_, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  "",
	}))
	require.ErrorContains(t, err, "url cannot be empty")
}

func TestHttpSourceFactoryRejectsUnknownFields(t *testing.T) {
	_, err := httpSourceFactory(mustMarshal(map[string]string{
		"type":    "http",
		"url":     "https://example.com",
		"unknown": "field",
	}))
	require.Error(t, err)
}

func TestHttpSourceFactoryDefaultsMethodToGET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		fmt.Fprintf(w, `{"value":"ok"}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  srv.URL,
	}))
	require.NoError(t, err)

	_, err = src.Resolve(context.Background())
	require.NoError(t, err)
}

func TestHttpSourceCustomMethod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		fmt.Fprintf(w, `{"value":"ok"}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]any{
		"type":   "http",
		"url":    srv.URL,
		"method": "POST",
	}))
	require.NoError(t, err)

	_, err = src.Resolve(context.Background())
	require.NoError(t, err)
}

func TestHttpSourceRegisteredInDefaultRegistry(t *testing.T) {
	r := NewSourceRegistry()
	types := r.SupportedTypes()
	found := false
	for _, typ := range types {
		if typ == "http" {
			found = true
			break
		}
	}
	require.True(t, found, "http source type should be registered in default registry")
}

func TestHttpSourceConcurrentResolveSingleFetch(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintf(w, `{"value":"shared"}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  srv.URL,
	}))
	require.NoError(t, err)

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			val, err := src.Resolve(context.Background())
			require.NoError(t, err)
			require.Equal(t, "shared", val)
		}()
	}
	wg.Wait()

	require.Equal(t, int32(1), calls.Load(), "singleflight should coalesce concurrent fetches into one")
}

func TestHttpSourceFactoryRejectsNonHTTPScheme(t *testing.T) {
	_, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  "ftp://vault.example.com/cred",
	}))
	require.ErrorContains(t, err, "must use http or https scheme")
}

func TestHttpSourceFactoryRejectsRelativeURL(t *testing.T) {
	_, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  "/token",
	}))
	require.ErrorContains(t, err, "must use http or https scheme")
}

func TestHttpSourceFactoryRejectsHostlessURL(t *testing.T) {
	for _, u := range []string{"http:/token", "https:///token", "http://"} {
		_, err := httpSourceFactory(mustMarshal(map[string]string{
			"type": "http",
			"url":  u,
		}))
		require.ErrorContains(t, err, "must include a host", u)
	}
}

func TestHttpSourceFactoryRejectsInvalidMethod(t *testing.T) {
	_, err := httpSourceFactory(mustMarshal(map[string]any{
		"type":   "http",
		"url":    "https://vault.example.com/cred",
		"method": "BAD METHOD",
	}))
	require.ErrorContains(t, err, "invalid method")
}

func TestHttpSourceFactoryRejectsInvalidHeaderName(t *testing.T) {
	_, err := httpSourceFactory(mustMarshal(map[string]any{
		"type":    "http",
		"url":     "https://vault.example.com/cred",
		"headers": map[string]string{"Bad Header": "value"},
	}))
	require.ErrorContains(t, err, "invalid header name")
}

func TestHttpSourceFactoryRejectsHeaderValueWithCRLF(t *testing.T) {
	_, err := httpSourceFactory(mustMarshal(map[string]any{
		"type":    "http",
		"url":     "https://vault.example.com/cred",
		"headers": map[string]string{"X-Auth": "value\r\ninjection"},
	}))
	require.ErrorContains(t, err, "CR/LF")
}

func TestHttpSourceRejectsRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.com", http.StatusFound)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]string{
		"type": "http",
		"url":  srv.URL,
	}))
	require.NoError(t, err)

	_, err = src.Resolve(context.Background())
	require.ErrorContains(t, err, "redirects are not allowed")
}

func TestHttpSourceHeadersRotationClearsBootstrapHeaders(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			require.Equal(t, "boot", r.Header.Get("X-Auth"))
			fmt.Fprintf(w, `{"value":"first","url":"http://%s/refreshed","headers":{},"ttl":0}`, r.Host)
			return
		}
		require.Equal(t, "", r.Header.Get("X-Auth"), "bootstrap header should not leak after rotation")
		fmt.Fprintf(w, `{"value":"second","ttl":0}`)
	}))
	defer srv.Close()

	src, err := httpSourceFactory(mustMarshal(map[string]any{
		"type":    "http",
		"url":     srv.URL,
		"headers": map[string]string{"X-Auth": "boot"},
	}))
	require.NoError(t, err)

	_, err = src.Resolve(context.Background())
	require.NoError(t, err)

	_, err = src.Resolve(context.Background())
	require.NoError(t, err)
}

func intPtr(v int) *int { return &v }
