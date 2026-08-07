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
	"context"
	"fmt"
	"sync"

	"github.com/kaito-project/production-stack/test/e2e/scenarios"
)

// HelmLifecycle implements scenarios.Lifecycle on top of this package's
// existing Helm-backed provisioning (EnsureNamespace,
// InstallModelDeployment, SetupInferenceSetsWithRouting, ...). It is what
// `make test-e2e` uses: behavior is unchanged from before this package
// existed — same Helm releases, same per-namespace Gateway wait, same
// gateway warm-up loop, same per-case auth settings (a deployment only
// gets an API key when its DeploymentSpec.AuthAPIKeyEnabled is true; this
// adapter never forces auth on for cases that don't request it).
//
// A HelmLifecycle instance is stateful: it remembers each namespace's
// resolved Gateway URL and (when applicable) API key between
// EnsureNamespace/EnsureModelDeployment and the later NamespaceAccess
// call, since the Helm chart's Gateway URL only depends on the namespace
// (not on any one deployment). Safe for concurrent use across parallel
// Ginkgo workers operating on different namespaces.
type HelmLifecycle struct {
	mu    sync.Mutex
	state map[string]*helmNamespaceState
}

type helmNamespaceState struct {
	gatewayURL  string
	apiKey      string
	authEnabled bool
}

// NewHelmLifecycle returns a ready-to-use HelmLifecycle.
func NewHelmLifecycle() *HelmLifecycle {
	return &HelmLifecycle{state: map[string]*helmNamespaceState{}}
}

var _ scenarios.Lifecycle = (*HelmLifecycle)(nil)

// EnsureNamespace implements scenarios.Lifecycle.
func (h *HelmLifecycle) EnsureNamespace(ctx context.Context, namespace string, authEnabled bool) error {
	if err := EnsureClusterClient(TestingCluster); err != nil {
		return err
	}
	if err := EnsureNamespace(ctx, namespace, authEnabled); err != nil {
		return err
	}

	gatewayName := namespace + "-gw"
	if err := WaitForGatewayService(ctx, namespace, gatewayName, InferenceSetReadyTimeout); err != nil {
		return err
	}
	gatewayURL, err := GetGatewayURLFor(namespace, gatewayName)
	if err != nil {
		return fmt.Errorf("resolve gateway URL for %s: %w", namespace, err)
	}

	h.mu.Lock()
	h.state[namespace] = &helmNamespaceState{gatewayURL: gatewayURL, authEnabled: authEnabled}
	h.mu.Unlock()
	return nil
}

// DeleteNamespace implements scenarios.Lifecycle.
func (h *HelmLifecycle) DeleteNamespace(ctx context.Context, namespace string) error {
	h.mu.Lock()
	delete(h.state, namespace)
	h.mu.Unlock()
	return DeleteNamespace(ctx, namespace)
}

// EnsureModelDeployment implements scenarios.Lifecycle. Reuses
// SetupInferenceSetsWithRouting so the warm-up behavior (wait for EPP +
// inference pods, then poll the gateway until it routes successfully,
// re-reading the API key Secret on every retry when auth is enabled) is
// identical to today's InstallCase path.
func (h *HelmLifecycle) EnsureModelDeployment(ctx context.Context, spec scenarios.DeploymentSpec) error {
	h.mu.Lock()
	st, ok := h.state[spec.Namespace]
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("namespace %s was not ensured before deployment %s (call EnsureNamespace first)", spec.Namespace, spec.Name)
	}

	values := ModelDeploymentValues{
		Name:              spec.Name,
		Namespace:         spec.Namespace,
		Model:             spec.Model,
		Replicas:          spec.Replicas,
		InstanceType:      spec.InstanceType,
		AuthAPIKeyEnabled: spec.AuthAPIKeyEnabled,
	}
	SetupInferenceSetsWithRouting([]ModelDeploymentValues{values}, spec.Namespace, st.gatewayURL)

	if spec.AuthAPIKeyEnabled {
		key, err := GetAPIKeyFromSecret(ctx, spec.Namespace)
		if err != nil {
			return fmt.Errorf("read API key for %s: %w", spec.Namespace, err)
		}
		h.mu.Lock()
		st.apiKey = key
		h.mu.Unlock()
	}
	return nil
}

// DeleteModelDeployment implements scenarios.Lifecycle.
func (h *HelmLifecycle) DeleteModelDeployment(_ context.Context, namespace, name string) error {
	return UninstallModelDeployment(name, namespace)
}

// NamespaceAccess implements scenarios.Lifecycle.
func (h *HelmLifecycle) NamespaceAccess(_ context.Context, namespace string) (scenarios.NamespaceAccess, error) {
	h.mu.Lock()
	st, ok := h.state[namespace]
	h.mu.Unlock()
	if !ok {
		return scenarios.NamespaceAccess{}, fmt.Errorf("namespace %s was not ensured (call EnsureNamespace first)", namespace)
	}
	access := scenarios.NamespaceAccess{
		GatewayURL:  st.gatewayURL,
		APIKey:      st.apiKey,
		AuthEnabled: st.authEnabled,
	}
	// Only set HostHeader for auth-enabled namespaces, matching the
	// pre-existing per-case Helm behavior (SetupInferenceSetsWithRouting
	// only overrides Host when AuthAPIKeyEnabled): the apikey-authz
	// service needs the subdomain-encoded namespace to resolve auth
	// policy, but non-auth namespaces route on the Gateway's default
	// listener and must not have their Host header perturbed.
	if st.authEnabled {
		access.HostHeader = namespace + ".gw.example.com"
	}
	return access, nil
}
