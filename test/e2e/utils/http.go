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
	"os/exec"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
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

// IstioGatewayServiceName returns the Kubernetes Service name Istio
// creates for a Gateway with the given resource name. Istio's pattern is
// "<gateway-name>-istio".
func IstioGatewayServiceName(gatewayName string) string {
	return gatewayName + "-istio"
}

// portForward holds per-gateway port-forward state. Each (namespace,
// gatewayName) tuple owns one kubectl port-forward and one cached URL,
// so multiple gateways (the cluster-wide default plus per-case ones)
// can be exercised concurrently by parallel Ginkgo workers.
type portForward struct {
	cmd       *exec.Cmd
	done      chan struct{}
	err       error
	localPort int
	url       string
}

// portForwardKey identifies a (namespace, serviceName) pair so the same
// port-forward is reused across calls.
type portForwardKey struct {
	namespace string
	service   string
}

var (
	portForwardMu sync.Mutex
	portForwards  = make(map[portForwardKey]*portForward)
	// portForwardByURL lets Send* helpers swap a stale (pre-restart)
	// URL for the current one without callers having to re-resolve.
	// Populated at first GetGatewayURLFor() and updated on restart.
	portForwardByURL = make(map[string]portForwardKey)
)

// resolveGatewayURL maps a possibly-stale URL (cached by a test at
// BeforeAll time) to the current URL of the same gateway after any
// port-forward restarts. Returns the input unchanged if we don't own it.
func resolveGatewayURL(url string) string {
	portForwardMu.Lock()
	defer portForwardMu.Unlock()
	// Fast path: URL is current.
	if _, ok := portForwardByURL[url]; ok {
		// portForwardByURL maps both stale and current URLs to the
		// same key; resolve via the key to the canonical pf.url.
		key := portForwardByURL[url]
		if pf, ok := portForwards[key]; ok && pf.url != "" {
			return pf.url
		}
	}
	return url
}

// ResolveGatewayURL is the exported form of resolveGatewayURL for use by
// test sites that build their own *http.Request and bypass the
// SendChatCompletion* helpers. Combine with EnsurePortForwards() before
// every raw HTTP call so a port-forward that died between specs is
// rebound and the URL is refreshed in lock-step.
func ResolveGatewayURL(url string) string {
	return resolveGatewayURL(url)
}

// EnsurePortForwards verifies every registered kubectl port-forward is
// alive and restarts any that died (transparently re-allocating a fresh
// local port and updating the cached URL). Test sites that construct
// requests via http.NewRequest + http.Client.Do should call this — and
// then re-resolve the gateway URL via ResolveGatewayURL — before each
// network call. SendChatCompletion* already does this internally.
func EnsurePortForwards() error {
	return checkAllPortForwards()
}

// RemovePortForwardsForNamespace kills and unregisters every kubectl
// port-forward whose target service lives in the given namespace.
// Call this from teardown paths that delete a namespace, so subsequent
// EnsurePortForwards / SendChatCompletion calls do not try to restart
// a forward against a namespace that no longer exists (which surfaces as
// `Error from server (NotFound): namespaces "<ns>" not found` and
// eventually a 90s readiness timeout).
func RemovePortForwardsForNamespace(namespace string) {
	portForwardMu.Lock()
	victims := make([]*portForward, 0)
	for k, pf := range portForwards {
		if k.namespace != namespace {
			continue
		}
		victims = append(victims, pf)
		delete(portForwards, k)
		if pf.url != "" {
			delete(portForwardByURL, pf.url)
		}
	}
	portForwardMu.Unlock()

	for _, pf := range victims {
		if pf.cmd != nil && pf.cmd.Process != nil {
			_ = pf.cmd.Process.Kill()
			if pf.done != nil {
				<-pf.done
			} else {
				_ = pf.cmd.Wait()
			}
		}
	}
}

