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
	"fmt"
	"net/http"

	"github.com/kaito-project/production-stack/test/e2e/scenarios"
)

// ChatClientHelm implements scenarios.ChatClient on top of this
// package's existing gateway port-forward + HTTP helpers
// (SendChatCompletion*), so scenario code sends traffic through the same
// wire path (port-forwarded kubectl tunnel to the Istio Gateway Service)
// every other Helm-suite spec already uses.
type ChatClientHelm struct{}

var _ scenarios.ChatClient = ChatClientHelm{}

// SendChat implements scenarios.ChatClient.
func (ChatClientHelm) SendChat(_ context.Context, gatewayURL string, req scenarios.ChatRequest) (*scenarios.ChatResponse, error) {
	var (
		resp *http.Response
		err  error
	)

	switch {
	case req.RawBody != nil:
		resp, err = sendRawChatRequest(gatewayURL, req)
	case req.BearerToken != "" || req.HostHeader != "":
		resp, err = SendChatCompletionWithAuth(gatewayURL, req.Model, promptOrDefault(req.Prompt), req.BearerToken, req.HostHeader)
	default:
		resp, err = SendChatCompletionWithPrompt(gatewayURL, req.Model, promptOrDefault(req.Prompt))
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ReadResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	return &scenarios.ChatResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       body,
	}, nil
}

func promptOrDefault(prompt string) string {
	if prompt == "" {
		return "hello"
	}
	return prompt
}

// sendRawChatRequest posts req.RawBody verbatim, optionally attaching a
// bearer token and/or a Host header override — used by assertions that
// need a malformed or non-standard payload (missing `model` field,
// non-JSON content, ...).
func sendRawChatRequest(gatewayURL string, req scenarios.ChatRequest) (*http.Response, error) {
	if err := EnsurePortForwards(); err != nil {
		return nil, err
	}
	contentType := req.ContentType
	if contentType == "" {
		contentType = "application/json"
	}

	url := ResolveGatewayURL(gatewayURL) + "/v1/chat/completions"
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(req.RawBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", contentType)
	if req.BearerToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.BearerToken)
	}
	if req.HostHeader != "" {
		httpReq.Host = req.HostHeader
	}
	client := &http.Client{Timeout: HTTPTimeout}
	return client.Do(httpReq)
}
