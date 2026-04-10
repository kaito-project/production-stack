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
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// GatewayServiceName is the Kubernetes Service name created by Istio for the Gateway.
	// Istio names it "<gateway-name>-<gatewayclass>".
	GatewayServiceName = "inference-gateway-istio"

	// GatewayNamespace is where the Gateway and its Service are deployed.
	GatewayNamespace = "default"

	// DefaultGatewayPort is the HTTP listener port on the Gateway.
	DefaultGatewayPort = 80

	// HTTPTimeout is the default timeout for HTTP requests.
	HTTPTimeout = 30 * time.Second
)

// ChatCompletionRequest represents an OpenAI-compatible chat completion request body.
type ChatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
}

// ChatMessage represents a single message in a chat completion request.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatCompletionResponse represents the relevant fields of an OpenAI-compatible
// chat completion response.
type ChatCompletionResponse struct {
	ID     string `json:"id"`
	Object string `json:"object"`
	Model  string `json:"model"`
}

// ErrorResponse represents an OpenAI-compatible error response.
type ErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// GetGatewayURL discovers the base URL for the inference gateway.
// It checks the GATEWAY_URL env var first, then falls back to looking up
// the Kubernetes Service.
func GetGatewayURL() (string, error) {
	if url := os.Getenv("GATEWAY_URL"); url != "" {
		return url, nil
	}

	clientset, err := GetK8sClientset()
	if err != nil {
		return "", fmt.Errorf("failed to create clientset: %w", err)
	}

	svc, err := clientset.CoreV1().Services(GatewayNamespace).Get(
		context.Background(), GatewayServiceName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get gateway service %s/%s: %w",
			GatewayNamespace, GatewayServiceName, err)
	}

	// Try LoadBalancer external address first.
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			return fmt.Sprintf("http://%s:%d", ingress.IP, DefaultGatewayPort), nil
		}
		if ingress.Hostname != "" {
			return fmt.Sprintf("http://%s:%d", ingress.Hostname, DefaultGatewayPort), nil
		}
	}

	// Fall back to ClusterIP.
	if svc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != "None" {
		return fmt.Sprintf("http://%s:%d", svc.Spec.ClusterIP, DefaultGatewayPort), nil
	}

	return "", fmt.Errorf("no reachable address found for gateway service %s/%s",
		GatewayNamespace, GatewayServiceName)
}

// SendChatCompletion sends an OpenAI-compatible chat completion request to the
// gateway and returns the raw HTTP response. The caller is responsible for
// closing the response body (or using one of the Parse helpers).
func SendChatCompletion(gatewayURL, model string) (*http.Response, error) {
	return SendChatCompletionWithPrompt(gatewayURL, model, "hello")
}

// SendChatCompletionWithPrompt sends a chat completion request with a custom
// prompt message.
func SendChatCompletionWithPrompt(gatewayURL, model, prompt string) (*http.Response, error) {
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