// CleanupPortForward kills every kubectl port-forward process started by
// the suite. Safe to call from AfterSuite even if no forwards were ever
// started.
func CleanupPortForward() {
	portForwardMu.Lock()
	pfs := portForwards
	portForwards = make(map[portForwardKey]*portForward)
	portForwardMu.Unlock()

	for _, pf := range pfs {
		if pf.cmd != nil && pf.cmd.Process != nil {
			_ = pf.cmd.Process.Kill()
			if pf.done != nil {
				<-pf.done
			} else {
				_ = pf.cmd.Wait()
			}
		}
	}
}

// startPortForward spawns `kubectl port-forward` for the given service in
// namespace, bound to localPort. portForwardMu must be held by the caller.
func startPortForward(pf *portForward, namespace, serviceName string) error {
	cmd := exec.Command("kubectl", "port-forward",
		fmt.Sprintf("svc/%s", serviceName),
		fmt.Sprintf("%d:%d", pf.localPort, DefaultGatewayPort),
		"-n", namespace)

	// Capture stdout/stderr so a 30s readiness timeout (or an unexpected
	// exit) can surface kubectl's actual error message instead of failing
	// silently. Without this, transient bind/RBAC/API-server errors look
	// identical to "took too long" and are nearly impossible to debug.
	var pfOut bytes.Buffer
	cmd.Stdout = &pfOut
	cmd.Stderr = &pfOut

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start kubectl port-forward: %w", err)
	}
	pf.cmd = cmd
	pf.done = make(chan struct{})
	pf.err = nil
	done := pf.done

	// Monitor the process so tests fail fast instead of hanging on
	// HTTP timeouts when the forward dies.
	go func() {
		err := cmd.Wait()
		portForwardMu.Lock()
		if pf.cmd == cmd {
			pf.err = fmt.Errorf("kubectl port-forward exited unexpectedly: %w\nkubectl output:\n%s",
				err, pfOut.String())
		}
		portForwardMu.Unlock()
		close(done)
	}()

	// Wait for port-forward to become ready. The deadline is generous
	// (90s) because kubectl port-forward setup latency is dominated by
	// the API server's spdy/exec channel setup, which can take 20-30s
	// on a busy or slow control plane (and the previous 30s budget
	// raced right at that boundary, producing identical-looking
	// "did not become ready" failures).
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return pf.err
		default:
		}
		conn, err := net.DialTimeout("tcp",
			fmt.Sprintf("localhost:%d", pf.localPort), time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("kubectl port-forward to %s/%s did not become ready within 90s\nkubectl output:\n%s",
		namespace, serviceName, pfOut.String())
}

// restartPortForward re-spawns kubectl port-forward on a FRESH local
// port. Previously this rebound the same port, but on long e2e runs the
// kernel often still held the prior socket in TIME_WAIT (or `kubectl`
// hadn't fully released the listener), so the 30s readiness wait would
// expire and cascade-fail every spec sharing this forward.
//
// Allocating a new port avoids the bind race entirely. The pf.url field
// is updated atomically so subsequent calls to GetGatewayURLFor() (which
// every Send* helper makes via checkAllPortForwards → cached lookup)
// pick up the new URL on the next iteration. Test code that caches the
// URL once at BeforeAll time will see the stale URL fail with a connect
// error and should re-resolve via GetGatewayURLFor — most Eventually
// loops in the suite already do this implicitly because they call the
// Send* helpers fresh each iteration. portForwardMu must NOT be held.
func restartPortForward(key portForwardKey) error {
	portForwardMu.Lock()
	defer portForwardMu.Unlock()
	pf, ok := portForwards[key]
	if !ok {
		return fmt.Errorf("no port-forward registered for %s/%s", key.namespace, key.service)
	}
	// Another goroutine may have already restarted it.
	if pf.done != nil {
		select {
		case <-pf.done:
			// still dead — proceed
		default:
			return nil
		}
	}
	newPort, err := getFreePort()
	if err != nil {
		return fmt.Errorf("failed to allocate fresh port for restart: %w", err)
	}
	pf.localPort = newPort
	pf.cmd = nil
	if err := startPortForward(pf, key.namespace, key.service); err != nil {
		return err
	}
	newURL := fmt.Sprintf("http://localhost:%d", newPort)
	// Keep the old URL pointing at this key so resolveGatewayURL can
	// translate stale BeforeAll-cached URLs to the live one.
	portForwardByURL[newURL] = key
	pf.url = newURL
	return nil
}

