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
	"errors"
	"testing"
	"time"
)

func TestAssertGatewayReachable_RetriesUntilSuccess(t *testing.T) {
	chat := &fakeChatClient{
		rules: []fakeChatRule{
			{err: errors.New("connection refused")},
			{status: 503, body: "warming up"},
			{status: 200, body: "ok"},
		},
	}
	state := GPUSmokeState{
		cfg: GPUSmokeConfig{
			Chat:                chat,
			GatewayTimeout:      time.Second, // bounded, fast unit test
			GatewayPollInterval: time.Millisecond,
		},
		namespace: "e2e-gpu-smoke",
		modelName: "gpu-smoke-gemma",
		access:    NamespaceAccess{GatewayURL: "http://fake"},
	}

	if err := AssertGatewayReachable(context.Background(), state); err != nil {
		t.Fatalf("AssertGatewayReachable() = %v, want nil after 3rd attempt succeeds", err)
	}
	if got := chat.requestCount(); got != 3 {
		t.Errorf("expected exactly 3 requests (internal retry, not whole-case retry), got %d", got)
	}
}

func TestAssertGatewayReachable_FailsAfterTimeout(t *testing.T) {
	chat := &fakeChatClient{def: fakeChatRule{status: 500, body: "still down"}}
	state := GPUSmokeState{
		cfg: GPUSmokeConfig{
			Chat:                chat,
			GatewayTimeout:      20 * time.Millisecond,
			GatewayPollInterval: 5 * time.Millisecond,
		},
		namespace: "e2e-gpu-smoke",
		modelName: "gpu-smoke-gemma",
		access:    NamespaceAccess{GatewayURL: "http://fake"},
	}

	err := AssertGatewayReachable(context.Background(), state)
	if err == nil {
		t.Fatal("expected AssertGatewayReachable to fail once the timeout elapses")
	}
}

// TestAssertGatewayReachable_UsesLifecycleCredentialsWhenPresent guards
// against the (previously real) bug where an AIManager-mode Lifecycle
// that always returns an API key + HostHeader in NamespaceAccess would
// still see its GPU-smoke traffic sent with no Authorization header —
// guaranteed 401s against an always-auth backend.
func TestAssertGatewayReachable_UsesLifecycleCredentialsWhenPresent(t *testing.T) {
	chat := &fakeChatClient{def: fakeChatRule{status: 200}}
	state := GPUSmokeState{
		cfg: GPUSmokeConfig{
			Chat:                chat,
			GatewayTimeout:      time.Second,
			GatewayPollInterval: time.Millisecond,
		},
		namespace: "e2e-gpu-smoke",
		modelName: "gpu-smoke-gemma",
		access: NamespaceAccess{
			GatewayURL:  "http://fake",
			APIKey:      "sk-aimanager-key",
			AuthEnabled: true,
			HostHeader:  "e2e-gpu-smoke.gw.example.com",
		},
	}

	if err := AssertGatewayReachable(context.Background(), state); err != nil {
		t.Fatalf("AssertGatewayReachable() = %v, want nil", err)
	}

	req := chat.lastRequest()
	if req.BearerToken != "sk-aimanager-key" {
		t.Errorf("request BearerToken = %q, want the lifecycle-provided API key", req.BearerToken)
	}
	if req.HostHeader != "e2e-gpu-smoke.gw.example.com" {
		t.Errorf("request HostHeader = %q, want the lifecycle-provided Host header", req.HostHeader)
	}
}

// TestAssertGatewayReachable_OmitsCredentialsWhenAbsent asserts the
// converse: a Helm-mode (non-auth) NamespaceAccess with no APIKey/
// HostHeader must not have either field synthesized — the request stays
// exactly as unauthenticated as before this fix.
func TestAssertGatewayReachable_OmitsCredentialsWhenAbsent(t *testing.T) {
	chat := &fakeChatClient{def: fakeChatRule{status: 200}}
	state := GPUSmokeState{
		cfg: GPUSmokeConfig{
			Chat:                chat,
			GatewayTimeout:      time.Second,
			GatewayPollInterval: time.Millisecond,
		},
		namespace: "e2e-gpu-smoke",
		modelName: "gpu-smoke-gemma",
		access:    NamespaceAccess{GatewayURL: "http://fake"}, // no APIKey/HostHeader
	}

	if err := AssertGatewayReachable(context.Background(), state); err != nil {
		t.Fatalf("AssertGatewayReachable() = %v, want nil", err)
	}

	req := chat.lastRequest()
	if req.BearerToken != "" {
		t.Errorf("request BearerToken = %q, want empty for a non-auth Helm namespace", req.BearerToken)
	}
	if req.HostHeader != "" {
		t.Errorf("request HostHeader = %q, want empty for a non-auth Helm namespace", req.HostHeader)
	}
}

func TestAPIKeyAuthAssertions_FullSequence(t *testing.T) {
	chat := &fakeChatClient{
		rules: []fakeChatRule{
			{status: 401}, // AssertRejectsMissingAuth
			{status: 401}, // AssertRejectsInvalidAPIKey
			{status: 200}, // AssertAcceptsValidAPIKey
		},
	}
	state := APIKeyAuthState{
		cfg: APIKeyAuthConfig{
			Chat:                    chat,
			MissingAuthTimeout:      time.Second,
			MissingAuthPollInterval: time.Millisecond,
			ValidKeyTimeout:         time.Second,
			ValidKeyPollInterval:    time.Millisecond,
		},
		namespace:  "e2e-auth",
		modelName:  "auth-gemma",
		hostHeader: "e2e-auth.gw.example.com",
		access:     NamespaceAccess{GatewayURL: "http://fake", APIKey: "sk-real-key", AuthEnabled: true},
	}

	for _, a := range APIKeyAuthAssertions(state) {
		if err := a.Run(context.Background()); err != nil {
			t.Fatalf("assertion %q failed: %v", a.Name, err)
		}
	}

	// The "invalid key" request must carry the bogus token, not the real
	// one, and the "valid key" request must carry the real API key.
	reqs := chat.requests
	if reqs[0].BearerToken != "" {
		t.Errorf("missing-auth request must omit the bearer token, got %q", reqs[0].BearerToken)
	}
	if reqs[1].BearerToken != "invalid-key-12345" {
		t.Errorf("invalid-key request bearer = %q, want the canned invalid token", reqs[1].BearerToken)
	}
	if reqs[2].BearerToken != "sk-real-key" {
		t.Errorf("valid-key request bearer = %q, want the namespace's real API key", reqs[2].BearerToken)
	}
	for _, r := range reqs {
		if r.HostHeader != state.hostHeader {
			t.Errorf("request Host header = %q, want %q", r.HostHeader, state.hostHeader)
		}
	}
}

func TestAssertRejectsInvalidAPIKey_FailsWhen200(t *testing.T) {
	chat := &fakeChatClient{def: fakeChatRule{status: 200}}
	state := APIKeyAuthState{
		cfg:       APIKeyAuthConfig{Chat: chat},
		namespace: "e2e-auth",
		modelName: "auth-gemma",
		access:    NamespaceAccess{GatewayURL: "http://fake"},
	}

	if err := AssertRejectsInvalidAPIKey(context.Background(), state); err == nil {
		t.Fatal("expected failure when the gateway wrongly accepts an invalid key")
	}
}
