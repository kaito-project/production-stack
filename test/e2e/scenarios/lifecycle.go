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

import "context"

// DeploymentSpec is the input needed to stand up one model deployment
// inside a scenario namespace. Each scenario group's Setup function
// (GPUSmokeSetup, APIKeyAuthSetup, FilterOrderSetup, ClusterFilterHASetup)
// applies the caller-provided Suffix (see names.go) to its logical base
// Name/Namespace *before* calling Lifecycle, so by the time a Lifecycle
// implementation sees a DeploymentSpec, Name and Namespace are already
// physical (suffixed) names — Lifecycle implementations never apply a
// Suffix themselves. An empty Suffix is a no-op, so existing Helm-suite
// call sites (which don't pass one) see logical and physical names as
// identical.
type DeploymentSpec struct {
	// Name is the ModelDeployment / Helm release name, and the value
	// carried in the `model` field of OpenAI-style chat requests
	// (matched by the gateway's HTTPRoute as X-Gateway-Model-Name).
	Name string
	// Namespace is the target namespace for this deployment.
	Namespace string
	// Model is the inference preset name.
	Model string
	// Replicas is the desired number of serving replicas.
	Replicas int64
	// InstanceType is the VM instance type backing the deployment.
	InstanceType string
	// AuthAPIKeyEnabled requests that the namespace/deployment be
	// provisioned behind API-key ext_authz. Helm-mode Lifecycle
	// implementations honor this per-deployment (preserving today's
	// per-case auth settings); AIManager-mode implementations may choose
	// to always provision an API key regardless of this flag (see
	// NamespaceAccess).
	AuthAPIKeyEnabled bool
}

// NamespaceAccess is what a scenario needs to reach one namespace's
// dataplane: the resolved Gateway URL, and — mode-dependent — an API key.
//
// Helm-mode Lifecycle implementations only populate APIKey when the
// namespace's deployment(s) requested AuthAPIKeyEnabled, preserving the
// existing per-case auth behavior of `make test-e2e`. AIManager-mode
// implementations may always return an API key regardless of
// AuthAPIKeyEnabled so every case can exercise authenticated traffic;
// AuthEnabled reflects what actually happened for a given namespace so
// assertions can behave correctly either way.
type NamespaceAccess struct {
	// GatewayURL is the base URL scenario traffic should target.
	GatewayURL string
	// APIKey is the bearer token for authenticated requests, or "" when
	// the namespace has no auth configured.
	APIKey string
	// AuthEnabled reports whether this namespace is actually running
	// behind API-key ext_authz (independent of whether APIKey is set —
	// though in practice APIKey is non-empty iff AuthEnabled is true).
	AuthEnabled bool
	// HostHeader is the Host header value the apikey-authz service
	// resolves to this namespace (subdomain = namespace), needed by auth
	// assertions that must set an explicit Host on raw requests.
	HostHeader string
}

// Lifecycle is the narrow, injected surface each scenario group uses to
// stand up / tear down namespaces and model deployments, and to resolve
// per-namespace dataplane access. Scenario code never talks to Helm,
// AIManager, or the Kubernetes API directly for these operations — it
// only ever calls through this interface, so the same scenario logic runs
// unmodified against either backend.
//
// Implementations:
//   - Helm-backed (this repo): test/e2e/utils/lifecycle_helm.go, wrapping
//     the existing EnsureNamespace / InstallModelDeployment helpers.
//   - AIManager-backed (downstream repo): not implemented here.
//   - Fake (unit tests): test/e2e/scenarios/fakes_test.go.
type Lifecycle interface {
	// EnsureNamespace idempotently provisions the (already physical /
	// suffixed) namespace with the given auth setting and blocks until
	// the namespace's shared dataplane (Gateway, catch-all route, and —
	// when authEnabled — the AuthorizationPolicy/APIKey) is ready.
	EnsureNamespace(ctx context.Context, namespace string, authEnabled bool) error
	// DeleteNamespace tears down the namespace and every resource it
	// owns (Gateway, HTTPRoutes, auth artifacts, ...).
	DeleteNamespace(ctx context.Context, namespace string) error
	// EnsureModelDeployment stands up (or reconciles) one model
	// deployment — spec.Name/spec.Namespace are already physical
	// (suffixed) names — and blocks until it is ready to serve traffic
	// through the namespace's Gateway.
	EnsureModelDeployment(ctx context.Context, spec DeploymentSpec) error
	// DeleteModelDeployment tears down one model deployment.
	DeleteModelDeployment(ctx context.Context, namespace, name string) error
	// NamespaceAccess resolves the endpoint (and, mode-dependent, API
	// key) scenario assertions should use to reach the namespace.
	NamespaceAccess(ctx context.Context, namespace string) (NamespaceAccess, error)
}
