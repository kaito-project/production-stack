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
	"time"
)

// GPUSmokeConfig configures one run of the GPU / framework smoke group.
// This group covers ONLY the Smoke-labeled "framework validation" and
// "gateway connectivity" checks — the rest of gpu_mocker_test.go (Infra /
// Routing labeled InferenceSet, fake-node, shadow-pod, and GC assertions)
// is out of scope for this library and is left untouched in Ginkgo.
type GPUSmokeConfig struct {
	// Suffix is applied to every physical Namespace/ModelDeployment name
	// this group creates.
	Suffix Suffix
	// Namespace is the logical (pre-suffix) namespace name.
	Namespace string
	// Deployment is the logical (pre-suffix) deployment the group stands
	// up and sends smoke traffic to.
	Deployment DeploymentSpec

	Lifecycle Lifecycle
	Chat      ChatClient
	Logger    Logger

	// GatewayTimeout bounds how long AssertGatewayReachable retries
	// before failing. Defaults to 5 minutes when zero.
	GatewayTimeout time.Duration
	// GatewayPollInterval is the retry interval for
	// AssertGatewayReachable. Defaults to 10 seconds when zero.
	GatewayPollInterval time.Duration
}

// GPUSmokeState is the resolved state produced by GPUSmokeSetup, threaded
// into every GPU-smoke assertion.
type GPUSmokeState struct {
	cfg       GPUSmokeConfig
	namespace string
	modelName string
	access    NamespaceAccess
}

// Resources returns the physical resources this run owns, for Cleanup /
// Diagnostics.
func (s GPUSmokeState) Resources() GroupResources {
	return GroupResources{
		Namespaces: []string{s.namespace},
		Deployments: []DeploymentSpec{
			{Namespace: s.namespace, Name: s.modelName},
		},
	}
}

// GPUSmokeSetup provisions the group's namespace and single model
// deployment (non-auth — the GPU-mocker case does not enable the
// API-key AuthorizationPolicy) and resolves the case Gateway URL.
//
// The returned GPUSmokeState always carries cfg plus the resolved
// physical namespace/deployment identities — even when an error is
// returned — so a caller can still call state.Resources() (and thus
// Cleanup / Diagnostics) to locate whatever was partially created before
// the failure.
func GPUSmokeSetup(ctx context.Context, cfg GPUSmokeConfig) (GPUSmokeState, error) {
	namespace := cfg.Suffix.Namespace(cfg.Namespace)
	deployment := cfg.Deployment
	deployment.Namespace = namespace
	deployment.Name = cfg.Suffix.Deployment(deployment.Name)

	state := GPUSmokeState{
		cfg:       cfg,
		namespace: namespace,
		modelName: deployment.Name,
	}

	if err := cfg.Lifecycle.EnsureNamespace(ctx, namespace, deployment.AuthAPIKeyEnabled); err != nil {
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

	return state, nil
}

// GPUSmokeAssertions returns the group's two named, independently
// reportable checks in execution order.
func GPUSmokeAssertions(state GPUSmokeState) []Assertion {
	return []Assertion{
		namedAssertion("framework properly initialised", func(ctx context.Context) error {
			return AssertFrameworkInitialised()
		}),
		namedAssertion("gateway reachable and returns a response", func(ctx context.Context) error {
			return AssertGatewayReachable(ctx, state)
		}),
	}
}

// AssertFrameworkInitialised is a trivial sanity check that the test
// framework/harness wiring itself is functional (mirrors the pre-existing
// "should have the test framework properly initialised" spec).
func AssertFrameworkInitialised() error {
	return nil
}

// AssertGatewayReachable sends chat-completion requests to the case
// Gateway, retrying with backoff, and fails if no attempt returns 200
// within the timeout. Retries with backoff because the BBR/EPP ext_proc
// filters may need time to establish gRPC connections right after
// cluster/namespace setup — this Eventually-style polling lives inside
// the assertion itself, not as a wrapper around the whole case.
//
// Requests automatically carry state.access.APIKey / HostHeader when the
// Lifecycle populated them (e.g. an AIManager-mode Lifecycle that always
// provisions an API key) and omit them when absent (Helm mode's default
// non-auth namespaces), so this group works unmodified against either
// mode.
func AssertGatewayReachable(ctx context.Context, state GPUSmokeState) error {
	timeout := state.cfg.GatewayTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	interval := state.cfg.GatewayPollInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}

	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		resp, err := state.cfg.Chat.SendChat(ctx, state.access.GatewayURL, ChatRequest{
			Model:       state.modelName,
			Prompt:      "hello",
			BearerToken: state.access.APIKey,
			HostHeader:  state.access.HostHeader,
		})
		if err != nil {
			lastErr = fmt.Errorf("request failed: %w", err)
		} else if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("expected 200, got %d: %s", resp.StatusCode, string(resp.Body))
		} else {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("case gateway should be reachable and return 200: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
