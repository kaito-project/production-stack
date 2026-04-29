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

package e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // Ginkgo DSL
	. "github.com/onsi/gomega"    //nolint:revive // Gomega DSL

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Inference preset names. These are the underlying KAITO presets and become
// the InferenceSet's spec.template.inference.preset.name. They do NOT appear
// in the gateway's HTTPRoute matches — those use the deployment Name.
//
// NOTE: We pin two presets so the model-routing tests can verify
// cross-model isolation. KAITO main HEAD (image tag `nightly-latest`)
// has dropped older presets like `falcon-7b-instruct` from its supported
// list — always cross-check `presets/workspace/models/model_catalog.yaml`
// in kaito-project/kaito@main when picking presets here.
const (
	presetPhi       = "phi-4-mini-instruct"
	presetMinistral = "ministral-3-3b-instruct"
)

// Test-case identifiers. Each case owns its own ModelDeploymentValues table
// entry — deployments are NOT shared across cases. Suite-level cases install
// their deployments via the Ordered Describe's BeforeAll / AfterAll;
// lifecycle cases install per-test in their own random namespace.
const (
	// CaseGPUMocker covers gpu_mocker_test.go (framework smoke, gateway
	// connectivity, InferenceSet/EPP/HTTPRoute observability, fake-node and
	// shadow-pod lifecycle, status patching, unknown-model 404).
	CaseGPUMocker = "gpu-mocker"

	// CaseModelRouting covers model_routing_test.go (single-model echo,
	// cross-model isolation, EPP metrics, load distribution, debug-filter
	// log chain, malformed-request handling).
	CaseModelRouting = "model-routing"

	// CasePrefixCache covers prefix_cache_routing_test.go (prefix/KV-cache
	// aware routing — needs ≥2 replicas of a single model).
	CasePrefixCache = "prefix-cache"

	// CaseModelDeploymentChart covers the "ModelDeployment Chart" Describe
	// in modeldeployment_chart_test.go (chart install/render assertions and
	// chart uninstall/deletion assertions, exercised by sibling It blocks
	// that share BeforeEach/AfterEach).
	CaseModelDeploymentChart = "modeldeployment-chart"

	// CaseAuth covers apikey_auth_test.go (API key authentication via
	// Istio ext_authz — valid/invalid/missing key scenarios).
	CaseAuth = "auth"
)

// CaseDeployments enumerates the full set of ModelDeploymentValues required
// by each test case. The table is the single source of truth for what the
// modeldeployment Helm chart is invoked with per case. Deployment Names are
// unique across the entire table so Helm releases never collide.
//
// Conventions:
//   - Name: deployment / Helm release name. AND the value carried in the
//     `model` field of OpenAI-style requests (matched by the HTTPRoute as
//     X-Gateway-Model-Name).
//   - Model: inference preset name, written to the InferenceSet's
//     spec.template.inference.preset.name.
//   - Namespace: per-case namespace — the suite installs into this
//     namespace directly. Each non-default namespace gets its own
//     dedicated Istio Gateway (named GatewayName) so parallel Ginkgo
//     workers can target independent dataplanes.
//   - GatewayName: the Gateway resource the HTTPRoute parents into. For
//     non-default namespaces this Gateway is provisioned by EnsureNamespace
//     during InstallCase. For the `default` namespace, the cluster-wide
//     "inference-gateway" installed by hack/e2e/scripts/install-components.sh
//     is reused.
//   - Replicas / InstanceType: explicit so test assertions can compare
//     against a known-good value.
var CaseDeployments = map[string][]utils.ModelDeploymentValues{
	CaseGPUMocker: {
		{
			Name:         "gpu-mocker-phi",
			Namespace:    "e2e-gpu-mocker",
			Model:        presetPhi,
			Replicas:     1,
			InstanceType: "Standard_NV36ads_A10_v5",
			GatewayName:  "gpu-mocker-gateway",
		},
	},
	CaseModelRouting: {
		{
			Name:         "routing-phi",
			Namespace:    "e2e-model-routing",
			Model:        presetPhi,
			Replicas:     2,
			InstanceType: "Standard_NV36ads_A10_v5",
			GatewayName:  "model-routing-gateway",
		},
		{
			Name:         "routing-ministral",
			Namespace:    "e2e-model-routing",
			Model:        presetMinistral,
			Replicas:     2,
			InstanceType: "Standard_NV36ads_A10_v5",
			GatewayName:  "model-routing-gateway",
		},
	},
	CasePrefixCache: {
		{
			Name:         "prefix-cache-phi",
			Namespace:    "e2e-prefix-cache",
			Model:        presetPhi,
			Replicas:     2, // prefix-cache tests require ≥2 shadow pods.
			InstanceType: "Standard_NV36ads_A10_v5",
			GatewayName:  "prefix-cache-gateway",
		},
	},
	CaseModelDeploymentChart: {
		{
			// Per-test case — Namespace is filled in at runtime with a
			// random suffix by modeldeployment_chart_test.go. The
			// GatewayName references the cluster-wide default Gateway
			// because the chart test asserts spec.parentRefs[0].name.
			Name:         "mdchart-phi",
			Model:        presetPhi,
			Replicas:     1,
			InstanceType: "Standard_NV36ads_A10_v5",
			GatewayName:  utils.DefaultGatewayName,
		},
	},
	CaseAuth: {
		{
			Name:         "auth-phi",
			Model:        presetPhi,
			Replicas:     2,
			InstanceType: "Standard_NV36ads_A10_v5",
			GatewayName:  "inference-gateway",
			AuthAPIKeyEnabled: true,
		},
	},
}

