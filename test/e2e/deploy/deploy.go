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

// Package deploy defines the backend-agnostic contract the E2E suite uses to
// manage modelharness and modeldeployment lifecycles.
//
// The package intentionally depends on nothing but the standard library so it
// can be imported by out-of-tree repositories that supply their own Deployer
// (for example an Azure AI Manager implementation backed by
// armcontainerserviceaimanager) without pulling in Helm, Ginkgo, or
// Kubernetes client dependencies.
package deploy

import "context"

// Deployer manages the lifecycle of the two resources the E2E suite deploys:
// the per-namespace modelharness and the per-deployment modeldeployment.
//
// Implementations must satisfy the following contract:
//
//   - Install operations are idempotent: calling them against an existing
//     resource reconciles it to the supplied values rather than failing.
//   - Uninstall operations are idempotent: a missing resource is not an error.
//   - Values are validated before any remote call is issued, so that a
//     malformed request fails fast and locally.
//   - Errors identify the resource involved and preserve the terminal
//     backend error, and never include credentials or access keys.
//
// Implementations are not required to be safe for concurrent use with
// different values for the same resource; the suite serializes lifecycle
// operations per namespace.
type Deployer interface {
	// Name returns the backend identifier this Deployer was registered
	// under, such as "helm". It is used in E2E logs and failure messages to
	// make the active backend unambiguous.
	Name() string

	// InstallModelHarness creates or reconciles the modelharness owning the
	// per-namespace shared resources (Gateway, catch-all EnvoyFilter, and
	// optionally the AuthorizationPolicy + APIKey pair).
	//
	// The workload namespace is part of what a modelharness owns, so
	// implementations create it (carrying whatever discovery labels the
	// control plane selects on) rather than expecting the caller to.
	InstallModelHarness(ctx context.Context, values ModelHarnessValues) error

	// UninstallModelHarness removes the modelharness from namespace,
	// including the namespace itself. A namespace with no modelharness is
	// treated as success.
	UninstallModelHarness(ctx context.Context, namespace string) error

	// AuthHeaders returns the header sets that each independently authenticate
	// a request to the namespace's gateway, in the backend's preferred order.
	//
	// An empty result means no credentials are available. E2E harnesses always
	// enable authentication, so the suite waits for a nonempty result instead
	// of sending bare requests. More than one entry means several equivalent
	// transports; the API-key specs iterate over them rather than hard-coding
	// header names, so a backend that accepts only one is not failed for the
	// ones it never claimed to support.
	//
	// It is part of the interface because both the credential and the header
	// carrying it are properties of the backend, not of the cluster: the Helm
	// backend reads the Secret the apikey-operator reconciles from the
	// harness's APIKey CR and accepts it in any of three headers, while a
	// managed backend mints the credential through its own API and pins the
	// transport its gateway classifies on.
	AuthHeaders(ctx context.Context, namespace string) ([]AuthHeader, error)

	// OpenGateway returns the endpoint inference requests for namespace are
	// sent to, allocating whatever transport the backend needs to reach it.
	//
	// How the gateway is reachable is a backend property, not a cluster one: a
	// self-hosted stack is only reachable through a kubectl port-forward, while
	// a managed control plane publishes a routable URL over its own API and
	// denies the pods/portforward subresource outright.
	//
	// Repeated calls for the same namespace return an equivalent endpoint
	// rather than allocating a second transport.
	OpenGateway(ctx context.Context, namespace, gatewayName string) (GatewayEndpoint, error)

	// CloseGateway releases what OpenGateway allocated for namespace. Callers
	// invoke it before deleting the namespace, so a backend does not keep
	// trying to reach one that is gone. A namespace that was never opened is
	// treated as success.
	CloseGateway(ctx context.Context, namespace string) error

	// InstallModelDeployment creates or reconciles a single model deployment
	// (InferenceSet, InferencePool, EPP artifacts, and HTTPRoute).
	InstallModelDeployment(ctx context.Context, values ModelDeploymentValues) error

	// UpgradeModelDeployment reconciles an EXISTING model deployment to
	// values, and is how a test changes a deployment's declared
	// configuration (replica count, scaling thresholds, scorer weights, ...).
	//
	// Unlike InstallModelDeployment it does not create the deployment: a
	// missing deployment is an error, so a test that mistypes a name fails
	// loudly instead of silently provisioning a second one.
	//
	// Note this reconciles DECLARED values. A field the chart hands off to
	// another controller at runtime — spec.replicas once EnableScaling is on,
	// which KEDA then drives through the scale subresource — will not change
	// live state through this call.
	UpgradeModelDeployment(ctx context.Context, values ModelDeploymentValues) error

	// UninstallModelDeployment removes the named model deployment from
	// namespace. A missing deployment is treated as success.
	UninstallModelDeployment(ctx context.Context, name, namespace string) error
}

// AuthHeader is one request header that authenticates a caller against a
// namespace gateway. Value is complete as sent, including any scheme prefix
// such as "Bearer ".
type AuthHeader struct {
	Name  string
	Value string
}

// GatewayEndpoint is a live handle to one namespace gateway's
// OpenAI-compatible API root.
type GatewayEndpoint interface {
	// BaseURL returns the URL that "/chat/completions" or "/models" is
	// appended to — the API root, so it already carries the "/v1" prefix.
	//
	// It is a method rather than a field because a tunnelled transport does
	// not survive a whole suite: callers get the currently valid URL here, and
	// a backend that had to rebuild its tunnel on a fresh local port heals
	// transparently instead of handing out an address that stopped working.
	BaseURL() (string, error)

	// Host is the Host header the gateway's authz service resolves the
	// workload namespace from, or "" when BaseURL already carries it.
	Host() string

	// Reset discards the current transport so the next BaseURL rebuilds it.
	// Callers use it after a transport-level failure, which a tunnel can
	// produce while its process is still alive and therefore still looks
	// healthy. A no-op for endpoints that hold no transport.
	Reset()
}
