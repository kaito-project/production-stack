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
//   - Small  (~7B qwen2.5-coder) — single GPU, single A100 node
//   - Medium (~32B qwen2.5-coder) — single 2-GPU A100 node (tensor parallel)
//   - Large  (~32B qwen2.5-coder) — two-node A100 InferenceSet deployment
//
// KAITO's Karpenter flow for InferenceSet:
//
//	InferenceSet → KAITO creates per-replica child Workspaces
//	            → each Workspace gets a Karpenter NodePool
//	            → Karpenter creates NodeClaims from the NodePool
//	            → Azure VMs provisioned (~15 min)
//	            → container image pulled on fresh node (~5 min)
//	            → vLLM loads model into GPU (~5–30 min depending on model size)
//	            → EPP registers pod → Gateway serves HTTP 200
//	            → workload removed → KAITO deletes NodePool → Karpenter
//	              deprovisions nodes (scale-back-to-zero)
//
// Each scenario is registered as its own top-level Describe so that Ginkgo
// treats them as completely independent spec trees. A nested-loop approach
// with a single outer Describe causes Ginkgo to assign all specs to the same
// proc (inner Ordered containers share the parent's proc affinity), making a
// BeforeAll failure in Small cascade-skip Medium and Large. Top-level
// Describes have no shared proc affinity.
//
// QUOTA NOTE: all three scenarios run on the A100 (NCADS_A100_v4) family.
//
//	A100 (NCADS_A100_v4) family limit 288 vCPUs:
//	  Small  1× NC24ads_A100_v4 = 24 vCPUs
//	  Medium 1× NC48ads_A100_v4 = 48 vCPUs
//	  Large  2× NC24ads_A100_v4 = 48 vCPUs
//
// The Medium scenario runs the curated/ungated qwen2.5-coder-32b-instruct on
// a single 2-GPU NC48ads_A100_v4 node (160GB total); the model fits one node,
// so KAITO shards it across both GPUs with tensor parallelism (TP=2) instead
// of provisioning a second node. The Large scenario runs the same preset on
// 1-GPU NC24ads_A100_v4 nodes (80GB each); KAITO derives the node count from
// the preset's total GPU-memory requirement (weights + KV cache + runtime
// overhead) divided by the 80GB per-node capacity, which for this model on a
// single-GPU SKU resolves to two nodes, exercising the multi-node distributed
// inference path. No HuggingFace token is required for either.
// --procs=1 is still recommended so a BeforeAll failure in one scenario does
// not cascade-skip the others (see proc-affinity note above) and to keep peak
// VM demand predictable:
//
//	E2E_LABEL='Karpenter' E2E_PARALLEL=1 E2E_TIMEOUT=180m make test-e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// nodeInstanceTypeLabel is the well-known Kubernetes label carrying the VM
// SKU on every AKS node. It is used to assert that Karpenter provisioned the
// exact instance type each scenario requested (e.g. NC48 for Medium, NC24 for
// Large).
const nodeInstanceTypeLabel = "node.kubernetes.io/instance-type"

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
		description:    "Small (~7B qwen2.5-coder, single GPU / single node)",
		minGPUsPerNode: 1,
		expectedNodes:  1,
	},
	{
		caseName:       CaseKarpenterMedium,
		description:    "Medium (~32B qwen2.5-coder, single 2-GPU A100 node, TP=2)",
		minGPUsPerNode: 2,
		expectedNodes:  1,
	},
	{
		caseName:       CaseKarpenterLarge,
		description:    "Large (~32B qwen2.5-coder, two-node A100 InferenceSet)",
		minGPUsPerNode: 1,
		expectedNodes:  2,
	},
}

