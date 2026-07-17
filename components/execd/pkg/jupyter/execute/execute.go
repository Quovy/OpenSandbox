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

// Package execute provides functionality for executing Jupyter kernel code via WebSocket
package execute

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alibaba/opensandbox/internal/safego"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	execdflag "github.com/alibaba/opensandbox/execd/pkg/flag"
)

// HTTPClient defines the HTTP client interface
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client is the client for code execution
type Client struct {
	// Internal HTTP client for sending HTTP requests
	httpClient HTTPClient

	// WebSocket connection
	conn *websocket.Conn

	// Message handler mappings
	handlers map[MessageType]func(*Message)

	// Session ID
	session string

	// Message ID counter
	msgCounter int

	// Mutex for protecting concurrent access
	mu sync.Mutex

	// WebSocket URL for kernel connection
	wsURL string

	// activeStream is the in-flight streaming execution, if any. Used by
	// receiveMessages/Disconnect to fail it when the WebSocket goes away.
	activeStream *streamExecutionState
}

// NewClient creates a new code execution client
func NewClient(baseURL string, httpClient HTTPClient) *Client {
	return &Client{
		httpClient: httpClient,
		handlers:   make(map[MessageType]func(*Message)),
		session:    uuid.New().String(),
		msgCounter: 0,
	}
}

// Connect connects to the WebSocket of the specified kernel
func (c *Client) Connect(wsURL string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Save WebSocket URL
	c.wsURL = wsURL

	// Connect to WebSocket
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil && err != nil {
		resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("failed to connect to kernel: %w", err)
	}
	c.conn = conn

	// Register default message handlers
	c.registerDefaultHandlers()

	// Start message receiving goroutine
	safego.Go(func() { c.receiveMessages() })

	return nil
}

// Disconnect disconnects the WebSocket connection to the kernel
func (c *Client) Disconnect() {
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	active := c.activeStream
	c.mu.Unlock()

	// Fail any in-flight execution not yet in finalization; otherwise
	// finalizeExecution's grace period will wind it down on its own.
	if active != nil {
		active.executeMutex.Lock()
		alreadyFinalizing := active.executeDone
		active.executeMutex.Unlock()
		if !alreadyFinalizing {
			c.clearActiveStream(active)
			active.terminate(&ErrorOutput{
				EName:  "KernelDisconnected",
				EValue: "kernel WebSocket was disconnected before execution completed",
			})
		}
	}
}

// IsConnected checks if connected to the kernel
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

type streamExecutionState struct {
	startTime    time.Time
	result       *ExecutionResult
	executeDone  bool
	executeMutex sync.Mutex
	resultMutex  sync.Mutex

	// resultChan is the terminal result channel; closed exactly once.
	resultChan chan *ExecutionResult
	// closed lets concurrent senders skip after terminate() has closed the chan.
	closed atomic.Bool
	// closeOnce guards the single close + terminal-error injection.
	closeOnce sync.Once
}

func newStreamExecutionState(startTime time.Time, resultChan chan *ExecutionResult) *streamExecutionState {
	return &streamExecutionState{
		startTime: startTime,
		result: &ExecutionResult{
			Status:        "ok",
			Stream:        make([]*StreamOutput, 0),
			ExecutionTime: 0,
		},
		resultChan: resultChan,
	}
}

// trySend delivers a result to resultChan, safe against a concurrent terminate().
func (s *streamExecutionState) trySend(r *ExecutionResult) {
	if s.closed.Load() {
		return
	}
	// terminate() may close the chan between the check above and the send below.
	defer func() { _ = recover() }()
	s.resultChan <- r
}

// terminate optionally injects a synthetic error and closes resultChan. Idempotent.
func (s *streamExecutionState) terminate(errOutput *ErrorOutput) {
	s.closeOnce.Do(func() {
		if errOutput != nil {
			s.resultMutex.Lock()
			if s.result.Error == nil {
				s.result.Error = errOutput
				s.result.Status = "error"
			}
			s.resultMutex.Unlock()

			// Best-effort; if reader is gone, skip rather than block.
			select {
			case s.resultChan <- &ExecutionResult{Status: "error", Error: errOutput}:
			default:
			}
		}
		s.closed.Store(true)
		close(s.resultChan)
	})
}

