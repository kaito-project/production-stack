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

	// GinkgoLabelAuth marks tests that verify API key authentication.
	GinkgoLabelAuth = g.Label("Auth")

	// GinkgoLabelNetworkPolicy marks tests that verify NetworkPolicy enforcement.
	GinkgoLabelNetworkPolicy = g.Label("NetworkPolicy")

	// GinkgoLabelScaling marks tests that verify end-to-end KEDA-driven
	// scaling of InferenceSet replicas. Nightly-only because KEDA polling
	// and cooldown windows make these tests take minutes per run.
	GinkgoLabelScaling = g.Label("Scaling")

	// GinkgoLabelScaleUp marks the scale-up phase of a scaling test.
	GinkgoLabelScaleUp = g.Label("ScaleUp")

	// GinkgoLabelScaleDown marks the scale-down phase of a scaling test.
	GinkgoLabelScaleDown = g.Label("ScaleDown")

	// GinkgoLabelAntiFlapping marks anti-flapping tests that verify KEDA
	// does not oscillate replicas near the threshold or during cooldown.
	GinkgoLabelAntiFlapping = g.Label("AntiFlapping")
)
