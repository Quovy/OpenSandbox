// Copyright 2025 Alibaba Group Holding Ltd.
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

package execute

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	execdflag "github.com/alibaba/opensandbox/execd/pkg/flag"
)

// Create WebSocket test server
func createTestServer(t *testing.T, handleFunc func(conn *websocket.Conn)) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Validate request path
		if !strings.HasPrefix(r.URL.Path, "/api/kernels/") {
			t.Errorf("expected path to start with '/api/kernels/', got '%s'", r.URL.Path)
		}
		if !strings.HasSuffix(r.URL.Path, "/channels") {
			t.Errorf("expected path to end with '/channels', got '%s'", r.URL.Path)
		}

		// Upgrade HTTP connection to WebSocket
		upgrader := websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("failed to upgrade to WebSocket: %v", err)
		}
		defer conn.Close()

		// Handle WebSocket connection
		handleFunc(conn)
	}))

	return server
}

// Test streaming code execution
func TestExecuteCodeStream(t *testing.T) {
	// Spin up mock WebSocket server
	server := createTestServer(t, func(conn *websocket.Conn) {
		// Read execution request
		var executeRequest Message
		err := conn.ReadJSON(&executeRequest)
		if err != nil {
			t.Fatalf("failed to read execution request: %v", err)
		}

		// Send multiple stream messages
		for i := 0; i < 3; i++ {
			streamContent, _ := json.Marshal(StreamOutput{
				Name: StreamStdout,
				Text: "Line " + string(rune('0'+i)) + "\n",
			})

			streamMsg := Message{
				Header: Header{
					MessageID:   "stream-msg-id-" + string(rune('0'+i)),
					Session:     executeRequest.Header.Session,
					MessageType: string(MsgStream),
				},
				ParentHeader: executeRequest.Header,
				Content:      json.RawMessage(streamContent),
			}
			conn.WriteJSON(streamMsg)
			time.Sleep(100 * time.Millisecond)
		}

		// Send execution result
		resultContent, _ := json.Marshal(ExecuteResult{
			ExecutionCount: 1,
			Data: map[string]interface{}{
				"text/plain": "Completed",
			},
			Metadata: map[string]interface{}{},
		})

		executeResultMsg := Message{
			Header: Header{
				MessageID:   "result-msg-id",
				Session:     executeRequest.Header.Session,
				MessageType: string(MsgExecuteResult),
			},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(resultContent),
		}
		conn.WriteJSON(executeResultMsg)

		// Send status message
		statusContent, _ := json.Marshal(StatusUpdate{
			ExecutionState: StateIdle,
		})

		statusMsg := Message{
			Header: Header{
				MessageID:   "status-msg-id",
				Session:     executeRequest.Header.Session,
				MessageType: string(MsgStatus),
			},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(statusContent),
		}
		conn.WriteJSON(statusMsg)
	})
	defer server.Close()

	// Convert HTTP URL to WebSocket URL
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/test-kernel-id/channels"

	// Create executor client
	executor := NewExecutor(wsURL, nil)

	// Connect to WebSocket
	err := executor.Connect()
	if err != nil {
		t.Fatalf("failed to connect to WebSocket: %v", err)
	}
	defer executor.Disconnect()

	// Execute code in streaming mode
	resultChan := make(chan *ExecutionResult, 10)
	err = executor.ExecuteCodeStream("for i in range(3):\n    print(f'Line {i}')", resultChan)
	if err != nil {
		t.Fatalf("failed to start streaming execution: %v", err)
	}

	// Receive and verify stream results
	resultCount := 0
	for result := range resultChan {
		if result == nil {
			break
		}
		resultCount++
	}

	// Should receive at least 4 results (3 stream outputs + 1 final result)
	if resultCount < 4 {
		t.Errorf("expected at least 4 results, got %d", resultCount)
	}
}