// ExecuteCodeStream executes code in streaming mode, sending results to the provided channel
func (c *Client) ExecuteCodeStream(code string, resultChan chan *ExecutionResult) error {
	if !c.IsConnected() {
		return errors.New("not connected to kernel, please call Connect method")
	}

	msg, err := c.buildExecuteMessage(code)
	if err != nil {
		return err
	}

	state := newStreamExecutionState(time.Now(), resultChan)

	// Clear temporary handlers
	c.clearTemporaryHandlers()
	c.registerExecuteCodeStreamHandlers(state, resultChan)

	// Publish before the write so receive/Disconnect can fail it if the WS dies.
	c.mu.Lock()
	c.activeStream = state
	c.mu.Unlock()

	if err := c.writeMessage(msg); err != nil {
		c.clearActiveStream(state)
		return fmt.Errorf("failed to send execution request: %w", err)
	}

	return nil
}

func (c *Client) clearActiveStream(state *streamExecutionState) {
	c.mu.Lock()
	if c.activeStream == state {
		c.activeStream = nil
	}
	c.mu.Unlock()
}

func (c *Client) currentActiveStream() *streamExecutionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeStream
}

func (c *Client) buildExecuteMessage(code string) (*Message, error) {
	msgID := c.nextMessageID()
	request := &ExecuteRequest{
		Code:            code,
		Silent:          false,
		StoreHistory:    true,
		UserExpressions: make(map[string]string),
		AllowStdin:      false,
		StopOnError:     true,
	}

	content, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize request: %w", err)
	}

	msg := &Message{
		Header: Header{
			MessageID:   msgID,
			Username:    "go-client",
			Session:     c.session,
			Date:        time.Now().Format(time.RFC3339),
			MessageType: string(MsgExecuteRequest),
			Version:     "5.3",
		},
		ParentHeader: Header{},
		Metadata:     make(map[string]interface{}),
		Content:      content,
		Channel:      "shell",
	}

	return msg, nil
}

func (c *Client) registerExecuteCodeStreamHandlers(state *streamExecutionState, resultChan chan *ExecutionResult) {
	c.registerHandler(MsgExecuteReply, func(msg *Message) {
		c.handleExecuteReply(msg, state)
	})
	c.registerHandler(MsgExecuteResult, func(msg *Message) {
		c.handleExecuteResult(msg, state, resultChan)
	})
	c.registerHandler(MsgStream, func(msg *Message) {
		c.handleStreamOutput(msg, state, resultChan)
	})
	c.registerHandler(MsgError, func(msg *Message) {
		c.handleExecutionError(msg, state, resultChan)
	})
	c.registerHandler(MsgStatus, func(msg *Message) {
		c.handleExecutionStatus(msg, state, resultChan)
	})
}

func (c *Client) handleExecuteReply(msg *Message, state *streamExecutionState) {
	var execReply ExecuteReply
	if err := json.Unmarshal(msg.Content, &execReply); err != nil {
		return
	}

	state.resultMutex.Lock()
	defer state.resultMutex.Unlock()
	state.result.ExecutionCount = execReply.ExecutionCount
	if execReply.EName != "" {
		state.result.Error = &execReply.ErrorOutput
	}
}

func (c *Client) handleExecuteResult(msg *Message, state *streamExecutionState, resultChan chan *ExecutionResult) {
	var execResult ExecuteResult
	if err := json.Unmarshal(msg.Content, &execResult); err != nil {
		return
	}

	state.resultMutex.Lock()
	defer state.resultMutex.Unlock()
	state.result.ExecutionCount = execResult.ExecutionCount

	notify := &ExecutionResult{
		ExecutionCount: execResult.ExecutionCount,
		ExecutionData:  execResult.Data,
	}
	state.trySend(notify)
}

