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

// Model-based routing tests verify that inference requests containing a specific
// model name in the request body are forwarded to the correct model's pods and
// the response returns the same model name.
//
// Validation approach:
//   - Send POST /v1/chat/completions with {"model": "<model-name>", ...}
//   - Check the response JSON "model" field matches the requested model name
//
// Prerequisites (deployed on the test cluster):
//   - Istio Gateway with Body-Based Routing (BBR) configured
//   - At least two KAITO InferenceSets serving different models
//   - GPU node mocker creating shadow pods with llm-d-inference-sim

var _ = Describe("Model-Based Routing", utils.GinkgoLabelRouting, func() {

	Context("Single model request", func() {
		It("should return the same model name in response as specified in the request for model-a", func() {
			Skip("TODO: implement")

			// Test plan:
			// 1. Send POST /v1/chat/completions to the Gateway with body:
			//    {"model": "model-a", "messages": [{"role": "user", "content": "hello"}]}
			// 2. Parse the response JSON
			// 3. Expect response.model == "model-a"
		})

		It("should return the same model name in response as specified in the request for model-b", func() {
			Skip("TODO: implement")

			// Test plan:
			// 1. Send POST /v1/chat/completions with {"model": "model-b", ...}
			// 2. Expect response.model == "model-b"
		})
	})

	Context("Cross-model isolation", func() {
		It("should consistently return the correct model name across multiple requests", func() {
			Skip("TODO: implement")

			// Test plan:
			// 1. Send N requests for model-a, collect response.model from each
			// 2. Send N requests for model-b, collect response.model from each
			// 3. Verify all model-a responses have model == "model-a"
			// 4. Verify all model-b responses have model == "model-b"
			// 5. No cross-contamination between model pools
		})
	})
})
