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
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// TransportConfig controls HTTP connection pooling and keep-alive behavior.
type TransportConfig struct {
	// MaxIdleConns is the maximum total idle connections across all hosts.
	MaxIdleConns int

	// MaxIdleConnsPerHost is the maximum idle connections kept per host.
	// Go's default is 2, which is too low for SDKs talking to multiple
	// sandbox endpoints concurrently.
	MaxIdleConnsPerHost int

	// IdleConnTimeout is how long an idle connection stays in the pool
	// before being closed.
	IdleConnTimeout time.Duration

	// TLSHandshakeTimeout limits the TLS handshake duration.
	TLSHandshakeTimeout time.Duration

	// ResponseHeaderTimeout caps the time spent waiting for a server's
	// response headers after fully writing the request. Unlike
	// http.Client.Timeout, it does NOT cover reading the response body,
	// so it is safe to apply to long-lived SSE streams while still
	// bounding the pre-stream handshake. Zero disables the bound.
	ResponseHeaderTimeout time.Duration

	// DialTimeout limits TCP connection establishment.
	DialTimeout time.Duration

	// KeepAlive sets the TCP keep-alive probe interval.
	KeepAlive time.Duration

	// AllowWeakServerCertKeyLengths allows server certificates below NIST minimum
	// key/hash lengths. Keep false unless interoperability requires legacy certs.
	AllowWeakServerCertKeyLengths bool
}

// DefaultTransportConfig returns connection pool settings tuned for SDK
// workloads: moderate concurrency across multiple sandbox endpoints.
//
// IdleConnTimeout is intentionally lower than Go's stdlib default of 90s.
// Many enterprise load balancers silently drop idle TCP connections after
// 60s without sending FIN/RST. If the SDK holds a connection idle past
// that point, the next request reuses a black-holed connection: the
// request writes succeed locally but no response headers ever arrive,
// causing the client to hang until http.Client.Timeout fires with
// "Client.Timeout exceeded while awaiting headers". Evicting idle
// connections well before the typical LB idle timeout avoids this class
// of intermittent failure. Callers that know their infrastructure keeps
// connections alive longer can override via TransportConfig.
func DefaultTransportConfig() TransportConfig {
	return TransportConfig{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     25 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		// Bounds the response-header phase for every request, including
		// SSE streams that intentionally leave http.Client.Timeout at 0.
		// Without this, a server or LB that accepts the TCP connection
		// but never emits response headers would make streaming calls
		// (ExecuteCode / RunCommand / WatchMetrics) hang indefinitely.
		ResponseHeaderTimeout:         30 * time.Second,
		DialTimeout:                   30 * time.Second,
		KeepAlive:                     30 * time.Second,
		AllowWeakServerCertKeyLengths: false,
	}
}

// NewTransport creates an *http.Transport from the config.
func (tc TransportConfig) NewTransport() *http.Transport {
	tlsClientConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if !tc.AllowWeakServerCertKeyLengths {
		tlsClientConfig.VerifyConnection = enforceNISTPeerCertificateMinimums
	}

	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   tc.DialTimeout,
			KeepAlive: tc.KeepAlive,
		}).DialContext,
		MaxIdleConns:          tc.MaxIdleConns,
		MaxIdleConnsPerHost:   tc.MaxIdleConnsPerHost,
		IdleConnTimeout:       tc.IdleConnTimeout,
		TLSHandshakeTimeout:   tc.TLSHandshakeTimeout,
		ResponseHeaderTimeout: tc.ResponseHeaderTimeout,
		TLSClientConfig:       tlsClientConfig,
	}
}

// DefaultTransport creates an *http.Transport with connection pooling
// tuned for SDK workloads. Use with WithHTTPClient:
//
//	client := NewLifecycleClient(url, key,
//	    WithHTTPClient(&http.Client{Transport: DefaultTransport()}),
//	)
func DefaultTransport() *http.Transport {
	return DefaultTransportConfig().NewTransport()
}