func TestExecuteCodeStreamWaitsForLateExecuteResultUsingConfiguredPollInterval(t *testing.T) {
	previousPollInterval := execdflag.JupyterIdlePollInterval
	execdflag.JupyterIdlePollInterval = time.Millisecond
	t.Cleanup(func() {
		execdflag.JupyterIdlePollInterval = previousPollInterval
	})

	server := createTestServer(t, func(conn *websocket.Conn) {
		var executeRequest Message
		err := conn.ReadJSON(&executeRequest)
		if err != nil {
			t.Fatalf("failed to read execution request: %v", err)
		}

		statusContent, _ := json.Marshal(StatusUpdate{ExecutionState: StateIdle})
		statusMsg := Message{
			Header: Header{
				MessageID:   "status-msg-id",
				Session:     executeRequest.Header.Session,
				MessageType: string(MsgStatus),
			},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(statusContent),
		}
		require.NoError(t, conn.WriteJSON(statusMsg))

		time.Sleep(15 * time.Millisecond)

		resultContent, _ := json.Marshal(ExecuteResult{
			ExecutionCount: 1,
			Data: map[string]interface{}{
				"text/plain": "Completed late",
			},
			Metadata: map[string]interface{}{},
		})
		executeResultMsg := Message{
			Header: Header{
				MessageID:   "result-msg-id",
				Session:     executeRequest.Header.Session,
				MessageType: string(MsgExecuteResult),
			},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(resultContent),
		}
		require.NoError(t, conn.WriteJSON(executeResultMsg))
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/test-kernel-id/channels"
	executor := NewExecutor(wsURL, nil)
	require.NoError(t, executor.Connect())
	defer executor.Disconnect()

	resultChan := make(chan *ExecutionResult, 10)
	require.NoError(t, executor.ExecuteCodeStream("print('late result')", resultChan))

	start := time.Now()
	var gotLateResult bool
	for result := range resultChan {
		if result != nil && result.ExecutionCount == 1 {
			gotLateResult = true
		}
	}
	elapsed := time.Since(start)

	require.True(t, gotLateResult, "expected late execute_result to be delivered before stream close")
	require.Less(t, elapsed, 100*time.Millisecond, "expected stream to close promptly after late execute_result")
}

func TestExecuteCodeStreamFallsBackWhenPollIntervalIsNonPositive(t *testing.T) {
	previousPollInterval := execdflag.JupyterIdlePollInterval
	execdflag.JupyterIdlePollInterval = 0
	t.Cleanup(func() {
		execdflag.JupyterIdlePollInterval = previousPollInterval
	})

	server := createTestServer(t, func(conn *websocket.Conn) {
		var executeRequest Message
		err := conn.ReadJSON(&executeRequest)
		if err != nil {
			t.Fatalf("failed to read execution request: %v", err)
		}

		statusContent, _ := json.Marshal(StatusUpdate{ExecutionState: StateIdle})
		statusMsg := Message{
			Header: Header{
				MessageID:   "status-msg-id",
				Session:     executeRequest.Header.Session,
				MessageType: string(MsgStatus),
			},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(statusContent),
		}
		require.NoError(t, conn.WriteJSON(statusMsg))

		time.Sleep(15 * time.Millisecond)

		resultContent, _ := json.Marshal(ExecuteResult{
			ExecutionCount: 1,
			Data: map[string]interface{}{
				"text/plain": "Completed with fallback",
			},
			Metadata: map[string]interface{}{},
		})
		executeResultMsg := Message{
			Header: Header{
				MessageID:   "result-msg-id",
				Session:     executeRequest.Header.Session,
				MessageType: string(MsgExecuteResult),
			},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(resultContent),
		}
		require.NoError(t, conn.WriteJSON(executeResultMsg))
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/test-kernel-id/channels"
	executor := NewExecutor(wsURL, nil)
	require.NoError(t, executor.Connect())
	defer executor.Disconnect()

	resultChan := make(chan *ExecutionResult, 10)
	require.NoError(t, executor.ExecuteCodeStream("print('fallback')", resultChan))

	start := time.Now()
	var gotLateResult bool
	for result := range resultChan {
		if result != nil && result.ExecutionCount == 1 {
			gotLateResult = true
		}
	}
	elapsed := time.Since(start)

	require.True(t, gotLateResult, "expected late execute_result to be delivered before stream close")
	require.GreaterOrEqual(t, elapsed, 90*time.Millisecond, "expected non-positive poll interval to fall back to runtime default (100ms)")
	require.Less(t, elapsed, 300*time.Millisecond, "expected fallback poll interval to still close stream promptly")
}

// Reproduces issue #1206: kernel goes idle but no execute_reply/error ever
// arrives. finalizeExecution must bail out within JupyterIdleGracePeriod
// with a synthetic error instead of polling forever.
func TestExecuteCodeStreamGivesUpWhenExecuteReplyNeverArrives(t *testing.T) {
	previousPollInterval := execdflag.JupyterIdlePollInterval
	previousGracePeriod := execdflag.JupyterIdleGracePeriod
	execdflag.JupyterIdlePollInterval = 5 * time.Millisecond
	execdflag.JupyterIdleGracePeriod = 60 * time.Millisecond
	t.Cleanup(func() {
		execdflag.JupyterIdlePollInterval = previousPollInterval
		execdflag.JupyterIdleGracePeriod = previousGracePeriod
	})

	// Mock kernel only publishes idle; no execute_reply/execute_result/error.
	server := createTestServer(t, func(conn *websocket.Conn) {
		var executeRequest Message
		require.NoError(t, conn.ReadJSON(&executeRequest))

		statusContent, _ := json.Marshal(StatusUpdate{ExecutionState: StateIdle})
		require.NoError(t, conn.WriteJSON(Message{
			Header: Header{
				MessageID:   "status-msg-id",
				Session:     executeRequest.Header.Session,
				MessageType: string(MsgStatus),
			},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(statusContent),
		}))

		// Hold the WS open so the grace-period path runs, not the disconnect path.
		time.Sleep(500 * time.Millisecond)
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/test-kernel-id/channels"
	executor := NewExecutor(wsURL, nil)
	require.NoError(t, executor.Connect())
	defer executor.Disconnect()

	resultChan := make(chan *ExecutionResult, 10)
	require.NoError(t, executor.ExecuteCodeStream("print('missing reply')", resultChan))

	start := time.Now()
	var terminalErr *ErrorOutput
	for result := range resultChan {
		if result != nil && result.Error != nil {
			terminalErr = result.Error
		}
	}
	elapsed := time.Since(start)

	require.NotNil(t, terminalErr, "expected a synthetic error when execute_reply never arrives")
	require.Equal(t, "KernelReplyTimeout", terminalErr.EName,
		"expected KernelReplyTimeout to be surfaced when idle grace period expires")
	require.GreaterOrEqual(t, elapsed, 50*time.Millisecond,
		"expected finalizeExecution to wait roughly one grace period before giving up")
	require.Less(t, elapsed, 400*time.Millisecond,
		"expected finalizeExecution to close the stream shortly after the grace period")
}

// Regression for PR #1321 review: on the idle-grace timeout path, no
// ExecutionTime-only "completion" notify may be emitted before the error.
// dispatchExecutionResultHooks would otherwise fire OnExecuteComplete first
// and misreport a hung execution as successful.
func TestExecuteCodeStreamDoesNotSignalCompletionBeforeSyntheticError(t *testing.T) {
	previousPollInterval := execdflag.JupyterIdlePollInterval
	previousGracePeriod := execdflag.JupyterIdleGracePeriod
	execdflag.JupyterIdlePollInterval = 5 * time.Millisecond
	execdflag.JupyterIdleGracePeriod = 40 * time.Millisecond
	t.Cleanup(func() {
		execdflag.JupyterIdlePollInterval = previousPollInterval
		execdflag.JupyterIdleGracePeriod = previousGracePeriod
	})

	server := createTestServer(t, func(conn *websocket.Conn) {
		var executeRequest Message
		require.NoError(t, conn.ReadJSON(&executeRequest))

		statusContent, _ := json.Marshal(StatusUpdate{ExecutionState: StateIdle})
		require.NoError(t, conn.WriteJSON(Message{
			Header: Header{
				MessageID:   "status-msg-id",
				Session:     executeRequest.Header.Session,
				MessageType: string(MsgStatus),
			},
			ParentHeader: executeRequest.Header,
			Content:      json.RawMessage(statusContent),
		}))

		time.Sleep(300 * time.Millisecond)
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/test-kernel-id/channels"
	executor := NewExecutor(wsURL, nil)
	require.NoError(t, executor.Connect())
	defer executor.Disconnect()

	resultChan := make(chan *ExecutionResult, 10)
	require.NoError(t, executor.ExecuteCodeStream("print('missing reply')", resultChan))

	var results []*ExecutionResult
	for r := range resultChan {
		if r != nil {
			results = append(results, r)
		}
	}

	require.NotEmpty(t, results, "expected at least one terminal result")
	for i, r := range results {
		// A pre-error "success completion" notify has ExecutionTime > 0 and
		// no Error. That is exactly the shape dispatchExecutionResultHooks
		// treats as OnExecuteComplete — must never appear on the error path.
		if r.Error == nil && r.ExecutionTime > 0 {
			t.Fatalf("result[%d] looks like a success-completion notify on the error path: %+v (all results: %+v)", i, r, results)
		}
	}
	// The final notify must carry the synthetic error.
	last := results[len(results)-1]
	require.NotNil(t, last.Error, "final notify on the error path must carry the error")
	require.Equal(t, "KernelReplyTimeout", last.Error.EName)
}

// WebSocket dies before idle: receiveMessages must fail the stream instead
// of silently break-ing and leaving the caller blocked on resultChan.
func TestExecuteCodeStreamFailsWhenWebSocketDiesBeforeIdle(t *testing.T) {
	server := createTestServer(t, func(conn *websocket.Conn) {
		var executeRequest Message
		require.NoError(t, conn.ReadJSON(&executeRequest))
		// Drop the connection with no idle/reply.
		_ = conn.Close()
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/test-kernel-id/channels"
	executor := NewExecutor(wsURL, nil)
	require.NoError(t, executor.Connect())
	defer executor.Disconnect()

	resultChan := make(chan *ExecutionResult, 10)
	require.NoError(t, executor.ExecuteCodeStream("print('lost socket')", resultChan))

	done := make(chan *ErrorOutput, 1)
	go func() {
		var terminalErr *ErrorOutput
		for result := range resultChan {
			if result != nil && result.Error != nil {
				terminalErr = result.Error
			}
		}
		done <- terminalErr
	}()

	select {
	case terminalErr := <-done:
		require.NotNil(t, terminalErr, "expected a synthetic error when WebSocket dies mid-execution")
		require.Equal(t, "KernelStreamAborted", terminalErr.EName,
			"expected KernelStreamAborted when the read loop exits before idle")
	case <-time.After(2 * time.Second):
		t.Fatal("resultChan was never closed after WebSocket death — codes.run() would hang")
	}
}

// Explicit Disconnect() mid-execution must release the caller with a typed
// error instead of leaking resultChan.
func TestExecuteCodeStreamFailsWhenDisconnectedMidExecution(t *testing.T) {
	serverReady := make(chan struct{})
	server := createTestServer(t, func(conn *websocket.Conn) {
		var executeRequest Message
		require.NoError(t, conn.ReadJSON(&executeRequest))
		close(serverReady)
		// Block until the client Disconnects.
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	})
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/kernels/test-kernel-id/channels"
	executor := NewExecutor(wsURL, nil)
	require.NoError(t, executor.Connect())

	resultChan := make(chan *ExecutionResult, 10)
	require.NoError(t, executor.ExecuteCodeStream("print('will be interrupted')", resultChan))

	<-serverReady

	done := make(chan *ErrorOutput, 1)
	go func() {
		var terminalErr *ErrorOutput
		for result := range resultChan {
			if result != nil && result.Error != nil {
				terminalErr = result.Error
			}
		}
		done <- terminalErr
	}()

	executor.Disconnect()

	select {
	case terminalErr := <-done:
		require.NotNil(t, terminalErr, "expected a synthetic error when Disconnect races an in-flight execution")
		// Disconnect and receiveMessages exit races; either terminal is valid.
		require.Contains(t, []string{"KernelDisconnected", "KernelStreamAborted"}, terminalErr.EName,
			"expected a documented disconnect-related terminal error")
	case <-time.After(2 * time.Second):
		t.Fatal("resultChan was never closed after Disconnect — codes.run() would hang")
	}
}
