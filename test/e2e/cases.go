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
// entry — deployments are NOT shared across cases. Suite-level cases are
// installed once by BeforeSuite (their union is the steady-state fixture);
// lifecycle cases are installed per-test in their own random namespace.
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
//   - Namespace: left empty here; callers (Setup / per-case install) inject
//     the runtime namespace at install time.
//   - Replicas / InstanceType / GatewayName: explicit so test assertions can
//     compare against a known-good value.
var CaseDeployments = map[string][]utils.ModelDeploymentValues{
	CaseGPUMocker: {
		{
			Name:         "gpu-mocker-phi",
			Model:        presetPhi,
			Replicas:     2,
			InstanceType: "Standard_NV36ads_A10_v5",
			GatewayName:  "inference-gateway",
		},
		{
			Name:         "gpu-mocker-ministral",
			Model:        presetMinistral,
			Replicas:     2,
			InstanceType: "Standard_NV36ads_A10_v5",
			GatewayName:  "inference-gateway",
		},
	},
	CaseModelRouting: {
		{
			Name:         "routing-phi",
			Model:        presetPhi,
			Replicas:     2,
			InstanceType: "Standard_NV36ads_A10_v5",
			GatewayName:  "inference-gateway",
		},
		{
			Name:         "routing-ministral",
			Model:        presetMinistral,
			Replicas:     2,
			InstanceType: "Standard_NV36ads_A10_v5",
			GatewayName:  "inference-gateway",
		},
	},
	CasePrefixCache: {
		{
			Name:         "prefix-cache-phi",
			Model:        presetPhi,
			Replicas:     2, // prefix-cache tests require ≥2 shadow pods.
			InstanceType: "Standard_NV36ads_A10_v5",
			GatewayName:  "inference-gateway",
		},
	},
	CaseModelDeploymentChart: {
		{
			Name:         "mdchart-phi",
			Model:        presetPhi,
			Replicas:     2,
			InstanceType: "Standard_NV36ads_A10_v5",
			GatewayName:  "inference-gateway",
		},
	},
}

// CaseDeploymentsWithNamespace returns a copy of CaseDeployments[caseName]
// with Namespace set to ns on every entry. Use this whenever the case is
// installed into a namespace that differs from the table's default (empty).
func CaseDeploymentsWithNamespace(caseName, ns string) []utils.ModelDeploymentValues {
	src := CaseDeployments[caseName]
	out := make([]utils.ModelDeploymentValues, len(src))
	for i, v := range src {
		v.Namespace = ns
		out[i] = v
	}
	return out
}

// AllSuiteDeployments returns the concatenation of every suite-level case's
// deployments (in deterministic order), with Namespace stamped to ns. This
// is the full set of Helm releases BeforeSuite installs. Per-test cases
// (CaseModelDeploymentChart) are intentionally excluded — they are
// installed per-test in a fresh namespace.
func AllSuiteDeployments(ns string) []utils.ModelDeploymentValues {
	suiteCases := []string{
		CaseGPUMocker,
		CaseModelRouting,
		CasePrefixCache,
	}
	var out []utils.ModelDeploymentValues
	for _, key := range suiteCases {
		out = append(out, CaseDeploymentsWithNamespace(key, ns)...)
	}
	return out
}
