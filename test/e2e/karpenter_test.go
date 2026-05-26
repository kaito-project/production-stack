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

// Package e2e contains end-to-end tests for the KAITO production-stack.
//
// karpenter_test.go exercises Karpenter-driven GPU node provisioning on Azure
// (AKS NAP) across three model-size classes:
//
//   - Small  (phi-4-mini) — single A10 node, baked model image, fast startup
//   - Medium (~32B Qwen)  — 2-GPU A100 node, tensor-parallel on one instance
//   - Large  (2× ~32B)    — two separate A100 nodes, validates multi-node
//     Karpenter provisioning
//
// KAITO's Karpenter flow for InferenceSet:
//
//	InferenceSet → KAITO creates per-replica child Workspaces
//	            → each Workspace gets a Karpenter NodePool
//	            → Karpenter creates NodeClaims from the NodePool
//	            → Azure VMs provisioned (~15 min)
//	            → container image pulled on fresh node (~5 min)
//	            → vLLM loads model into GPU (~5 min for small, 15+ for large)
//	            → EPP registers pod → Gateway serves HTTP 200
//
// Total wall-clock per scenario: 30–90 min. Run with E2E_PARALLEL=3 so all
// three scenarios execute concurrently; the critical path is then the longest
// single scenario (~90 min) rather than the serial sum (~3 h).
//
// These tests are tagged [Nightly, Karpenter] and are excluded from the
// GPU-mocker PR tier via the !GPUMocker label filter.

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// karpenterScenario holds per-scenario metadata used in assertions.
type karpenterScenario struct {
	// caseName is the key into CaseDeployments.
	caseName string
	// description is used as the Describe label.
	description string
	// minGPUsPerNode is the minimum nvidia.com/gpu capacity expected on
	// every node that hosts an inference pod.
	minGPUsPerNode int64
	// expectedNodes is the number of distinct GPU nodes Karpenter must
	// provision for the deployment. 1 for small/medium, 2 for large.
	expectedNodes int
}

var karpenterScenarios = []karpenterScenario{
	{
		caseName:       CaseKarpenterSmall,
		description:    "Small (phi-4-mini, single A10 node)",
		minGPUsPerNode: 1,
		expectedNodes:  1,
	},
	{
		caseName:       CaseKarpenterMedium,
		description:    "Medium (~32B, multi-GPU, single node)",
		minGPUsPerNode: 2,
		expectedNodes:  1,
	},
	{
		caseName:       CaseKarpenterLarge,
		description:    "Large (2× 32B replicas, Karpenter multi-node)",
		minGPUsPerNode: 2,
		expectedNodes:  2,
	},
}