func (c *Client) handleStreamOutput(msg *Message, state *streamExecutionState, resultChan chan *ExecutionResult) {
	var stream StreamOutput
	if err := json.Unmarshal(msg.Content, &stream); err != nil {
		return
	}

	state.resultMutex.Lock()
	defer state.resultMutex.Unlock()
	state.result.Stream = append(state.result.Stream, &stream)
	notify := &ExecutionResult{
		Stream: []*StreamOutput{&stream},
	}
	state.trySend(notify)
}

func (c *Client) handleExecutionError(msg *Message, state *streamExecutionState, resultChan chan *ExecutionResult) {
	var errOutput ErrorOutput
	if err := json.Unmarshal(msg.Content, &errOutput); err != nil {
		return
	}

	state.resultMutex.Lock()
	defer state.resultMutex.Unlock()
	state.result.Status = "error"
	state.result.Error = &errOutput
	notify := &ExecutionResult{
		Error:  &errOutput,
		Status: "error",
	}
	state.trySend(notify)
}

func (c *Client) handleExecutionStatus(msg *Message, state *streamExecutionState, resultChan chan *ExecutionResult) {
	var status StatusUpdate
	if err := json.Unmarshal(msg.Content, &status); err != nil {
		return
	}
	if status.ExecutionState != StateIdle {
		return
	}

	state.executeMutex.Lock()
	defer state.executeMutex.Unlock()
	if state.executeDone {
		return
	}
	state.executeDone = true
	safego.Go(func() { c.finalizeExecution(state, resultChan) })
}

func (c *Client) finalizeExecution(state *streamExecutionState, resultChan chan *ExecutionResult) {
	defer c.clearActiveStream(state)

	state.resultMutex.Lock()
	state.result.ExecutionTime = time.Since(state.startTime)
	notify := &ExecutionResult{
		ExecutionTime: state.result.ExecutionTime,
	}
	state.resultMutex.Unlock()
	state.trySend(notify)

	pollInterval := execdflag.JupyterIdlePollInterval
	if pollInterval <= 0 {
		pollInterval = 100 * time.Millisecond
	}
	gracePeriod := execdflag.JupyterIdleGracePeriod
	if gracePeriod <= 0 {
		gracePeriod = 5 * time.Second
	}

	// Wait for a late execute_reply/error, but bail out after gracePeriod
	// with a synthetic error rather than hanging forever. See issue #1206.
	deadline := time.Now().Add(gracePeriod)
	var syntheticErr *ErrorOutput
	for {
		state.resultMutex.Lock()
		done := state.result.ExecutionCount > 0 || state.result.Error != nil
		state.resultMutex.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			syntheticErr = &ErrorOutput{
				EName: "KernelReplyTimeout",
				EValue: fmt.Sprintf(
					"kernel reported idle but no execute_reply arrived within %s; the shell-channel reply was likely lost",
					gracePeriod,
				),
			}
			break
		}
		time.Sleep(pollInterval)
	}

	state.terminate(syntheticErr)
}

func (c *Client) writeMessage(msg *Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteJSON(msg)
}

