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

	// CaseNetworkPolicyA covers the primary workload namespace in
	// network_policy_test.go (default-deny-ingress + allow-inference
	// NetworkPolicy pair). Holds the model pod that the deny / allow
	// probes target.
	CaseNetworkPolicyA = "network-policy-a"

	// CaseNetworkPolicyB covers the secondary workload namespace in
	// network_policy_test.go used to prove cross-namespace isolation
	// between two NetworkPolicy-locked-down workload namespaces.
	CaseNetworkPolicyB = "network-policy-b"

	// CaseScaling covers scaling_test.go (KEDA-driven Scale-Up / Scale-Down
	// of a single model, fake-node and shadow-pod inventory churn,
	// background load to assert no 5xx during transitions).
	CaseScaling = "scaling"

	// CaseFilterOrder covers filter_order_test.go — verifies the Envoy
	// HTTP filter chain execution order on a per-namespace Gateway:
	//   ext_authz  →  ext_proc.bbr  →  ext_proc (InferencePool/EPP)  →  router
	// The case provisions an API-key-enabled deployment so that
	// ext_authz, BBR, EPP and the catch-all route are all exercised by
	// the same dataplane.
	CaseFilterOrder = "filter-order"

	// CaseClusterFilterHA covers cluster_filter_ha_test.go — BBR
	// high-availability and single-replica-loss failover (issue #89).
	// A lightweight gpu-mocker-style deployment provides a working
	// BBR → EPP request path; the test then perturbs the cluster-wide
	// BBR Deployment in kaito-system (delete one replica / scale to
	// zero) and asserts the request path stays healthy while ≥1 replica
	// survives and only fails closed when ALL replicas are down.
	CaseClusterFilterHA = "cluster-filter-ha"

	// CaseBBROutage covers bbr_outage_test.go — verifies that when the
	// cluster-wide BBR ext_proc filter is unavailable (fail-closed), a
	// request is mapped to a 502 `bbr_unavailable` outage reply by the
	// per-namespace local_reply, and NOT to a misleading 404 model_not_found.
	// Non-auth so only the BBR + EPP filters are in path.
	CaseBBROutage = "bbr-outage"

	// CaseExtAuthzOutage covers ext_authz_outage_test.go — verifies that
	// when the cluster-wide llm-gateway-auth ext_authz filter is
	// unavailable (fail-closed), an authenticated request is NOT mapped to
	// a misleading 404 model_not_found, and (best-effort) surfaces a 502
	// `ext_authz_unavailable` outage reply. Auth-enabled so ext_authz is
	// in path.
	CaseExtAuthzOutage = "ext-authz-outage"

	// CaseEPPOutage covers epp_outage_test.go — verifies that when the
	// per-InferenceSet EPP (InferencePool ext_proc, failureMode: FailClose)
	// is unavailable, a request is mapped to a 502 `epp_unavailable` outage
	// reply (x-kaito-error-source: epp) by the consolidated per-namespace
	// local_reply (mapper #4: model header present + local 5xx, no router
	// flag), and NOT to a misleading 404 model_not_found. Scaling the EPP
	// Deployment to zero touches only this case's namespace, so the suite
	// does not need Serial. Non-auth so only BBR + EPP are in path.
	CaseEPPOutage = "epp-outage"

	// CaseModelUnavailable covers model_unavailable_test.go — verifies that
	// when the InferencePool has zero ready inference endpoints (the
	// InferenceSet is scaled to replicas=0) while the HTTPRoute and EPP
	// remain healthy, a request is mapped to a 503 `model_unavailable`
	// outage reply (x-kaito-error-source: inferenceset, Retry-After) by the
	// consolidated per-namespace local_reply (mapper #2: model header
	// present + router flag), and NOT to a 404. Touches only this case's
	// namespace (its InferenceSet replicas), so the suite does not need
	// Serial. Non-auth so only BBR + EPP are in path.
	CaseModelUnavailable = "model-unavailable"
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
//     dedicated Istio Gateway (named "<namespace>-gw" by chart
//     convention) so parallel Ginkgo workers can target independent
//     dataplanes. The Gateway is provisioned by EnsureNamespace via
//     the modelharness chart during InstallCase.
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
		},
	},
	CaseModelRouting: {
		{
			Name:         "routing-phi",
			Namespace:    "e2e-model-routing",
			Model:        presetPhi,
			Replicas:     2,
			InstanceType: "Standard_NV36ads_A10_v5",
		},
		{
			Name:         "routing-ministral",
			Namespace:    "e2e-model-routing",
			Model:        presetMinistral,
			Replicas:     2,
			InstanceType: "Standard_NV36ads_A10_v5",
		},
	},
	CasePrefixCache: {
		{
			Name:         "prefix-cache-phi",
			Namespace:    "e2e-prefix-cache",
			Model:        presetPhi,
			Replicas:     2, // prefix-cache tests require ≥2 shadow pods.
			InstanceType: "Standard_NV36ads_A10_v5",
		},
	},
	CaseModelDeploymentChart: {
		{
			Name:         "mdchart-phi",
			Namespace:    "e2e-mdchart",
			Model:        presetPhi,
			Replicas:     1,
			InstanceType: "Standard_NV36ads_A10_v5",
		},
	},
	CaseAuth: {
		{
			Name:              "auth-phi",
			Namespace:         "e2e-auth",
			Model:             presetPhi,
			Replicas:          2,
			InstanceType:      "Standard_NV36ads_A10_v5",
			AuthAPIKeyEnabled: true,
		},
	},
	CaseNetworkPolicyA: {
		{
			Name:         "netpol-a",
			Namespace:    "e2e-netpol-a",
			Model:        presetPhi,
			Replicas:     1,
			InstanceType: "Standard_NV36ads_A10_v5",
		},
	},
	CaseNetworkPolicyB: {
		{
			Name:         "netpol-b",
			Namespace:    "e2e-netpol-b",
			Model:        presetPhi,
			Replicas:     1,
			InstanceType: "Standard_NV36ads_A10_v5",
		},
	},
	CaseFilterOrder: {
		{
			// Auth-enabled deployment so the full Envoy HTTP filter
			// chain (ext_authz → bbr → ext_proc(EPP) → router) is
			// materialised on this case's per-namespace Gateway. Two
			// replicas let the load / endpoint-picker assertions in
			// filter_order_test.go observe non-trivial routing
			// decisions across more than one shadow pod.
			Name:              "filter-order-phi",
			Namespace:         "e2e-filter-order",
			Model:             presetPhi,
			Replicas:          2,
			InstanceType:      "Standard_NV36ads_A10_v5",
			AuthAPIKeyEnabled: true,
		},
	},
	CaseBBROutage: {
		{
			Name:         "bbr-outage-phi",
			Namespace:    "e2e-bbr-outage",
			Model:        presetPhi,
			Replicas:     1,
			InstanceType: "Standard_NV36ads_A10_v5",
		},
	},
	CaseExtAuthzOutage: {
		{
			Name:              "ext-authz-outage-phi",
			Namespace:         "e2e-ext-authz-outage",
			Model:             presetPhi,
			Replicas:          1,
			InstanceType:      "Standard_NV36ads_A10_v5",
			AuthAPIKeyEnabled: true,
		},
	},
	CaseEPPOutage: {
		{
			Name:         "epp-outage-phi",
			Namespace:    "e2e-epp-outage",
			Model:        presetPhi,
			Replicas:     1,
			InstanceType: "Standard_NV36ads_A10_v5",
		},
	},
	CaseModelUnavailable: {
		{
			Name:         "model-unavailable-phi",
			Namespace:    "e2e-model-unavailable",
			Model:        presetPhi,
			Replicas:     1,
			InstanceType: "Standard_NV36ads_A10_v5",
		},
	},
	CaseScaling: {
		{
			// Scaling baseline matches KEDA's minReplicaCount so that
			// scale-down can converge back to it. With Replicas>1, KEDA's
			// HPA computes desiredReplicas=ceil(metric/threshold)*1=0 when
			// the queue drains and clamps to minReplicaCount=1, so the
			// pool would never return to its starting size and Scale-Down
			// assertions would fail.
			Name:             "scaling-phi",
			Namespace:        "e2e-scaling",
			Model:            presetPhi,
			Replicas:         1,
			InstanceType:     "Standard_NV36ads_A10_v5",
			EnableScaling:    true,
			MaxReplicas:      2,
			ScalingThreshold: 10, // low threshold to trigger scaling during tests
		},
	},
	CaseClusterFilterHA: {
		{
			// Lightweight, GPU-mocked deployment (mirrors CaseGPUMocker):
			// the gpu-node-mocker patches the inference pod to Running so
			// the BBR → EPP request path returns 200 without a real GPU.
			// One replica is enough — this case exercises the cluster-wide
			// BBR Deployment's HA, not the model pool's.
			Name:         "cluster-filter-ha-phi",
			Namespace:    "e2e-cluster-filter-ha",
			Model:        presetPhi,
			Replicas:     1,
			InstanceType: "Standard_NV36ads_A10_v5",
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

// CaseGatewayName returns the Gateway name owned by the case namespace.
// Mirrors the chart convention "<namespace>-gw" (see
// charts/modelharness/templates/_helpers.tpl).
func CaseGatewayName(caseName string) string {
	ns := CaseNamespace(caseName)
	if ns == "" {
		return ""
	}
	return ns + "-gw"
}

// InstallCase provisions every modeldeployment Helm release owned by the
// given case into its declared namespace (see CaseDeployments) and waits
// for the EPP / inference pods + gateway routing to be ready. Returns the
// gateway URL that routes to this case's deployments.
//
// EnsureNamespace installs the modelharness chart (Gateway, catch-all
// HTTPRoute, ReferenceGrant, and optional auth artifacts) so each case
// has an isolated dataplane and parallel Ginkgo workers do not contend
// on a shared gateway.
//
// Intended to be called from a Ginkgo Ordered Describe's BeforeAll.
func InstallCase(caseName string) string {
	ns := CaseNamespace(caseName)
	gatewayName := CaseGatewayName(caseName)
	Expect(ns).NotTo(BeEmpty(), "case %q has no namespace declared in CaseDeployments", caseName)

	ctx := context.Background()
	first := CaseDeployments[caseName][0]
	Expect(utils.EnsureNamespace(ctx, ns, first.AuthAPIKeyEnabled)).To(Succeed(),
		"failed to ensure namespace %s for case %s", ns, caseName)

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