func registerKarpenterScenario(s karpenterScenario) {
	Describe("Karpenter GPU Provisioning "+s.description, utils.GinkgoLabelKarpenter, utils.GinkgoLabelNightly, utils.GinkgoLabelRouting, Ordered, func() {
		// caseDeployments and caseNamespace are derived from the static
		// CaseDeployments table — safe to evaluate at tree-build time.
		caseDeployments := CaseDeployments[s.caseName]
		caseNamespace := CaseNamespace(s.caseName)

		// expectedInstanceTypes is the set of VM SKUs the case's deployments
		// requested. Every provisioned GPU node must advertise one of them via
		// the node.kubernetes.io/instance-type label — this is what
		// distinguishes the Medium (NC48, 2-GPU) scenario from the Large
		// (NC24, 1-GPU) scenario at the infrastructure level.
		expectedInstanceTypes := make(map[string]struct{})
		for _, d := range caseDeployments {
			if d.InstanceType != "" {
				expectedInstanceTypes[d.InstanceType] = struct{}{}
			}
		}

		var (
			// gatewayURL is populated in BeforeAll by InstallCase.
			gatewayURL string
			// provisionedNodeNames holds the distinct GPU nodes that hosted
			// the inference pods, captured by the provisioning It block and
			// reused by the scale-to-zero block to assert the Node objects
			// themselves are eventually removed.
			provisionedNodeNames []string
			// uninstalled tracks whether the scale-to-zero It block
			// already tore down the case so AfterAll skips the redundant
			// UninstallCase call.
			uninstalled bool
		)

		BeforeAll(func() {
			gatewayURL = InstallCase(s.caseName)
		})

		AfterAll(func() {
			if !uninstalled {
				UninstallCase(s.caseName)
			}
		})

		// ── 1. Node provisioning ─────────────────────────────────────────────
		// Verify Karpenter provisioned exactly s.expectedNodes distinct GPU
		// nodes, each one Ready, advertising the requested VM SKU, and
		// carrying at least s.minGPUsPerNode in nvidia.com/gpu capacity. By
		// the time BeforeAll completes the inference pods are already Running
		// (SetupInferenceSetsWithRouting blocks until pods are Running), so
		// the NodeName field is set.
		It("should provision the expected number of Ready GPU nodes with the requested SKU and GPU capacity", func() {
			ctx := context.Background()
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			// Collect distinct node names from inference pods across all
			// deployments in this case.
			nodeNames := make(map[string]struct{})
			for _, d := range caseDeployments {
				pods, err := clientset.CoreV1().Pods(caseNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: d.InferencePodSelector(),
				})
				Expect(err).NotTo(HaveOccurred(),
					"failed to list inference pods for deployment %s", d.Name)

				podItems := pods.Items
				Expect(podItems).NotTo(BeEmpty(),
					"no inference pods found for deployment %s in %s", d.Name, caseNamespace)
				for _, pod := range podItems {
					Expect(pod.Spec.NodeName).NotTo(BeEmpty(),
						"pod %s has not been scheduled to a node yet", pod.Name)
					nodeNames[pod.Spec.NodeName] = struct{}{}
				}
			}

			// Build a sorted slice for readable failure messages.
			sortedNames := make([]string, 0, len(nodeNames))
			for n := range nodeNames {
				sortedNames = append(sortedNames, n)
			}
			sort.Strings(sortedNames)

			Expect(sortedNames).To(HaveLen(s.expectedNodes),
				"scenario %q: expected %d distinct GPU node(s), got %d: %v",
				s.caseName, s.expectedNodes, len(sortedNames), sortedNames)

			// Verify that every hosting node is Ready, advertises the
			// requested SKU, and exposes enough GPU capacity.
			for _, nodeName := range sortedNames {
				node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "failed to get node %s", nodeName)

				// Node must have joined the cluster and be Ready.
				nodeReady := false
				for _, c := range node.Status.Conditions {
					if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
						nodeReady = true
						break
					}
				}
				Expect(nodeReady).To(BeTrue(),
					"node %s hosting an inference pod is not Ready", nodeName)

				// Node must advertise one of the SKUs the case requested,
				// proving Karpenter honoured the InferenceSet instanceType.
				if len(expectedInstanceTypes) > 0 {
					sku := node.Labels[nodeInstanceTypeLabel]
					Expect(sku).NotTo(BeEmpty(),
						"node %s has no %s label", nodeName, nodeInstanceTypeLabel)
					Expect(expectedInstanceTypes).To(HaveKey(sku),
						"node %s SKU %q is not one of the requested instance types for scenario %q",
						nodeName, sku, s.caseName)
				}

				gpuQty, ok := node.Status.Capacity[corev1.ResourceName("nvidia.com/gpu")]
				Expect(ok).To(BeTrue(),
					"node %s has no nvidia.com/gpu capacity — is it a GPU node?", nodeName)

				gpuCount, ok := gpuQty.AsInt64()
				Expect(ok).To(BeTrue(),
					"could not parse nvidia.com/gpu quantity on node %s", nodeName)
				Expect(gpuCount).To(BeNumerically(">=", s.minGPUsPerNode),
					"node %s: expected >= %d GPUs (scenario %q), got %d",
					nodeName, s.minGPUsPerNode, s.caseName, gpuCount)

				GinkgoWriter.Printf("[%s] node %s: sku=%s nvidia.com/gpu=%d ready=%t\n",
					s.caseName, nodeName, node.Labels[nodeInstanceTypeLabel], gpuCount, nodeReady)
			}

			// Record the hosting nodes so the scale-to-zero block can assert
			// these exact Node objects are removed after teardown.
			provisionedNodeNames = sortedNames
		})

		// ── 1b. NodeClaim readiness ──────────────────────────────────────────
		// KAITO drives provisioning through Karpenter: each per-replica
		// Workspace owns a NodePool from which Karpenter carves NodeClaims.
		// Assert that exactly s.expectedNodes NodeClaims exist for this case
		// (name-prefixed by the case namespace) and that every one reports a
		// Ready=True status condition — i.e. the claim launched a VM and
		// registered its Node, rather than being stuck on quota/capacity.
		It("should back the provisioned nodes with Ready Karpenter NodeClaims", func() {
			ctx := context.Background()
			dynClient, err := utils.GetDynamicClient()
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				ncList, err := dynClient.Resource(utils.NodeClaimGVR).List(ctx, metav1.ListOptions{})
				g.Expect(err).NotTo(HaveOccurred())

				var caseClaims []unstructured.Unstructured
				for _, nc := range ncList.Items {
					if strings.HasPrefix(nc.GetName(), caseNamespace+"-") {
						caseClaims = append(caseClaims, nc)
					}
				}
				g.Expect(caseClaims).To(HaveLen(s.expectedNodes),
					"scenario %q: expected %d Karpenter NodeClaim(s) for namespace %s, got %d",
					s.caseName, s.expectedNodes, caseNamespace, len(caseClaims))

				for _, nc := range caseClaims {
					conds, found, err := unstructured.NestedSlice(nc.Object, "status", "conditions")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(found).To(BeTrue(),
						"NodeClaim %s has no status.conditions yet", nc.GetName())

					ready := false
					for _, c := range conds {
						cm, ok := c.(map[string]interface{})
						if !ok {
							continue
						}
						if cm["type"] == "Ready" && cm["status"] == "True" {
							ready = true
							break
						}
					}
					g.Expect(ready).To(BeTrue(),
						"NodeClaim %s is not Ready", nc.GetName())
				}
			}, 5*time.Minute, 15*time.Second).Should(Succeed(),
				"scenario %q: expected %d Ready NodeClaim(s) backing the provisioned nodes",
				s.caseName, s.expectedNodes)
		})

		// ── 2. Inference readiness ───────────────────────────────────────────
		// Send a real chat-completion request to each deployment through the
		// production-stack gateway and verify HTTP 200. The response's
		// "model" field is checked to confirm the EPP routed to the correct
		// InferencePool (not just that the gateway is reachable).
		It("should serve live inference requests with HTTP 200 for each deployment", func() {
			for _, d := range caseDeployments {
				d := d // capture
				By(fmt.Sprintf("Sending inference request to deployment %s (model=%s)", d.Name, d.Model))
				Eventually(func() error {
					resp, err := utils.SendChatCompletion(gatewayURL, d.Name)
					if err != nil {
						return fmt.Errorf("request to %s failed: %w", d.Name, err)
					}
					if resp.StatusCode != http.StatusOK {
						body, _ := utils.ReadResponseBody(resp)
						return fmt.Errorf("expected HTTP 200, got %d for deployment %s: %s",
							resp.StatusCode, d.Name, string(body))
					}
					parsed, err := utils.ParseChatCompletionResponse(resp)
					if err != nil {
						return fmt.Errorf("failed to parse response for %s: %w", d.Name, err)
					}
					if parsed.Model != d.Name {
						return fmt.Errorf("response model %q does not match requested deployment %q",
							parsed.Model, d.Name)
					}
					return nil
				}, 5*time.Minute, 15*time.Second).Should(Succeed(),
					"gateway should return HTTP 200 with correct model name for deployment %s", d.Name)
			}
		})

		// ── 3. Scale to zero ─────────────────────────────────────────────────
		// Remove all deployments so KAITO deletes the per-replica NodePools.
		// Karpenter then drains and terminates the underlying Azure VMs.
		// This test asserts that every NodeClaim owned by this case
		// (name-prefixed by caseNamespace) is eventually removed, confirming
		// true scale-back-to-zero with no lingering GPU VMs.
		It("should deprovision all GPU nodes after workload removal (scale to zero)", func() {
			ctx := context.Background()
			dynClient, err := utils.GetDynamicClient()
			Expect(err).NotTo(HaveOccurred())

			// Snapshot the NodeClaim names owned by this case before teardown
			// so the assertion can report which ones survive unexpectedly.
			ncListBefore, err := dynClient.Resource(utils.NodeClaimGVR).List(ctx, metav1.ListOptions{})
			Expect(err).NotTo(HaveOccurred())

			var caseNodeClaims []string
			for _, nc := range ncListBefore.Items {
				if strings.HasPrefix(nc.GetName(), caseNamespace+"-") {
					caseNodeClaims = append(caseNodeClaims, nc.GetName())
				}
			}
			Expect(caseNodeClaims).NotTo(BeEmpty(),
				"expected at least one Karpenter NodeClaim for namespace %s before teardown; "+
					"was the provisioning step skipped?", caseNamespace)

			GinkgoWriter.Printf("[%s] removing workload; NodeClaims before teardown: %v\n",
				s.caseName, caseNodeClaims)

			// Uninstall: KAITO deletes the NodePool → Karpenter begins draining.
			UninstallCase(s.caseName)
			uninstalled = true

			// Wait up to 30 minutes for all case-owned NodeClaims to be gone.
			// Karpenter's disruption controller has a default 30-second polling
			// cadence; actual VM deletion on Azure takes 5–10 minutes.
			const scaleDownTimeout = 30 * time.Minute
			Eventually(func() error {
				ncList, err := dynClient.Resource(utils.NodeClaimGVR).List(ctx, metav1.ListOptions{})
				if err != nil {
					return fmt.Errorf("failed to list NodeClaims: %w", err)
				}
				var remaining []string
				for _, nc := range ncList.Items {
					if strings.HasPrefix(nc.GetName(), caseNamespace+"-") {
						remaining = append(remaining, nc.GetName())
					}
				}
				if len(remaining) > 0 {
					return fmt.Errorf("%d NodeClaim(s) still present for %s: %v",
						len(remaining), caseNamespace, remaining)
				}
				return nil
			}, scaleDownTimeout, 30*time.Second).Should(Succeed(),
				"Karpenter should deprovision all GPU nodes for scenario %q within %s",
				s.caseName, scaleDownTimeout)

			// NodeClaim removal should cascade to the underlying Node objects.
			// Assert the exact GPU nodes that hosted the inference pods are
			// gone from the cluster, confirming no orphaned Node registrations
			// (and thus no lingering billable VMs) survive teardown.
			if len(provisionedNodeNames) > 0 {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				Eventually(func() error {
					var lingering []string
					for _, nodeName := range provisionedNodeNames {
						_, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
						if err == nil {
							lingering = append(lingering, nodeName)
							continue
						}
						if !apierrors.IsNotFound(err) {
							return fmt.Errorf("failed to get node %s: %w", nodeName, err)
						}
					}
					if len(lingering) > 0 {
						return fmt.Errorf("%d GPU node(s) still registered for %s: %v",
							len(lingering), caseNamespace, lingering)
					}
					return nil
				}, scaleDownTimeout, 30*time.Second).Should(Succeed(),
					"Karpenter should remove all hosting Node objects for scenario %q within %s",
					s.caseName, scaleDownTimeout)
			}

			GinkgoWriter.Printf("[%s] scale-to-zero confirmed: all NodeClaims and Nodes removed\n", s.caseName)
		})
	})
}

// Register each scenario as an independent top-level Describe so failures in
// one scenario do not cascade-skip siblings.
var _ = func() bool {
	for i := range karpenterScenarios {
		registerKarpenterScenario(karpenterScenarios[i])
	}
	return true
}()
