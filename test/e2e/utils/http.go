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
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// portForwardCmd holds the kubectl port-forward process so it can be
// cleaned up when the test suite exits. Call CleanupPortForward() in
// AfterSuite.
var portForwardCmd *exec.Cmd

// cachedGatewayURL stores the gateway URL after the first successful
// discovery so subsequent calls reuse the same port-forward.
var cachedGatewayURL string

// portForwardDone is closed when the kubectl port-forward process exits unexpectedly.
var portForwardDone chan struct{}

// portForwardErr stores the error from the port-forward process.
var portForwardErr error

// CleanupPortForward kills the kubectl port-forward process if one is running.
func CleanupPortForward() {
	if portForwardCmd != nil && portForwardCmd.Process != nil {
		_ = portForwardCmd.Process.Kill()
		// Wait only if the background goroutine hasn't already reaped the process.
		if portForwardDone != nil {
			<-portForwardDone
		} else {
			_ = portForwardCmd.Wait()
		}
		portForwardCmd = nil
	}
	cachedGatewayURL = ""
	portForwardDone = nil
	portForwardErr = nil
}

// GetGatewayURL discovers the base URL for the inference gateway.
// It checks the GATEWAY_URL env var first, then starts a kubectl port-forward
// to the gateway service and returns a localhost URL.
func GetGatewayURL() (string, error) {
	if url := os.Getenv("GATEWAY_URL"); url != "" {
		return url, nil
	}

	// Return cached URL if port-forward is already running.
	if cachedGatewayURL != "" {
		return cachedGatewayURL, nil
	}

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

	// Start kubectl port-forward in the background.
	// Do NOT attach stdout/stderr — Go's test runner waits for child I/O
	// to close, and port-forward never exits, causing a 3-minute hang.
	cmd := exec.Command("kubectl", "port-forward",
		fmt.Sprintf("svc/%s", GatewayServiceName),
		fmt.Sprintf("%d:%d", localPort, DefaultGatewayPort),
		"-n", GatewayNamespace)

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start kubectl port-forward: %w", err)
	}
	portForwardCmd = cmd
	portForwardDone = make(chan struct{})

	// Monitor the process in the background so tests fail fast
	// instead of hanging on HTTP timeouts.
	go func() {
		err := cmd.Wait()
		portForwardErr = fmt.Errorf("kubectl port-forward exited unexpectedly: %w", err)
		close(portForwardDone)
	}()

	// Wait for port-forward to become ready.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		// Check if port-forward already died during startup.
		select {
		case <-portForwardDone:
			cachedGatewayURL = ""
			return "", portForwardErr
		default:
		}
		conn, err := net.DialTimeout("tcp",
			fmt.Sprintf("localhost:%d", localPort), time.Second)
		if err == nil {
			conn.Close()
			cachedGatewayURL = fmt.Sprintf("http://localhost:%d", localPort)
			return cachedGatewayURL, nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Clean up if we couldn't connect.
	CleanupPortForward()
	return "", fmt.Errorf("kubectl port-forward to %s/%s did not become ready within 30s",
		GatewayNamespace, GatewayServiceName)
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

// SendChatCompletion sends an OpenAI-compatible chat completion request to the
// gateway and returns the raw HTTP response. The caller is responsible for
// closing the response body (or using one of the Parse helpers).
// checkPortForward returns an error if the port-forward process has exited.
func checkPortForward() error {
	if portForwardDone == nil {
		return nil
	}
	select {
	case <-portForwardDone:
		return portForwardErr
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
