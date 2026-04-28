/*
Copyright 2026 The KAITO Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// GatewayServiceName is the Kubernetes Service name created by Istio for the Gateway.
	GatewayServiceName = "inference-gateway-istio"

	// GatewayNamespace is where the Gateway and its Service are deployed.
	GatewayNamespace = "default"

	// DefaultGatewayPort is the HTTP listener port on the Gateway.
	DefaultGatewayPort = 80

	// HTTPTimeout is the default timeout for HTTP requests.
	// Set high to account for BBR/EPP ext_proc startup latency.
	HTTPTimeout = 60 * time.Second
)

// ChatCompletionRequest represents an OpenAI-compatible chat completion request body.
type ChatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Tools    []Tool        `json:"tools,omitempty"`
}

// ChatMessage represents a single message in a chat completion request.
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Tool represents a tool definition in a chat completion request.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a function that can be called by the model.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCall represents a tool call in an assistant message.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction holds the function name and arguments for a tool call.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatCompletionResponse represents the relevant fields of an OpenAI-compatible
// chat completion response.
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices,omitempty"`
}

// Choice represents a single choice in a chat completion response.
type Choice struct {
	Index        int             `json:"index"`
	Message      ResponseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

// ResponseMessage represents the message within a response choice.
type ResponseMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ErrorResponse represents an OpenAI-compatible error response.
type ErrorResponse struct {
	Error struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
	} `json:"error"`
}

// ErrorCode returns the error code as a string, handling both string
// and numeric JSON values (vLLM returns numeric, catch-all returns string).
func (e *ErrorResponse) ErrorCode() string {
	if e == nil || e.Error.Code == nil {
		return ""
	}
	// Try to unmarshal as string first.
	var s string
	if err := json.Unmarshal(e.Error.Code, &s); err == nil {
		return s
	}
	// Fall back to raw representation (e.g., "400").
	return strings.TrimSpace(string(e.Error.Code))
}

// portForwardCmd holds the kubectl port-forward process so it can be
// cleaned up when the test suite exits. Call CleanupPortForward() in
// AfterSuite.
var portForwardCmd *exec.Cmd

// cachedGatewayURL stores the gateway URL after the first successful
// discovery so subsequent calls reuse the same port-forward.
var cachedGatewayURL string

// portForwardLocalPort is the local TCP port the kubectl port-forward
// child binds to. Stored so we can restart on the same port and keep
// any cached `gatewayURL` strings the suite has already handed out
// to test specs (see e2e_test.go) valid across restarts.
var portForwardLocalPort int

// portForwardDone is closed when the kubectl port-forward process exits unexpectedly.
var portForwardDone chan struct{}

// portForwardErr stores the error from the port-forward process.
var portForwardErr error

// portForwardMu guards the port-forward state above. Concurrent specs
// (e.g. Cross-model isolation concurrent burst) call SendChatCompletion
// from many goroutines and any one of them may race a restart attempt.
var portForwardMu sync.Mutex

// CleanupPortForward kills the kubectl port-forward process if one is running.
func CleanupPortForward() {
	// Snapshot and reset state under the lock, then release it BEFORE
	// waiting on portForwardDone. The watcher goroutine in
	// startPortForward grabs portForwardMu after cmd.Wait() returns; if
	// we held the lock here while blocking on <-portForwardDone, the
	// watcher would deadlock on the mutex and never close(done).
	portForwardMu.Lock()
	cmd := portForwardCmd
	done := portForwardDone
	portForwardCmd = nil
	portForwardDone = nil
	cachedGatewayURL = ""
	portForwardLocalPort = 0
	portForwardErr = nil
	portForwardMu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		// Wait only if the background goroutine hasn't already reaped the process.
		if done != nil {
			<-done
		} else {
			_ = cmd.Wait()
		}
	}
}

// startPortForward spawns `kubectl port-forward` bound to localPort and
// installs the watcher goroutine. portForwardMu must be held.
func startPortForward(localPort int) error {
	cmd := exec.Command("kubectl", "port-forward",
		fmt.Sprintf("svc/%s", GatewayServiceName),
		fmt.Sprintf("%d:%d", localPort, DefaultGatewayPort),
		"-n", GatewayNamespace)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start kubectl port-forward: %w", err)
	}
	portForwardCmd = cmd
	portForwardDone = make(chan struct{})
	portForwardErr = nil
	done := portForwardDone

	// Monitor the process in the background so tests fail fast
	// instead of hanging on HTTP timeouts.
	go func() {
		err := cmd.Wait()
		portForwardMu.Lock()
		// Only record the error if we are still tracking THIS cmd.
		// CleanupPortForward / a concurrent restart may have replaced it.
		if portForwardCmd == cmd {
			portForwardErr = fmt.Errorf("kubectl port-forward exited unexpectedly: %w", err)
		}
		portForwardMu.Unlock()
		close(done)
	}()

	// Wait for port-forward to become ready.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return portForwardErr
		default:
		}
		conn, err := net.DialTimeout("tcp",
			fmt.Sprintf("localhost:%d", localPort), time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("kubectl port-forward to %s/%s did not become ready within 30s",
		GatewayNamespace, GatewayServiceName)
}

// restartPortForward re-spawns kubectl port-forward on the SAME local
// port the previous instance was using, so any `gatewayURL` strings the
// suite has already handed out to test specs remain valid. Caller MUST
// observe portForwardDone closed (i.e. the previous process is dead)
// before calling. portForwardMu must NOT be held.
func restartPortForward() error {
	portForwardMu.Lock()
	defer portForwardMu.Unlock()
	// Another goroutine may have already restarted while we were waiting
	// for the lock.
	if portForwardDone != nil {
		select {
		case <-portForwardDone:
			// still dead — proceed with restart
		default:
			return nil
		}
	}
	if portForwardLocalPort == 0 {
		return fmt.Errorf("port-forward died and no recorded local port to rebind to")
	}
	// Reap the dead cmd state (Wait already returned in the watcher goroutine).
	portForwardCmd = nil
	return startPortForward(portForwardLocalPort)
}

// GetGatewayURL discovers the base URL for the inference gateway.
// It checks the GATEWAY_URL env var first, then starts a kubectl port-forward
// to the gateway service and returns a localhost URL.
func GetGatewayURL() (string, error) {
	if url := os.Getenv("GATEWAY_URL"); url != "" {
		return url, nil
	}

	portForwardMu.Lock()
	if cachedGatewayURL != "" {
		url := cachedGatewayURL
		portForwardMu.Unlock()
		return url, nil
	}
	portForwardMu.Unlock()

	// Verify the gateway service exists.
	clientset, err := GetK8sClientset()
	if err != nil {
		return "", fmt.Errorf("failed to create clientset: %w", err)
	}

	_, err = clientset.CoreV1().Services(GatewayNamespace).Get(
		context.Background(), GatewayServiceName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get gateway service %s/%s: %w",
			GatewayNamespace, GatewayServiceName, err)
	}

	// Find a free local port.
	localPort, err := getFreePort()
	if err != nil {
		return "", fmt.Errorf("failed to find free port: %w", err)
	}

	portForwardMu.Lock()
	defer portForwardMu.Unlock()
	if cachedGatewayURL != "" {
		// Lost the race — another goroutine already started one.
		return cachedGatewayURL, nil
	}
	if err := startPortForward(localPort); err != nil {
		return "", err
	}
	portForwardLocalPort = localPort
	cachedGatewayURL = fmt.Sprintf("http://localhost:%d", localPort)
	return cachedGatewayURL, nil
}

// getFreePort asks the OS for an available port.
func getFreePort() (int, error) {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// checkPortForward returns nil if the port-forward is healthy. If the
// background kubectl process has died, it transparently restarts it on
// the same local port (so any previously-handed-out gatewayURL strings
// remain valid) and returns nil on success or a wrapped error on failure.
func checkPortForward() error {
	portForwardMu.Lock()
	done := portForwardDone
	portForwardMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		// Port-forward died — try to revive it.
		if err := restartPortForward(); err != nil {
			portForwardMu.Lock()
			origErr := portForwardErr
			portForwardMu.Unlock()
			return fmt.Errorf("port-forward died (%v) and restart failed: %w", origErr, err)
		}
		return nil
	default:
		return nil
	}
}

func SendChatCompletion(gatewayURL, model string) (*http.Response, error) {
	if err := checkPortForward(); err != nil {
		return nil, err
	}
	return SendChatCompletionWithPrompt(gatewayURL, model, "hello")
}

// SendChatCompletionWithRetry sends a chat completion request and retries
// on transport-level errors (EOF, connection reset, etc.) caused by
// transient kubectl port-forward connection drops. It does NOT retry on
// HTTP-level errors — any non-nil response is returned to the caller as-is
// so status-code assertions remain meaningful. The retry budget is small
// (3 attempts, 500ms backoff) because the underlying pod is healthy and
// the failure mode we're tolerating is purely the port-forward channel.
func SendChatCompletionWithRetry(gatewayURL, model string) (*http.Response, error) {
	const maxAttempts = 3
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		resp, err := SendChatCompletion(gatewayURL, model)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		// Only retry on transport errors (e.g., EOF, connection reset).
		// If the port-forward is permanently dead, checkPortForward()
		// will short-circuit on the next iteration anyway.
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// SendChatCompletionWithPrompt sends a chat completion request with a custom
// prompt message.
func SendChatCompletionWithPrompt(gatewayURL, model, prompt string) (*http.Response, error) {
	if err := checkPortForward(); err != nil {
		return nil, err
	}
	reqBody := ChatCompletionRequest{
		Model: model,
		Messages: []ChatMessage{
			{Role: "user", Content: prompt},
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	client := &http.Client{Timeout: HTTPTimeout}
	url := gatewayURL + "/v1/chat/completions"

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	return client.Do(req)
}

// SendChatCompletionRaw sends an arbitrary ChatCompletionRequest to the gateway.
func SendChatCompletionRaw(gatewayURL string, reqBody ChatCompletionRequest) (*http.Response, error) {
	if err := checkPortForward(); err != nil {
		return nil, err
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	client := &http.Client{Timeout: HTTPTimeout}
	url := gatewayURL + "/v1/chat/completions"

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	return client.Do(req)
}

// ReadResponseBody reads the full response body and closes it.
func ReadResponseBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// ParseChatCompletionResponse reads the response body and unmarshals it into
// a ChatCompletionResponse. It closes the response body.
func ParseChatCompletionResponse(resp *http.Response) (*ChatCompletionResponse, error) {
	body, err := ReadResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	var result ChatCompletionResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse response JSON: %w (body: %s)", err, string(body))
	}
	return &result, nil
}

// ParseErrorResponse reads the response body and unmarshals it into an
// ErrorResponse. It closes the response body.
func ParseErrorResponse(resp *http.Response) (*ErrorResponse, error) {
	body, err := ReadResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	var result ErrorResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse error response JSON: %w (body: %s)", err, string(body))
	}
	return &result, nil
}