// GetGatewayURLFor returns a base URL that proxies HTTP traffic to the
// Gateway named gatewayName in the given namespace. The first call for a
// (namespace, gatewayName) tuple starts a kubectl port-forward; later
// calls reuse the same forward.
func GetGatewayURLFor(namespace, gatewayName string) (string, error) {
	serviceName := IstioGatewayServiceName(gatewayName)
	key := portForwardKey{namespace: namespace, service: serviceName}

	portForwardMu.Lock()
	if pf, ok := portForwards[key]; ok && pf.url != "" {
		url := pf.url
		portForwardMu.Unlock()
		return url, nil
	}
	portForwardMu.Unlock()

	// Verify the gateway service exists before starting a port-forward.
	clientset, err := GetK8sClientset()
	if err != nil {
		return "", fmt.Errorf("failed to create clientset: %w", err)
	}

	if _, err := clientset.CoreV1().Services(namespace).Get(
		context.Background(), serviceName, metav1.GetOptions{}); err != nil {
		return "", fmt.Errorf("failed to get gateway service %s/%s: %w",
			namespace, serviceName, err)
	}

	localPort, err := getFreePort()
	if err != nil {
		return "", fmt.Errorf("failed to find free port: %w", err)
	}

	portForwardMu.Lock()
	defer portForwardMu.Unlock()
	if pf, ok := portForwards[key]; ok && pf.url != "" {
		// Lost the race — another goroutine already started one.
		return pf.url, nil
	}
	pf := &portForward{localPort: localPort}
	if err := startPortForward(pf, namespace, serviceName); err != nil {
		return "", err
	}
	pf.url = fmt.Sprintf("http://localhost:%d", localPort)
	portForwards[key] = pf
	portForwardByURL[pf.url] = key
	return pf.url, nil
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

// checkAllPortForwards returns nil if every registered port-forward is
// healthy. Dead forwards are restarted transparently on their original
// local port so previously-handed-out URLs remain valid.
func checkAllPortForwards() error {
	portForwardMu.Lock()
	keys := make([]portForwardKey, 0, len(portForwards))
	for k := range portForwards {
		keys = append(keys, k)
	}
	portForwardMu.Unlock()

	for _, key := range keys {
		portForwardMu.Lock()
		pf := portForwards[key]
		done := pf.done
		portForwardMu.Unlock()
		if done == nil {
			continue
		}
		select {
		case <-done:
			if err := restartPortForward(key); err != nil {
				portForwardMu.Lock()
				origErr := pf.err
				portForwardMu.Unlock()
				return fmt.Errorf("port-forward %s/%s died (%v) and restart failed: %w",
					key.namespace, key.service, origErr, err)
			}
		default:
		}
	}
	return nil
}

func SendChatCompletion(gatewayURL, model string) (*http.Response, error) {
	if err := checkAllPortForwards(); err != nil {
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
		// If the port-forward is permanently dead, checkAllPortForwards()
		// will short-circuit on the next iteration anyway.
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// SendChatCompletionWithPrompt sends a chat completion request with a custom
// prompt message.
func SendChatCompletionWithPrompt(gatewayURL, model, prompt string) (*http.Response, error) {
	if err := checkAllPortForwards(); err != nil {
		return nil, err
	}
	gatewayURL = resolveGatewayURL(gatewayURL)
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
	if err := checkAllPortForwards(); err != nil {
		return nil, err
	}
	gatewayURL = resolveGatewayURL(gatewayURL)
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

// SendChatCompletionWithAuth sends an OpenAI-compatible chat completion request
// with an Authorization Bearer token and a custom Host header (needed for
// namespace resolution by the apikey-authz service).
func SendChatCompletionWithAuth(gatewayURL, model, prompt, bearerToken, hostHeader string) (*http.Response, error) {
	if err := checkAllPortForwards(); err != nil {
		return nil, err
	}
	gatewayURL = resolveGatewayURL(gatewayURL)
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
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	if hostHeader != "" {
		req.Host = hostHeader
	}

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
