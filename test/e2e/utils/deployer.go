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
	"sync"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
	_ "github.com/kaito-project/production-stack/test/e2e/deploy/helm" // registers the default "helm" backend
)

var (
	deployerMu      sync.Mutex
	currentDeployer deploy.Deployer
)

// SetDeployer installs the deploy.Deployer every lifecycle helper in this
// package delegates to. Out-of-tree suites that reuse these E2E specs call it
// once from their suite bootstrap, before any spec runs, to supply their own
// backend. Passing nil restores environment-based selection.
func SetDeployer(d deploy.Deployer) {
	deployerMu.Lock()
	defer deployerMu.Unlock()
	currentDeployer = d
}

// CurrentDeployer returns the active Deployer, constructing it from
// E2E_DEPLOYMENT_BACKEND (default "helm") on first use.
func CurrentDeployer() (deploy.Deployer, error) {
	deployerMu.Lock()
	defer deployerMu.Unlock()

	if currentDeployer != nil {
		return currentDeployer, nil
	}
	d, err := deploy.NewFromEnv()
	if err != nil {
		return nil, err
	}
	currentDeployer = d
	return currentDeployer, nil
}

// InstallModelHarness creates or reconciles the modelharness owning the
// per-namespace shared resources: the Istio Gateway (named "<namespace>-gw" by
// chart default), the catch-all `model-not-found-direct` EnvoyFilter, and —
// when authEnabled is true — the AuthorizationPolicy + APIKey CR that wire the
// Gateway into the cluster-wide apikey-ext-authz CUSTOM provider.
//
// Idempotent: safe to call repeatedly for the same namespace.
func InstallModelHarness(ctx context.Context, namespace string, authEnabled bool) error {
	d, err := CurrentDeployer()
	if err != nil {
		return err
	}
	return d.InstallModelHarness(ctx, deploy.ModelHarnessValues{
		Namespace:   namespace,
		AuthEnabled: authEnabled,
	})
}

// UninstallModelHarness removes the modelharness from namespace. A namespace
// with no modelharness is treated as success.
func UninstallModelHarness(ctx context.Context, namespace string) error {
	d, err := CurrentDeployer()
	if err != nil {
		return err
	}
	return d.UninstallModelHarness(ctx, namespace)
}

// InstallModelDeployment creates or reconciles a model deployment
// (InferenceSet, InferencePool, EPP artifacts, and HTTPRoute) from values.
// Idempotent: re-running reconciles to the supplied values.
func InstallModelDeployment(ctx context.Context, values deploy.ModelDeploymentValues) error {
	d, err := CurrentDeployer()
	if err != nil {
		return err
	}
	return d.InstallModelDeployment(ctx, values)
}

// UninstallModelDeployment removes the named model deployment. Missing
// deployments are treated as success, so cleanup is idempotent.
func UninstallModelDeployment(ctx context.Context, name, namespace string) error {
	d, err := CurrentDeployer()
	if err != nil {
		return err
	}
	return d.UninstallModelDeployment(ctx, name, namespace)
}
