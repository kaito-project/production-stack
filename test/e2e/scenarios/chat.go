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

package scenarios

import (
	"context"
	"net/http"
)

// ChatRequest describes one OpenAI-compatible chat-completion request a
// scenario sends through a case's Gateway.
type ChatRequest struct {
	// Model is the value of the request's `model` field (matched by the
	// gateway's HTTPRoute as X-Gateway-Model-Name).
	Model string
	// Prompt is the single user message content. Ignored when RawBody is
	// set.
	Prompt string
	// BearerToken, when non-empty, is carried as an HTTP Authorization
	// header of the form "Bearer <token>". Leave empty to omit the
	// header entirely (some auth assertions rely on the header being
	// absent, not merely blank).
	BearerToken string
	// HostHeader, when non-empty, overrides the request's Host header —
	// needed so the apikey-authz service resolves the correct namespace
	// (subdomain = namespace).
	HostHeader string
	// RawBody, when non-nil, is sent verbatim instead of a marshalled
	// {model, messages} body. Used by assertions that need a malformed
	// or non-standard payload (missing `model` field, non-JSON content,
	// ...).
	RawBody []byte
	// ContentType overrides the request's Content-Type header. Defaults
	// to "application/json" when empty and RawBody is set; ignored
	// (always "application/json") when RawBody is nil.
	ContentType string
}

// ChatResponse is the minimal response shape scenario assertions need.
type ChatResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// ChatClient sends inference requests to a resolved gateway endpoint.
// Implementations: an http.Client-backed adapter reused by every
// Lifecycle mode (the wire protocol — OpenAI-compatible chat completions
// over the case Gateway — is the same regardless of how the namespace was
// provisioned), and a fake for unit tests.
type ChatClient interface {
	SendChat(ctx context.Context, gatewayURL string, req ChatRequest) (*ChatResponse, error)
}
