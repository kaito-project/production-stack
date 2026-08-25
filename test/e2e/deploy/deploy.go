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
	InstallModelHarness(ctx context.Context, values ModelHarnessValues) error

	// UninstallModelHarness removes the modelharness from namespace. A
	// namespace with no modelharness is treated as success.
	UninstallModelHarness(ctx context.Context, namespace string) error

	// InstallModelDeployment creates or reconciles a single model deployment
	// (InferenceSet, InferencePool, EPP artifacts, and HTTPRoute).
	InstallModelDeployment(ctx context.Context, values ModelDeploymentValues) error

	// UninstallModelDeployment removes the named model deployment from
	// namespace. A missing deployment is treated as success.
	UninstallModelDeployment(ctx context.Context, name, namespace string) error
}