// ExecuteCodeWithCallback executes code using callback functions
func (c *Client) ExecuteCodeWithCallback(code string, handler CallbackHandler) error {
	if !c.IsConnected() {
		return errors.New("not connected to kernel, please call Connect method")
	}

	// prepare execution request
	msgID := c.nextMessageID()
	request := &ExecuteRequest{
		Code:            code,
		Silent:          false,
		StoreHistory:    true,
		UserExpressions: make(map[string]string),
		AllowStdin:      false,
		StopOnError:     true,
	}

	// serialize request content
	content, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("failed to serialize request: %w", err)
	}

	// create message
	msg := &Message{
		Header: Header{
			MessageID:   msgID,
			Username:    "go-client",
			Session:     c.session,
			Date:        time.Now().Format(time.RFC3339),
			MessageType: string(MsgExecuteRequest),
			Version:     "5.3",
		},
		ParentHeader: Header{},
		Metadata:     make(map[string]interface{}),
		Content:      content,
		Channel:      "shell",
	}

	// register execution result handler
	if handler.OnExecuteResult != nil {
		c.registerHandler(MsgExecuteResult, func(msg *Message) {
			var execResult ExecuteResult
			if err := json.Unmarshal(msg.Content, &execResult); err != nil {
				return
			}

			// calls callback functions
			handler.OnExecuteResult(&execResult)
		})
	}

	// Register stream output handler
	if handler.OnStream != nil {
		c.registerHandler(MsgStream, func(msg *Message) {
			var stream StreamOutput
			if err := json.Unmarshal(msg.Content, &stream); err != nil {
				return
			}

			// calls callback functions
			handler.OnStream(&stream)
		})
	}

	// Register display data handler
	if handler.OnDisplayData != nil {
		c.registerHandler(MsgDisplayData, func(msg *Message) {
			var display DisplayData
			if err := json.Unmarshal(msg.Content, &display); err != nil {
				return
			}

			// calls callback functions
			handler.OnDisplayData(&display)
		})
	}

	// register error handler
	if handler.OnError != nil {
		c.registerHandler(MsgError, func(msg *Message) {
			var errOutput ErrorOutput
			if err := json.Unmarshal(msg.Content, &errOutput); err != nil {
				return
			}

			// calls callback functions
			handler.OnError(&errOutput)
		})
	}

	// register status handler
	if handler.OnStatus != nil {
		c.registerHandler(MsgStatus, func(msg *Message) {
			var status StatusUpdate
			if err := json.Unmarshal(msg.Content, &status); err != nil {
				return
			}

			// calls callback functions
			handler.OnStatus(&status)
		})
	}

	// send execution request
	c.mu.Lock()
	err = c.conn.WriteJSON(msg)
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to send execution request: %w", err)
	}

	return nil
}

// Register default message handlers
func (c *Client) registerDefaultHandlers() {
	// default message handlers can be registered here
}

// Register temporary message handler
func (c *Client) registerHandler(msgType MessageType, handler func(*Message)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers[msgType] = handler
}

// Clear temporary message handlers
func (c *Client) clearTemporaryHandlers() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers = make(map[MessageType]func(*Message))
	c.registerDefaultHandlers()
}

// Receive WebSocket messages
func (c *Client) receiveMessages() {
	var readErr error
	for {
		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()

		if conn == nil {
			break
		}

		// Receive message
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			// connection may already be closed
			readErr = err
			break
		}

		// Process message
		c.handleMessage(&msg)
	}

	// If the read loop exited before idle was observed, fail the in-flight
	// execution; otherwise finalizeExecution's grace period owns the close.
	if state := c.currentActiveStream(); state != nil {
		state.executeMutex.Lock()
		alreadyFinalizing := state.executeDone
		state.executeMutex.Unlock()
		if !alreadyFinalizing {
			errOutput := &ErrorOutput{
				EName:  "KernelStreamAborted",
				EValue: "kernel message stream was closed before execution completed",
			}
			if readErr != nil {
				errOutput.EValue = fmt.Sprintf("kernel message stream was closed before execution completed: %v", readErr)
			}
			state.terminate(errOutput)
			c.clearActiveStream(state)
		}
	}
}

// Handle received messages
func (c *Client) handleMessage(msg *Message) {
	// Extract message type
	msgType := MessageType(msg.Header.MessageType)

	// call the corresponding handler
	c.mu.Lock()
	handler, ok := c.handlers[msgType]
	c.mu.Unlock()

	if ok && handler != nil {
		handler(msg)
	}
}

// generate next messageID
func (c *Client) nextMessageID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgCounter++
	return fmt.Sprintf("%s-%d", c.session, c.msgCounter)
}
