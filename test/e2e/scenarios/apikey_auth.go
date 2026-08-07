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
	"fmt"
	"net/http"
	"time"
)

// APIKeyAuthConfig configures one run of the API-key authentication
// group: reject-missing-auth, reject-invalid-key, accept-valid-key.
type APIKeyAuthConfig struct {
	Suffix     Suffix
	Namespace  string
	Deployment DeploymentSpec // AuthAPIKeyEnabled is forced true by Setup.

	Lifecycle Lifecycle
	Chat      ChatClient
	Logger    Logger

	// MissingAuthTimeout/MissingAuthPollInterval bound
	// AssertRejectsMissingAuth's retry loop. Default 2m / 5s.
	MissingAuthTimeout      time.Duration
	MissingAuthPollInterval time.Duration
	// ValidKeyTimeout/ValidKeyPollInterval bound
	// AssertAcceptsValidAPIKey's retry loop. Default 2m / 5s.
	ValidKeyTimeout      time.Duration
	ValidKeyPollInterval time.Duration
}

// APIKeyAuthState is the resolved state produced by APIKeyAuthSetup,
// threaded into every API-key-auth assertion.
type APIKeyAuthState struct {
	cfg        APIKeyAuthConfig
	namespace  string
	modelName  string
	hostHeader string
	access     NamespaceAccess
}

// Resources returns the physical resources this run owns, for Cleanup /
// Diagnostics.
func (s APIKeyAuthState) Resources() GroupResources {
	return GroupResources{
		Namespaces: []string{s.namespace},
		Deployments: []DeploymentSpec{
			{Namespace: s.namespace, Name: s.modelName},
		},
	}
}

// APIKeyAuthSetup provisions the group's namespace and deployment with
// API-key auth enabled and resolves the API key issued for the
// namespace. AuthAPIKeyEnabled is forced true regardless of what the
// caller set on cfg.Deployment: this group only exists to exercise auth.
//
// The returned APIKeyAuthState always carries cfg plus the resolved
// physical namespace/deployment identities — even when an error is
// returned — so a caller can still call state.Resources() (and thus
// Cleanup / Diagnostics) to locate whatever was partially created before
// the failure.
func APIKeyAuthSetup(ctx context.Context, cfg APIKeyAuthConfig) (APIKeyAuthState, error) {
	namespace := cfg.Suffix.Namespace(cfg.Namespace)
	deployment := cfg.Deployment
	deployment.Namespace = namespace
	deployment.Name = cfg.Suffix.Deployment(deployment.Name)
	deployment.AuthAPIKeyEnabled = true

	state := APIKeyAuthState{
		cfg:        cfg,
		namespace:  namespace,
		modelName:  deployment.Name,
		hostHeader: cfg.Suffix.HostHeader(namespace),
	}

	if err := cfg.Lifecycle.EnsureNamespace(ctx, namespace, true); err != nil {
		return state, fmt.Errorf("ensure namespace %s: %w", namespace, err)
	}
	if err := cfg.Lifecycle.EnsureModelDeployment(ctx, deployment); err != nil {
		return state, fmt.Errorf("ensure deployment %s/%s: %w", namespace, deployment.Name, err)
	}
	access, err := cfg.Lifecycle.NamespaceAccess(ctx, namespace)
	if err != nil {
		return state, fmt.Errorf("resolve namespace access for %s: %w", namespace, err)
	}
	state.access = access
	if access.HostHeader != "" {
		state.hostHeader = access.HostHeader
	}

	return state, nil
}

// APIKeyAuthAssertions returns the group's three named, independently
// reportable checks in execution order.
func APIKeyAuthAssertions(state APIKeyAuthState) []Assertion {
	return []Assertion{
		namedAssertion("rejects requests without an Authorization header (401)", func(ctx context.Context) error {
			return AssertRejectsMissingAuth(ctx, state)
		}),
		namedAssertion("rejects requests with an invalid API key (401)", func(ctx context.Context) error {
			return AssertRejectsInvalidAPIKey(ctx, state)
		}),
		namedAssertion("accepts requests with a valid API key (200)", func(ctx context.Context) error {
			return AssertAcceptsValidAPIKey(ctx, state)
		}),
	}
}

// AssertRejectsMissingAuth polls until a request with no Authorization
// header returns 401. Uses an empty bearer token (rather than the
// deployment's known-good key) so a policy-not-enforcing regression is
// caught rather than masked.
func AssertRejectsMissingAuth(ctx context.Context, state APIKeyAuthState) error {
	timeout := state.cfg.MissingAuthTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	interval := state.cfg.MissingAuthPollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}

	deadline := time.Now().Add(timeout)
	var lastStatus int
	var lastErr error
	for {
		resp, err := state.cfg.Chat.SendChat(ctx, state.access.GatewayURL, ChatRequest{
			Model:      state.modelName,
			Prompt:     "hello",
			HostHeader: state.hostHeader,
		})
		if err != nil {
			lastErr = err
		} else {
			lastStatus = resp.StatusCode
			lastErr = nil
			if lastStatus == http.StatusUnauthorized {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("request without auth should be rejected with 401: last error: %w", lastErr)
			}
			return fmt.Errorf("request without auth should be rejected with 401, got %d", lastStatus)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// AssertRejectsInvalidAPIKey sends a single request with a bogus bearer
// token and asserts it is rejected with 401.
func AssertRejectsInvalidAPIKey(ctx context.Context, state APIKeyAuthState) error {
	resp, err := state.cfg.Chat.SendChat(ctx, state.access.GatewayURL, ChatRequest{
		Model:       state.modelName,
		Prompt:      "hello",
		BearerToken: "invalid-key-12345",
		HostHeader:  state.hostHeader,
	})
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("invalid key should be rejected; got status %d", resp.StatusCode)
	}
	return nil
}

// AssertAcceptsValidAPIKey polls until a request bearing the namespace's
// real API key returns 200.
func AssertAcceptsValidAPIKey(ctx context.Context, state APIKeyAuthState) error {
	timeout := state.cfg.ValidKeyTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	interval := state.cfg.ValidKeyPollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}

	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		resp, err := state.cfg.Chat.SendChat(ctx, state.access.GatewayURL, ChatRequest{
			Model:       state.modelName,
			Prompt:      "hello",
			BearerToken: state.access.APIKey,
			HostHeader:  state.hostHeader,
		})
		if err != nil {
			lastErr = fmt.Errorf("request failed: %w", err)
		} else if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("expected 200, got %d: %s", resp.StatusCode, string(resp.Body))
		} else {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("request with valid API key should succeed with 200: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
