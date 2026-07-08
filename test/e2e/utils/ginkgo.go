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

import g "github.com/onsi/ginkgo/v2"

var (
	// GinkgoLabelSmoke marks lightweight smoke tests that can run without GPUs.
	GinkgoLabelSmoke = g.Label("Smoke")

	// GinkgoLabelInfra marks tests that verify infrastructure (fake nodes, shadow pods, InferencePools).
	GinkgoLabelInfra = g.Label("Infra")

	// GinkgoLabelRouting marks tests that verify model-based request routing.
	GinkgoLabelRouting = g.Label("Routing")

	// GinkgoLabelPrefixCache marks tests that verify prefix/KV-cache aware routing.
	GinkgoLabelPrefixCache = g.Label("PrefixCache")

	// GinkgoLabelInferenceSet marks tests that verify InferenceSet lifecycle and routing setup.
	GinkgoLabelInferenceSet = g.Label("InferenceSet")

	// GinkgoLabelNightly marks tests that are destructive or slow and only run in nightly CI.
	GinkgoLabelNightly = g.Label("Nightly")

	// GinkgoLabelKarpenter marks tests that exercise AKS NAP / Karpenter-driven
	// GPU node provisioning from zero and back to zero.
	GinkgoLabelKarpenter = g.Label("Karpenter")

	// GinkgoLabelAuth marks tests that verify API key authentication.
	GinkgoLabelAuth = g.Label("Auth")

	// GinkgoLabelNetworkPolicy marks tests that verify NetworkPolicy enforcement.
	GinkgoLabelNetworkPolicy = g.Label("NetworkPolicy")

	// GinkgoLabelScaling marks tests that verify end-to-end KEDA-driven
	// scaling of InferenceSet replicas, covering the scale-up and
	// scale-down phases as well as anti-flapping (no oscillation near the
	// threshold or during cooldown). Nightly-only because KEDA polling and
	// cooldown windows make these tests take minutes per run.
	GinkgoLabelScaling = g.Label("Scaling")

	// GinkgoLabelFilterOrder marks tests that verify the Envoy HTTP
	// filter chain execution order on the per-namespace Gateway:
	//   ext_authz → ext_proc.bbr → ext_proc (InferencePool/EPP) → router
	// See test/e2e/filter_order_test.go for the test matrix.
	GinkgoLabelFilterOrder = g.Label("FilterOrder")

	// GinkgoLabelOutage marks tests that verify fail-closed / high-availability
	// behavior when a filter or model-serving component goes down. It spans
	// two halves:
	//   - Cluster-wide singletons (BBR ext_proc, llm-gateway-auth ext_authz):
	//     HA / single-replica-loss failover and scale-to-zero fail-closed
	//     behavior. These perturb shared kaito-system Deployments, so the
	//     corresponding suites MUST be decorated Serial.
	//   - Per-namespace data plane (EPP ext_proc fail-closed -> epp_unavailable;
	//     zero ready inference endpoints -> model_unavailable). These perturb
	//     only the case's OWN namespace, so they do not need Serial.
	// See test/e2e/{bbr,ext_authz,epp}_outage_test.go, model_unavailable_test.go
	// and cluster_filter_ha_test.go.
	GinkgoLabelOutage = g.Label("Outage")
)
