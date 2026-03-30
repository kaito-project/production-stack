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
	. "github.com/onsi/ginkgo/v2"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Prefix-cache aware routing tests verify that the EPP (Endpoint Picker)
// correctly routes requests with the same prefix to the same backend pod,
// leveraging KV-cache locality for better performance.
//
// Validation approach:
//   - Determine which backend pod served a request by inspecting EPP metrics
//     (routing decision logs / x-gateway-destination-endpoint) and shadow pod
//     metrics (per-pod request counters from llm-d-inference-sim /metrics).
//   - "Same prompt prefix → same pod" is validated by sending identical prompts
//     multiple times and confirming the EPP consistently picks the same endpoint.
//   - "Different prompt categories → different pods" is validated by sending
//     distinct prompts and confirming they are distributed, then each category
//     sticks to its assigned pod on subsequent requests.
//
// Prerequisites (deployed on the test cluster):
//   - Istio Gateway with EPP configured
//   - KAITO InferenceSet with 2+ replicas (shadow pods running llm-d-inference-sim)
//   - llm-d-inference-sim configured with enable-kvcache: true

var _ = Describe("Prefix Cache Aware Routing", utils.GinkgoLabelPrefixCache, func() {

	Context("Same prompt repeated requests", func() {
		It("should route identical requests to the same backend pod", func() {
			Skip("TODO: implement")

			// Test plan:
			// 1. Send request with prompt "Explain quantum computing" 5 times
			// 2. After each request, query EPP metrics to identify
			//    which pod was selected (x-gateway-destination-endpoint or EPP logs)
			// 3. Alternatively, scrape each shadow pod's /metrics endpoint:
			//    - vllm:num_requests_running or request counter per pod
			//    - Compare request counts before and after to identify which pod served it
			// 4. Verify all 5 requests were routed to the same pod
			//    (prefix cache hit should make EPP prefer the same endpoint)
		})
	})

	Context("Different prompt categories sticky routing", func() {
		It("should route different categories to potentially different pods, each category sticky", func() {
			Skip("TODO: implement")

			// Test plan:
			// 1. Define two prompt categories:
			//    Category A: "Explain quantum computing in detail..."
			//    Category B: "Write a Python function to sort a list..."
			// 2. Send Category A request 3 times, record which pod serves each
			// 3. Send Category B request 3 times, record which pod serves each
			// 4. Verify: all Category A requests hit the same pod (pod-X)
			// 5. Verify: all Category B requests hit the same pod (pod-Y)
			// 6. pod-X and pod-Y may be different (prefix locality) or same
			//    (if cache has room), but each category is internally consistent
		})
	})
})