var _ = Describe("Karpenter GPU Provisioning", utils.GinkgoLabelKarpenter, utils.GinkgoLabelNightly, func() {
	for i := range karpenterScenarios {
		s := karpenterScenarios[i] // capture loop variable

		Describe(s.description, Ordered, func() {
			var (
				ctx        context.Context
				gatewayURL string
				deployment utils.ModelDeploymentValues

				// podNodes holds the unique node names on which inference
				// pods ran at the end of BeforeAll (after full readiness).
				podNodes []string

				// addedNodeClaims is the count of new Karpenter NodeClaims
				// that appeared between BeforeAll start and end.
				addedNodeClaims int
			)

			BeforeAll(func() {
				ctx = context.Background()
				deployment = CaseDeployments[s.caseName][0]

				// ── Snapshot Karpenter NodeClaims before deployment ────────
				dynClient, err := utils.GetDynamicClient()
				Expect(err).NotTo(HaveOccurred())
				beforeNCList, err := dynClient.Resource(utils.NodeClaimGVR).List(ctx, metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				beforeNCCount := len(beforeNCList.Items)
				GinkgoWriter.Printf("[%s] NodeClaims before deploy: %d\n", s.caseName, beforeNCCount)

				// ── Install the full production-stack for this case ────────
				// InstallCase calls SetupInferenceSetsWithRouting which:
				//   1. Installs modelharness (Gateway) and modeldeployment charts
				//   2. Waits for EPP pods to be Running
				//   3. Waits for inference pods to be Running (PodReadyTimeout)
				//   4. Waits for gateway to serve HTTP 200 (GatewayWarmupTimeout)
				// For Karpenter cases these timeouts are set to 30–90 minutes
				// in CaseDeployments to cover Azure VM provisioning + model load.
				By(fmt.Sprintf("[%s] Installing case (this may take 30–90 min for Karpenter provisioning)", s.caseName))
				gatewayURL = InstallCase(s.caseName)

				// ── Collect inference pod → node mapping ──────────────────
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())
				pods, err := clientset.CoreV1().Pods(deployment.Namespace).List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", deployment.Name),
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(pods.Items).NotTo(BeEmpty(),
					"expected at least one inference pod for deployment %s", deployment.Name)
				nodeSet := map[string]struct{}{}
				for _, pod := range pods.Items {
					if pod.Spec.NodeName != "" {
						nodeSet[pod.Spec.NodeName] = struct{}{}
					}
				}
				for name := range nodeSet {
					podNodes = append(podNodes, name)
				}
				sort.Strings(podNodes)
				GinkgoWriter.Printf("[%s] Inference pods Running on nodes: %v\n", s.caseName, podNodes)

				// ── Count new NodeClaims created during deployment ─────────
				afterNCList, err := dynClient.Resource(utils.NodeClaimGVR).List(ctx, metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				addedNodeClaims = len(afterNCList.Items) - beforeNCCount
				GinkgoWriter.Printf("[%s] New NodeClaims created: %d\n", s.caseName, addedNodeClaims)
			})

			AfterAll(func() {
				UninstallCase(s.caseName)
			})

			// ── N1: Karpenter provisioned the right number of real GPU nodes ──
			It(fmt.Sprintf("N1: Karpenter provisioned %d real GPU node(s) of the expected instance type",
				s.expectedNodes), func() {

				Expect(podNodes).To(HaveLen(s.expectedNodes),
					"[%s] expected %d distinct node(s) hosting inference pods, got %v",
					s.caseName, s.expectedNodes, podNodes)

				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				for _, nodeName := range podNodes {
					By(fmt.Sprintf("Inspecting node %s", nodeName))
					node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())

					// Must NOT be a GPU-mocker fake node.
					Expect(node.Labels).NotTo(HaveKey("kaito.sh/fake-node"),
						"node %s must not be a GPU mocker fake node", nodeName)

					// Must match the expected Azure VM SKU.
					Expect(node.Labels).To(HaveKeyWithValue(
						"node.kubernetes.io/instance-type", deployment.InstanceType),
						"node %s must be instance type %s", nodeName, deployment.InstanceType)

					// Must advertise GPU resources.
					gpuQty := node.Status.Capacity[corev1.ResourceName("nvidia.com/gpu")]
					gpuCount := gpuQty.Value()
					Expect(gpuCount).To(BeNumerically(">=", s.minGPUsPerNode),
						"node %s must have >= %d GPU(s); capacity shows %d",
						nodeName, s.minGPUsPerNode, gpuCount)

					GinkgoWriter.Printf("[%s] node=%s instance-type=%s nvidia.com/gpu=%d\n",
						s.caseName, nodeName, deployment.InstanceType, gpuCount)
				}
			})

			// ── N2: Karpenter created NodeClaim(s) for the deployment ─────────
			It("N2: Karpenter created at least one NodeClaim during deployment", func() {
				Expect(addedNodeClaims).To(BeNumerically(">=", s.expectedNodes),
					"[%s] expected >= %d new NodeClaim(s), saw %d added during deployment",
					s.caseName, s.expectedNodes, addedNodeClaims)
			})

			// ── I1: Live inference request succeeds through the gateway ───────
			It("I1: gateway serves a live inference request with a valid response", func() {
				By(fmt.Sprintf("Sending chat completion to gateway for model=%s", deployment.Name))
				resp, err := utils.SendChatCompletionWithPrompt(
					gatewayURL, deployment.Name, "What is 2 + 2? Answer in one word.")
				Expect(err).NotTo(HaveOccurred())
				defer resp.Body.Close()

				Expect(resp.StatusCode).To(Equal(http.StatusOK),
					"[%s] expected HTTP 200 from gateway for deployment %s", s.caseName, deployment.Name)

				body, err := utils.ReadResponseBody(resp)
				Expect(err).NotTo(HaveOccurred())
				bodyStr := string(body)

				// Verify an OpenAI-compatible response shape.
				Expect(bodyStr).To(ContainSubstring(`"choices"`),
					"[%s] response must contain choices field; got: %s", s.caseName, bodyStr)
				Expect(bodyStr).To(ContainSubstring(`"message"`),
					"[%s] response must contain message field; got: %s", s.caseName, bodyStr)

				GinkgoWriter.Printf("[%s] response (first 512 chars): %.512s\n", s.caseName, bodyStr)
			})
		})
	}
})
