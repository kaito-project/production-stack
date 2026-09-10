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
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
)

const (
	// HTTPTimeout is the default timeout for HTTP requests.
	// Set high to account for BBR/EPP ext_proc startup latency.
	HTTPTimeout = 60 * time.Second

	// ChatCompletionsPath is the inference endpoint, relative to the API root
	// a deploy.GatewayEndpoint hands out.
	ChatCompletionsPath = "/chat/completions"
)

// ChatCompletionRequest represents an OpenAI-compatible chat completion request body.
type ChatCompletionRequest struct {
	Model     string        `json:"model"`
	Messages  []ChatMessage `json:"messages"`
	Tools     []Tool        `json:"tools,omitempty"`
	MaxTokens int           `json:"max_tokens,omitempty"`
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

// RequestOption customises a request sent through the gateway helpers.
type RequestOption func(*gatewayRequest)

type gatewayRequest struct {
	prompt         string
	method         string
	headers        []deploy.AuthHeader
	authHeaders    []deploy.AuthHeader
	explicitAuth   bool
	withoutAuth    bool
	transportRetry bool
	originPath     bool
	timeout        time.Duration
	buildBody      func(*gatewayRequest) ([]byte, error)
}

// WithOriginPath resolves the path against the gateway origin instead of its API root.
func WithOriginPath() RequestOption {
	return func(r *gatewayRequest) { r.originPath = true }
}

// WithTimeout overrides the HTTP client timeout for each request attempt.
func WithTimeout(timeout time.Duration) RequestOption {
	return func(r *gatewayRequest) { r.timeout = timeout }
}

// WithPrompt overrides the "hello" the chat helpers send by default.
func WithPrompt(prompt string) RequestOption {
	return func(r *gatewayRequest) { r.prompt = prompt }
}

// WithHeader adds one request header. Repeatable.
func WithHeader(name, value string) RequestOption {
	return func(r *gatewayRequest) {
		r.headers = append(r.headers, deploy.AuthHeader{Name: name, Value: value})
	}
}

// WithAuth replaces automatic authentication with the supplied nonempty headers.
func WithAuth(headers ...deploy.AuthHeader) RequestOption {
	return func(r *gatewayRequest) {
		r.explicitAuth = true
		r.withoutAuth = false
		r.authHeaders = headers
	}
}

// WithoutAuth explicitly disables automatic authentication for a negative test.
func WithoutAuth() RequestOption {
	return func(r *gatewayRequest) {
		r.explicitAuth = true
		r.withoutAuth = true
		r.authHeaders = nil
	}
}

// WithMethod overrides POST, for the specs that assert non-GET verbs are
// rejected on the discovery endpoints.
func WithMethod(method string) RequestOption {
	return func(r *gatewayRequest) { r.method = method }
}

// WithTransportRetry rebuilds the endpoint's transport and retries when the
// request fails below HTTP — a tunnelled gateway drops connections on its own
// schedule. Up to 3 attempts 500ms apart. HTTP-level errors are never retried:
// any response is returned as-is so status-code assertions stay meaningful.
func WithTransportRetry() RequestOption {
	return func(r *gatewayRequest) { r.transportRetry = true }
}

func newGatewayRequest(method string, opts []RequestOption) *gatewayRequest {
	r := &gatewayRequest{prompt: "hello", method: method, timeout: HTTPTimeout}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// SendChat posts an OpenAI-compatible chat completion for model to the gateway.
func SendChat(gateway deploy.GatewayEndpoint, model string, opts ...RequestOption) (*http.Response, error) {
	return SendChatContext(context.Background(), gateway, model, opts...)
}

// SendChatContext sends a chat completion with caller-controlled cancellation.
func SendChatContext(ctx context.Context, gateway deploy.GatewayEndpoint, model string, opts ...RequestOption) (*http.Response, error) {
	chatBody := func(request *gatewayRequest) {
		request.buildBody = func(settings *gatewayRequest) ([]byte, error) {
			return json.Marshal(ChatCompletionRequest{
				Model:    model,
				Messages: []ChatMessage{{Role: "user", Content: settings.prompt}},
			})
		}
	}
	return SendGatewayRequest(ctx, gateway, http.MethodPost, ChatCompletionsPath, nil,
		append([]RequestOption{chatBody}, opts...)...)
}

// SendModels calls one of the OpenAI model-discovery endpoints (ModelsPath or
// ModelRetrievePath).
func SendModels(gateway deploy.GatewayEndpoint, path string, opts ...RequestOption) (*http.Response, error) {
	return SendModelsContext(context.Background(), gateway, path, opts...)
}

// SendModelsContext sends a model-discovery request with caller-controlled cancellation.
func SendModelsContext(ctx context.Context, gateway deploy.GatewayEndpoint, path string, opts ...RequestOption) (*http.Response, error) {
	return SendGatewayRequest(ctx, gateway, http.MethodGet, path, nil, opts...)
}

// CheckChatSuccess performs one probe, retaining response details on failure.
func CheckChatSuccess(ctx context.Context, gateway deploy.GatewayEndpoint, model string, opts ...RequestOption) error {
	response, err := SendChatContext(ctx, gateway, model, opts...)
	if err != nil {
		return fmt.Errorf("chat probe for %s: %w", model, err)
	}
	body, err := ReadResponseBody(response)
	if err != nil {
		return fmt.Errorf("read chat probe for %s (status=%d): %w", model, response.StatusCode, err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("chat probe for %s (host=%q): expected 200, got %d: %s", model, gateway.Host(), response.StatusCode, body)
	}
	return nil
}

// SendGatewayRequest sends an authenticated request, preserving the supplied body.
// Paths are relative to the API root unless WithOriginPath is specified.
func SendGatewayRequest(ctx context.Context, gateway deploy.GatewayEndpoint, method, path string, body []byte, opts ...RequestOption) (*http.Response, error) {
	request := newGatewayRequest(method, opts)
	if request.buildBody != nil {
		var err error
		body, err = request.buildBody(request)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request: %w", err)
		}
	}
	return request.do(ctx, gateway, path, body)
}

func (r *gatewayRequest) do(ctx context.Context, gateway deploy.GatewayEndpoint, path string, body []byte) (*http.Response, error) {
	attempts := 1
	if r.transportRetry {
		attempts = 3
	}

	var lastErr error
	client := &http.Client{Timeout: r.timeout}
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := r.request(ctx, gateway, path, body)
		if err != nil {
			return nil, err
		}

		resp, err := client.Do(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if i < attempts-1 {
			// The transport may be wedged while its process is still
			// alive, so retrying against it alone would fail identically.
			gateway.Reset()
			timer := time.NewTimer(500 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
	if attempts > 1 {
		return nil, fmt.Errorf("after %d attempts: %w", attempts, lastErr)
	}
	return nil, lastErr
}

func (r *gatewayRequest) request(ctx context.Context, gateway deploy.GatewayEndpoint, path string, body []byte) (*http.Request, error) {
	base, err := gateway.BaseURL()
	if err != nil {
		return nil, err
	}
	if r.originPath {
		parsed, err := url.Parse(base)
		if err != nil {
			return nil, fmt.Errorf("parse gateway URL %q: %w", base, err)
		}
		base = parsed.Scheme + "://" + parsed.Host
	}
	return newGatewayHTTPRequest(ctx, gateway, base+path, body, r)
}

func newGatewayHTTPRequest(ctx context.Context, gateway deploy.GatewayEndpoint, target string, body []byte, settings *gatewayRequest) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, settings.method, target, reader)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if host := gateway.Host(); host != "" {
		req.Host = host
	}
	headers := settings.authHeaders
	if settings.explicitAuth && !settings.withoutAuth && len(headers) == 0 {
		return nil, fmt.Errorf("WithAuth requires credentials; use WithoutAuth for unauthenticated requests")
	}
	if !settings.explicitAuth {
		provider, ok := gateway.(interface {
			authHeaders(context.Context) ([]deploy.AuthHeader, error)
		})
		if !ok {
			return nil, fmt.Errorf("gateway has no authentication context: use OpenGateway, WithAuth, or WithoutAuth")
		}
		headers, err = provider.authHeaders(ctx)
		if err != nil {
			return nil, err
		}
		if len(headers) == 0 {
			return nil, fmt.Errorf("gateway has no authentication credentials")
		}
		headers = headers[:1]
	}
	for _, header := range headers {
		if header.Name == "" || header.Value == "" {
			return nil, fmt.Errorf("gateway authentication header must have a name and value")
		}
		req.Header.Set(header.Name, header.Value)
	}
	for _, header := range settings.headers {
		req.Header.Set(header.Name, header.Value)
	}
	return req, nil
}

// GatewayOrigin returns the gateway's scheme and authority, without the API
// root path. Specs that probe paths outside the OpenAI API — a health endpoint,
// a deliberately unroutable path — build on this.
func GatewayOrigin(gateway deploy.GatewayEndpoint) (string, error) {
	base, err := gateway.BaseURL()
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse gateway URL %q: %w", base, err)
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// ReadResponseBody reads the full response body and closes it.
func ReadResponseBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// ParseChatCompletionResponse reads the response body and unmarshals it into
// a ChatCompletionResponse. It closes the response body.
func ParseChatCompletionResponse(resp *http.Response) (*ChatCompletionResponse, error) {
	return parseJSONResponse[ChatCompletionResponse](resp, "response")
}

// ParseErrorResponse reads the response body and unmarshals it into an
// ErrorResponse. It closes the response body.
func ParseErrorResponse(resp *http.Response) (*ErrorResponse, error) {
	return parseJSONResponse[ErrorResponse](resp, "error response")
}

// ModelsPath is the OpenAI-compatible model listing endpoint served per
// workload namespace by the productionstack-status-reporter (routed there by
// the Gateway route that charts/modelharness renders).
const ModelsPath = "/models"

// Model is one entry of an OpenAI-compatible model listing. ID is the value
// clients send in the `model` field of an inference request.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelList is the `GET /v1/models` response envelope.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// IDs returns the model ids in the listing, for order-sensitive assertions.
func (l *ModelList) IDs() []string {
	ids := make([]string, 0, len(l.Data))
	for _, m := range l.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

// ModelRetrievePath builds the single-model endpoint path for a model id.
func ModelRetrievePath(id string) string {
	return ModelsPath + "/" + id
}

// ParseModelList reads the response body and unmarshals it into a ModelList.
// It closes the response body.
func ParseModelList(resp *http.Response) (*ModelList, error) {
	return parseJSONResponse[ModelList](resp, "model list")
}

// ParseModel reads the response body and unmarshals it into a single Model.
// It closes the response body.
func ParseModel(resp *http.Response) (*Model, error) {
	return parseJSONResponse[Model](resp, "model")
}

func parseJSONResponse[Response any](resp *http.Response, description string) (*Response, error) {
	body, err := ReadResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	var result Response
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse %s JSON: %w (body: %s)", description, err, string(body))
	}
	return &result, nil
}