// CaseNamespace returns the namespace declared on the first deployment of
// the case (all deployments in a case share a namespace).
func CaseNamespace(caseName string) string {
	deployments := CaseDeployments[caseName]
	if len(deployments) == 0 {
		return ""
	}
	return deployments[0].Namespace
}

// CaseGatewayName returns the Gateway resource name declared on the first
// deployment of the case (all deployments in a case share a gateway).
func CaseGatewayName(caseName string) string {
	deployments := CaseDeployments[caseName]
	if len(deployments) == 0 {
		return ""
	}
	return deployments[0].GatewayName
}

// InstallCase provisions every modeldeployment Helm release owned by the
// given case into its declared namespace (see CaseDeployments) and waits
// for the EPP / inference pods + gateway routing to be ready. Returns the
// gateway URL that routes to this case's deployments.
//
// For non-default namespaces, EnsureNamespace also creates the case's
// dedicated Istio Gateway so each case has an isolated dataplane and
// parallel Ginkgo workers do not contend on a shared gateway.
//
// Intended to be called from a Ginkgo Ordered Describe's BeforeAll.
func InstallCase(caseName string) string {
	ns := CaseNamespace(caseName)
	gatewayName := CaseGatewayName(caseName)
	Expect(ns).NotTo(BeEmpty(), "case %q has no namespace declared in CaseDeployments", caseName)
	Expect(gatewayName).NotTo(BeEmpty(), "case %q has no GatewayName declared in CaseDeployments", caseName)

	ctx := context.Background()
	Expect(utils.EnsureNamespace(ctx, ns, gatewayName)).To(Succeed(),
		"failed to ensure namespace %s (gateway %s) for case %s", ns, gatewayName, caseName)

	Expect(utils.WaitForGatewayService(ctx, ns, gatewayName, utils.InferenceSetReadyTimeout)).
		To(Succeed(), "gateway service for %s did not appear", caseName)

	gatewayURL, err := utils.GetGatewayURLFor(ns, gatewayName)
	Expect(err).NotTo(HaveOccurred(), "failed to resolve gateway URL for case %s", caseName)

	utils.SetupInferenceSetsWithRouting(CaseDeployments[caseName], ns, gatewayURL)
	return gatewayURL
}

// UninstallCase tears down every modeldeployment Helm release owned by the
// given case and deletes the case's dedicated namespace (which cascades
// the per-case Gateway). Intended to be called from a Ginkgo Ordered
// Describe's AfterAll.
func UninstallCase(caseName string) {
	deployments := CaseDeployments[caseName]
	if len(deployments) == 0 {
		return
	}
	ns := deployments[0].Namespace
	utils.TeardownInferenceSetsWithRouting(deployments, ns)
	if err := utils.DeleteNamespace(context.Background(), ns); err != nil {
		GinkgoWriter.Printf("Cleanup warning: %v\n", err)
	}
}
