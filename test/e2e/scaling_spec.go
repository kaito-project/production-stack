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
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Scaling tests target a single model to keep assertions deterministic.
// Scale-Up / Scale-Down are correctness properties of one pool at a time.
//
// The deployment Name / Namespace are resolved per-Describe from the
// CaseDeployments table (see cases.go) — that table is the single source
// of truth for the model name carried in OpenAI requests and matched by
// the gateway. The shared gateway request helpers authenticate both probes
// and LoadGenerator traffic using this namespace's backend credentials.
//
// The specs run unchanged on the GPU node mocker and on real GPU nodes, so
// they observe a scale event only through objects both environments share:
// the InferenceSet's model pods, the nodes those pods run on, and the
// Karpenter NodeClaims backing those nodes (see utils/scaling.go).

const (
	// Concurrency for queue-pressure workloads. CaseScaling runs 1 replica
	// with max-num-seqs=5 (scalingVLLMConfig), so 5 requests run and the
	// rest queue. The EPP saturation detector and KAITO's vLLM rate limiter
	// (429 once waiting > max-num-seqs) cap the per-pod queue around 5,
	// which still sits above the gauge up-threshold=3, so scale-up fires.
	scalingPressureConcurrency = 40

	// Concurrency for below-threshold workload. It fits within
	// max-num-seqs, so the queue stays strictly below the gauge
	// down-threshold=1 and scale-down converges back to the baseline.
	scalingSubThresholdConcurrency = 4

	// Longer, back-to-back generations keep the pool active during scale-down
	// without filling the five running slots or building a waiting queue.
	scalingBackgroundMaxTokens = 1024

	// Bounds every completion so request latency on a real GPU stays short
	// and the load shape matches the simulator's.
	scalingMaxTokens = 128

	// Real GPU nodes are provisioned on demand: an H100 node takes ~7m to
	// join, then the model image is pulled and weights are loaded.
	scalingProvisionTimeout = 30 * time.Minute

	// Upper bound for Karpenter to delete a NodeClaim and its node after
	// the workload that needed it is gone.
	scalingNodeRemovalTimeout = 30 * time.Minute

	// Upper bound for a model pod to terminate after KAITO removes it.
	scalingPodRemovalTimeout = 10 * time.Minute

	// Margin applied on top of the KEDA-derived Eventually timeouts.
	scalingEndpointTimeout = 1 * time.Minute

	scaleToZeroProvisioningMargin  = 2 * time.Minute
	scaleToZeroColdLoadConcurrency = 16

	// scalingVLLMConfigName is the KAITO inference ConfigMap CaseScaling
	// references; installScalingCase creates it before the InferenceSet.
	scalingVLLMConfigName = "scaling-vllm-config"
)

// scalingVLLMConfig caps concurrent sequences per pod so a modest workload
// builds a waiting queue even on an H100.
var scalingVLLMConfig = map[string]string{
	"inference_config.yaml": "vllm:\n  max-num-seqs: 5\n",
}

var _ = Describe("InferenceSet Scaling — Infra",
	Ordered, utils.GinkgoLabelScaling, utils.GinkgoLabelStandardK8sOnly, func() {

		// Per-case deployment owned by scaling_spec.go (see cases.go).
		// Resolved as Describe-local values so the table lookup happens
		// inside the suite (matching the pattern in model_routing_spec.go),
		// not at package init.
		caseDeployments := CaseDeployments[CaseScaling]
		scalingValues := caseDeployments[0]
		scalingModel := scalingValues.Name
		scalingNamespace := CaseNamespace(CaseScaling)

		var (
			// gateway routes into the CaseScaling deployment's Gateway.
			// Resolved in the outer BeforeAll once per suite run so all
			// nested Describes (Scale-Up → Scale-Down, Anti-Flapping) share it.
			gateway deploy.GatewayEndpoint
			// NodeClaims that existed before the case was installed; any
			// other NodeClaim must be gone after teardown.
			nodeClaimsBeforeInstall []string
		)

		BeforeAll(func() {
			ctx := context.Background()
			nodeClaimsBeforeInstall = snapshotNodeClaimNames(ctx)
			gateway = installScalingCase(ctx)
		})

		AfterAll(func() {
			UninstallCase(CaseScaling)
			if nodeClaimsBeforeInstall != nil {
				expectNoNewNodeClaims(context.Background(), nodeClaimsBeforeInstall, scalingNodeRemovalTimeout)
			}
		})

		// Baseline state shared by the Ordered Scale-Up → Scale-Down flow.
		type baseline struct {
			replicas int32
			// pods maps each Ready model pod to its node.
			pods map[string]string
			keda utils.KEDAParams
		}

		Describe("Scale 0 → 1 → 2 → 1 → 0 End-to-End",
			Ordered, utils.GinkgoLabelScaleToZero, func() {

				var (
					ctx             context.Context
					base            baseline
					bulkLoad        *utils.LoadGenerator
					lowLoad         *utils.LoadGenerator
					coldLoad        *utils.LoadGenerator
					parked          bool
					lifecycleValues deploy.ModelDeploymentValues
					// State carried across Its.
					newPod string
					// Every model pod seen during the flow → its node, and
					// node → backing NodeClaim. Which replica KAITO removes
					// on scale-down is not fixed, so F1 derives the victim
					// from these.
					seenPods     map[string]string
					claimsByNode map[string]string
				)

				BeforeAll(func() {
					ctx = context.Background()
					values := scalingValues
					values.MinReplicas = 0
					values.MaxReplicas = 1
					values.CooldownPeriod = 60
					initialCooldown := int64(60)
					values.InitialCooldownPeriod = &initialCooldown
					values.ScalingMetrics = slices.Clone(values.ScalingMetrics)
					values.ScalingMetrics[0].Source = "modelpod"
					values.ScalingMetrics[0].DeactivationThreshold = "0"
					values.ScalingMetrics = append(values.ScalingMetrics,
						deploy.ScalingMetric{
							Name: "llm_d_epp_flow_control_queue_size", Type: "gauge", Source: "epp",
							ActivationThreshold: "10", DeactivationThreshold: "0",
						},
						deploy.ScalingMetric{Name: "vllm:num_requests_running", Type: "gauge", Source: "modelpod", DeactivationThreshold: "0"},
					)
					lifecycleValues = values
					Expect(utils.UpgradeModelDeployment(ctx, values)).To(Succeed())
					Eventually(func(g Gomega) {
						dynClient, err := utils.GetDynamicClient()
						g.Expect(err).NotTo(HaveOccurred())
						scaledObject, err := dynClient.Resource(utils.ScaledObjectGVR).Namespace(scalingNamespace).
							Get(ctx, scalingModel, metav1.GetOptions{})
						g.Expect(err).NotTo(HaveOccurred())
						minReplicas, found, err := unstructured.NestedInt64(scaledObject.Object, "spec", "minReplicaCount")
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(found).To(BeTrue())
						g.Expect(minReplicas).To(Equal(int64(0)))
						maxReplicas, found, err := unstructured.NestedInt64(scaledObject.Object, "spec", "maxReplicaCount")
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(found).To(BeTrue())
						g.Expect(maxReplicas).To(Equal(int64(1)))
						g.Expect(scaledObjectReady(scaledObject.Object)).To(BeTrue())
					}, 2*time.Minute, 5*time.Second).Should(Succeed())
					var err error
					base.keda, err = utils.GetKEDAParams(ctx, scalingModel, scalingNamespace)
					Expect(err).NotTo(HaveOccurred())
				})

				It("parks the warm model and releases its GPU NodeClaim while EPP stays ready", func() {
					Eventually(func() (int32, error) {
						return utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
					}, max(base.keda.InitialCooldownPeriod, base.keda.CooldownPeriod)+scaleToZeroProvisioningMargin, 5*time.Second).
						Should(Equal(int32(0)))
					expectReadyModelPods(ctx, scalingValues, 0, scalingPodRemovalTimeout)
					Expect(assertEPPReady(ctx, scalingNamespace, scalingModel)).To(Succeed())
					expectNoNewNodeClaims(ctx, nodeClaimsBeforeInstall, scalingNodeRemovalTimeout)
				})

				It("wakes from sustained EPP demand and serves traffic at one replica", func() {
					coldLoad = &utils.LoadGenerator{
						Gateway: gateway, Model: scalingModel, Prompt: "hello",
						Concurrency: scaleToZeroColdLoadConcurrency, MaxTokens: scalingMaxTokens,
					}
					coldLoad.Start(ctx)
					Eventually(func() (int32, error) {
						return utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
					}, base.keda.ScaleUpTotalWait+scaleToZeroProvisioningMargin, 5*time.Second).Should(Equal(int32(1)))
					Eventually(func(g Gomega) {
						replicas, err := utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
						g.Expect(err).NotTo(HaveOccurred())
						if replicas == 0 {
							StopTrying("InferenceSet parked during cold-start provisioning").Now()
						}
						g.Expect(utils.WaitForInferenceSetReady(ctx, scalingValues, 10*time.Second)).To(Succeed())
					}, scalingProvisionTimeout, 5*time.Second).Should(Succeed())
					Eventually(func() int64 {
						return coldLoad.Stats().Success
					}, 2*time.Minute, time.Second).Should(BeNumerically(">", 0))

					By("Starting one continuous background worker before stopping cold-start pressure")
					lowLoad = &utils.LoadGenerator{
						Gateway:     gateway,
						Model:       scalingModel,
						Prompt:      "Write a detailed tutorial about how computers work, covering processors, memory, storage, networking, and operating systems. Explain each topic thoroughly with examples and continue for at least 1500 words.",
						Concurrency: 1,
						MaxTokens:   scalingBackgroundMaxTokens,
					}
					lowLoad.Start(ctx)
					coldLoad.Stop()
					handoffStarted := time.Now()
					stats := coldLoad.Stats()
					GinkgoWriter.Printf("cold-start load through handoff: %+v\n", stats)
					Expect(stats.Success).To(BeNumerically(">", 0))
					Expect(stats.Errors5xx).To(BeNumerically("<=", 3*scaleToZeroColdLoadConcurrency), "cold-start load: %+v", stats)

					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())
					By("Draining cold-start backlog and verifying KEDA observes the background workload before probing")
					Eventually(func(g Gomega) {
						replicas, err := utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
						g.Expect(err).NotTo(HaveOccurred())
						if replicas == 0 {
							StopTrying("InferenceSet parked during the cold-to-background load handoff").Now()
						}
						g.Expect(replicas).To(Equal(int32(1)))
						g.Expect(lowLoad.Stats().Success).To(BeNumerically(">", 0))
						waiting, err := utils.ScrapeModelPodMetric(ctx, clientset, scalingValues, "vllm:num_requests_waiting")
						g.Expect(err).NotTo(HaveOccurred())
						for pod, count := range waiting {
							g.Expect(count).To(BeZero(), "cold-start backlog must drain on pod %s", pod)
						}
						dynClient, err := utils.GetDynamicClient()
						g.Expect(err).NotTo(HaveOccurred())
						so, err := dynClient.Resource(utils.ScaledObjectGVR).Namespace(scalingNamespace).
							Get(ctx, scalingModel, metav1.GetOptions{})
						g.Expect(err).NotTo(HaveOccurred())
						lastActive, found, err := unstructured.NestedString(so.Object, "status", "lastActiveTime")
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(found).To(BeTrue(), "KEDA must record active demand")
						activeAt, err := time.Parse(time.RFC3339, lastActive)
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(activeAt.After(handoffStarted)).To(BeTrue(),
							"lastActiveTime=%s must advance after cold-start pressure stops", lastActive)
						g.Expect(utils.CheckChatSuccess(ctx, gateway, scalingModel, utils.WithMaxTokens(scalingMaxTokens))).To(Succeed())
					}, 2*time.Minute, 5*time.Second).Should(Succeed())
				})

				It("records the reactivated baseline before adding capacity", func() {
					ctx = context.Background()
					var err error

					By("Reading effective KEDA parameters from the ScaledObject")
					base.keda, err = utils.GetKEDAParams(ctx, scalingModel, scalingNamespace)
					Expect(err).NotTo(HaveOccurred(),
						"ScaledObject for %s must exist (CaseScaling.EnableScaling must be true)", scalingModel)
					Expect(base.keda.Threshold).To(BeNumerically(">", 0),
						"KEDA threshold must be set")
					GinkgoWriter.Printf(
						"KEDA params: threshold=%d polling=%s cooldown=%s initialCooldown=%s upPeriod=%s downPeriod=%s upStab=%s downStab=%s "+
							"→ scaleUpWait=%s scaleDownWait=%s\n",
						base.keda.Threshold, base.keda.PollingInterval, base.keda.CooldownPeriod,
						base.keda.InitialCooldownPeriod, base.keda.ScaleUpRatePeriod, base.keda.ScaleDownRatePeriod,
						base.keda.ScaleUpStabilization, base.keda.ScaleDownStabilization,
						base.keda.ScaleUpTotalWait, base.keda.ScaleDownTotalWait)

					By("Capturing baseline replicas and model pod placement")
					base.replicas, err = utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
					Expect(err).NotTo(HaveOccurred())
					Expect(base.replicas).To(Equal(int32(1)),
						"cold-start activation must leave %s at one replica", scalingModel)
					GinkgoWriter.Printf("Baseline replicas for %s: %d\n", scalingModel, base.replicas)

					base.pods = expectReadyModelPods(ctx, scalingValues, int(base.replicas), scalingEndpointTimeout)
					seenPods = maps.Clone(base.pods)
					claimsByNode = currentNodeClaimsByNode(ctx)
					for pod, node := range base.pods {
						Expect(claimsByNode).To(HaveKey(node),
							"baseline pod %s runs on node %s, which must be backed by a NodeClaim", pod, node)
					}
					GinkgoWriter.Printf("Baseline model pods → nodes: %v\n", base.pods)

					// keda-kaito-scaler only scales up while readyReplicas >= spec.replicas.
					By("Waiting for InferenceSet readyReplicas to match the baseline (KEDA readiness gate)")
					Eventually(func() (int32, error) {
						return utils.GetInferenceSetReadyReplicas(ctx, scalingModel, scalingNamespace)
					}, scalingEndpointTimeout, 5*time.Second).Should(BeNumerically(">=", base.replicas),
						"InferenceSet %s readyReplicas must reach %d or KEDA cannot scale up; "+
							"check the child Workspace WorkspaceSucceeded condition", scalingModel, base.replicas)

				})

				AfterAll(func() {
					if coldLoad != nil {
						coldLoad.Stop()
					}
					if bulkLoad != nil {
						bulkLoad.Stop()
					}
					if lowLoad != nil {
						lowLoad.Stop()
					}
					// If a failed It left the InferenceSet at an elevated
					// replica count, restore it so subsequent suites (or
					// the Anti-Flapping Describe) start from the same
					// baseline the chart installed.
					if base.replicas <= 0 || parked {
						return
					}
					By("Restoring baseline replicas")
					if err := utils.SetInferenceSetReplicas(
						context.Background(), scalingModel, scalingNamespace, base.replicas); err != nil {
						GinkgoWriter.Printf("warning: restore replicas: %v\n", err)
					}
				})

				It("A1: queue pressure crosses the KEDA threshold", func() {
					By(fmt.Sprintf("Starting bulk load at concurrency=%d", scalingPressureConcurrency))
					bulkLoad = &utils.LoadGenerator{
						Gateway:     gateway,
						Model:       scalingModel,
						Prompt:      "please explain the theory of relativity in as much detail as possible",
						Concurrency: scalingPressureConcurrency,
						MaxTokens:   scalingMaxTokens,
					}
					bulkLoad.Start(ctx)

					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())

					By(fmt.Sprintf("Waiting for at least one pod's vllm:num_requests_waiting > %d",
						base.keda.Threshold))
					Eventually(func(g Gomega) {
						snap, err := utils.ScrapeModelPodMetric(ctx, clientset, scalingValues, "vllm:num_requests_waiting")
						g.Expect(err).NotTo(HaveOccurred())
						var maxWaiting float64
						for _, v := range snap {
							if v > maxWaiting {
								maxWaiting = v
							}
						}
						g.Expect(maxWaiting).To(BeNumerically(">", float64(base.keda.Threshold)),
							"expected max num_requests_waiting > %d, saw %v", base.keda.Threshold, snap)
					}, base.keda.PollingInterval+60*time.Second, 3*time.Second).Should(Succeed())

					By("Allowing the second replica only after bulk queue pressure is established")
					lifecycleValues.MaxReplicas = scalingValues.MaxReplicas
					Expect(utils.UpgradeModelDeployment(ctx, lifecycleValues)).To(Succeed())
					Eventually(func(g Gomega) {
						replicas, err := utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
						g.Expect(err).NotTo(HaveOccurred())
						if replicas == 0 {
							StopTrying("InferenceSet parked while enabling capacity scaling").Now()
						}
						dynClient, err := utils.GetDynamicClient()
						g.Expect(err).NotTo(HaveOccurred())
						so, err := dynClient.Resource(utils.ScaledObjectGVR).Namespace(scalingNamespace).
							Get(ctx, scalingModel, metav1.GetOptions{})
						g.Expect(err).NotTo(HaveOccurred())
						maxReplicas, found, err := unstructured.NestedInt64(so.Object, "spec", "maxReplicaCount")
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(found).To(BeTrue())
						g.Expect(maxReplicas).To(Equal(int64(scalingValues.MaxReplicas)))
						g.Expect(scaledObjectReady(so.Object)).To(BeTrue())
					}, 2*time.Minute, 5*time.Second).Should(Succeed())
				})

				It("A2: KEDA bumps InferenceSet.spec.replicas", func() {
					expected := base.replicas + 1
					By(fmt.Sprintf("Waiting for .spec.replicas to reach %d", expected))
					Eventually(func(g Gomega) {
						replicas, err := utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
						g.Expect(err).NotTo(HaveOccurred())
						if replicas == 0 {
							StopTrying("InferenceSet parked while capacity-scaling load was active").Now()
						}
						g.Expect(replicas).To(BeNumerically(">=", expected))
					}, base.keda.ScaleUpTotalWait+60*time.Second, 5*time.Second).
						Should(Succeed(),
							"KEDA should bump replicas from %d to >=%d within %s",
							base.replicas, expected, base.keda.ScaleUpTotalWait+60*time.Second)
				})

				It("B1: a new model pod becomes Ready on a new GPU node backed by a NodeClaim", func() {
					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())
					dynClient, err := utils.GetDynamicClient()
					Expect(err).NotTo(HaveOccurred())

					baseNodes := map[string]bool{}
					for _, node := range base.pods {
						baseNodes[node] = true
					}

					var placements, byNode map[string]string
					var newNode string
					Eventually(func(g Gomega) {
						pods, err := utils.ListModelPods(ctx, clientset, scalingValues)
						g.Expect(err).NotTo(HaveOccurred())
						var candidate *corev1.Pod
						ready := utils.ReadyPods(pods)
						for i := range ready {
							if _, isBase := base.pods[ready[i].Name]; !isBase {
								candidate = &ready[i]
								break
							}
						}
						g.Expect(candidate).NotTo(BeNil(),
							"expected a Ready model pod beyond baseline %v, have %v",
							base.pods, utils.PodPlacements(pods))

						nodeName := candidate.Spec.NodeName
						g.Expect(baseNodes).NotTo(HaveKey(nodeName),
							"new pod %s must run on a newly provisioned node, not baseline node %s",
							candidate.Name, nodeName)
						node, err := clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(isNodeReady(node)).To(BeTrue(), "node %s must be Ready", nodeName)
						g.Expect(node.Labels).To(HaveKeyWithValue(corev1.LabelInstanceTypeStable, scalingValues.InstanceType),
							"node %s must be the requested instance type", nodeName)
						gpus := node.Status.Allocatable[corev1.ResourceName("nvidia.com/gpu")]
						g.Expect(gpus.Value()).To(BeNumerically(">=", 1),
							"node %s must advertise allocatable nvidia.com/gpu", nodeName)

						nodeClaims, err := utils.ListNodeClaims(ctx, dynClient)
						g.Expect(err).NotTo(HaveOccurred())
						byNode = utils.NodeClaimsByNode(nodeClaims)
						g.Expect(byNode).To(HaveKey(nodeName),
							"node %s must be backed by a NodeClaim", nodeName)

						newPod = candidate.Name
						newNode = nodeName
						placements = utils.PodPlacements(pods)
					}, scalingProvisionTimeout, 15*time.Second).Should(Succeed())

					maps.Copy(seenPods, placements)
					maps.Copy(claimsByNode, byNode)
					GinkgoWriter.Printf("New model pod %s on node %s (NodeClaim %s)\n",
						newPod, newNode, byNode[newNode])
				})

				It("C1: Ready model pods grow to match the new replica count", func() {
					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())

					Eventually(func(g Gomega) {
						current, err := utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
						g.Expect(err).NotTo(HaveOccurred())
						// Count Ready model pods directly via the Kubernetes
						// API instead of scraping the EPP /metrics endpoint. EPP
						// v1.3.x enables controller-runtime's metrics auth filter,
						// which rejects requests routed through the apiserver
						// pod-proxy (the proxy strips the Authorization header).
						// Ready pods selected by the InferenceSet are what the
						// InferencePool's `inference_pool_ready_pods` gauge counts.
						pods, err := utils.ListModelPods(ctx, clientset, scalingValues)
						g.Expect(err).NotTo(HaveOccurred())
						ready := len(utils.ReadyPods(pods))
						g.Expect(ready).To(BeNumerically(">=", int(current)),
							"Ready model pods (%d) should match replicas (%d)", ready, current)
					}, scalingEndpointTimeout+1*time.Minute, 5*time.Second).Should(Succeed())
				})

				It("D1: the new model pod actually serves traffic", func() {
					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())

					By("Scraping per-pod vllm:request_success_total")
					var lastSnap utils.PodMetricSnapshot
					Eventually(func(g Gomega) {
						snap, err := utils.ScrapeModelPodMetric(ctx, clientset, scalingValues, "vllm:request_success_total")
						g.Expect(err).NotTo(HaveOccurred())
						lastSnap = snap
						v, ok := snap[newPod]
						g.Expect(ok).To(BeTrue(),
							"new model pod %s must appear in per-pod metrics", newPod)
						g.Expect(v).To(BeNumerically(">", 0),
							"new model pod %s should have vllm:request_success_total > 0", newPod)
					}, 90*time.Second, 5*time.Second).Should(Succeed())
					GinkgoWriter.Printf("Per-pod success counters after scale-up: %+v\n", lastSnap)
				})

				It("E1: queue drains once the bulk load stops", func() {
					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())

					// Snapshot the pre-stop queue so the post-stop drain
					// assertion can be sized in terms of an actual workload
					// rather than a hard-coded 2-minute guess. We capture
					// max(num_requests_waiting) right before stopping bulk
					// load — this is the residual queue that must drain.
					By("Snapshotting pre-stop queue depth")
					var preStopMaxWaiting float64
					Eventually(func(g Gomega) {
						snap, err := utils.ScrapeModelPodMetric(ctx, clientset, scalingValues, "vllm:num_requests_waiting")
						g.Expect(err).NotTo(HaveOccurred())
						var m float64
						for _, v := range snap {
							if v > m {
								m = v
							}
						}
						// The queue must still be loaded — bulk load is
						// running at concurrency=40 against ≤2 pods × 5
						// running slots. If we don't see a non-zero queue,
						// either the model server isn't reporting the metric
						// or bulk load isn't actually pressuring the pool;
						// either way the drain assertion below would be
						// vacuous.
						g.Expect(m).To(BeNumerically(">", 0),
							"expected residual waiting>0 before Stop, saw %v", snap)
						preStopMaxWaiting = m
					}, 15*time.Second, 1*time.Second).Should(Succeed())
					GinkgoWriter.Printf("Pre-stop max waiting: %v\n", preStopMaxWaiting)

					By("Stopping bulk load generator")
					bulkLoad.Stop()
					stats := bulkLoad.Stats()
					GinkgoWriter.Printf("Bulk load stats: total=%d success=%d 5xx=%d other=%d transportErr=%d\n",
						stats.Total, stats.Success, stats.Errors5xx, stats.OtherNon2xx, stats.TransportErr)

					By("Waiting for vllm:num_requests_waiting == 0 on every Ready model pod")
					// The background worker continues generating while the waiting queue drains.
					Eventually(func(g Gomega) {
						snap, err := utils.ScrapeModelPodMetric(ctx, clientset, scalingValues, "vllm:num_requests_waiting")
						g.Expect(err).NotTo(HaveOccurred())
						for pod, v := range snap {
							g.Expect(v).To(BeNumerically("==", 0),
								"pod %s still has waiting=%v", pod, v)
						}
					}, 2*time.Minute, 5*time.Second).Should(Succeed())
				})

				It("E2: KEDA drops InferenceSet.spec.replicas back to baseline", func() {
					By(fmt.Sprintf("Waiting for .spec.replicas to return to %d (downStabilization=%s)",
						base.replicas, base.keda.ScaleDownStabilization))
					var last int32
					Eventually(func(g Gomega) {
						r, err := utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
						g.Expect(err).NotTo(HaveOccurred())
						last = r
						if r == 0 {
							StopTrying("InferenceSet parked before the background workload was stopped").Now()
						}
						g.Expect(r).To(Equal(base.replicas),
							"replicas should drop to %d, now %d", base.replicas, r)
					}, base.keda.ScaleDownTotalWait+2*time.Minute, 15*time.Second).Should(Succeed())
					_ = last
				})

				It("F1: the removed model pod, its NodeClaim, and its node are cleaned up", func() {
					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())
					dynClient, err := utils.GetDynamicClient()
					Expect(err).NotTo(HaveOccurred())

					By(fmt.Sprintf("Waiting for %d Ready model pod(s) and no others", base.replicas))
					current := expectReadyModelPods(ctx, scalingValues, int(base.replicas), scalingPodRemovalTimeout)

					// KAITO may remove either replica, so the victim is
					// whichever recorded pod is no longer current.
					hostingNodes := map[string]bool{}
					for _, node := range current {
						hostingNodes[node] = true
					}
					var victimPods, victimNodes []string
					for _, pod := range slices.Sorted(maps.Keys(seenPods)) {
						if _, ok := current[pod]; ok {
							continue
						}
						victimPods = append(victimPods, pod)
						if node := seenPods[pod]; node != "" && !hostingNodes[node] && !slices.Contains(victimNodes, node) {
							victimNodes = append(victimNodes, node)
						}
					}
					GinkgoWriter.Printf("Scale-down victims: pods=%v nodes=%v (current %v)\n", victimPods, victimNodes, current)
					Expect(victimPods).NotTo(BeEmpty(),
						"a model pod recorded during scale-up must be gone; seen=%v current=%v", seenPods, current)
					Expect(victimNodes).NotTo(BeEmpty(),
						"the removed pod's node must no longer host a model pod; seen=%v current=%v", seenPods, current)

					for _, pod := range victimPods {
						By(fmt.Sprintf("Waiting for model pod %s to be deleted", pod))
						Eventually(func() bool {
							_, err := clientset.CoreV1().Pods(scalingNamespace).Get(ctx, pod, metav1.GetOptions{})
							return apierrors.IsNotFound(err)
						}, scalingPodRemovalTimeout, 5*time.Second).Should(BeTrue(),
							"model pod %s should be deleted", pod)
					}

					for _, node := range victimNodes {
						nodeClaim := claimsByNode[node]
						Expect(nodeClaim).NotTo(BeEmpty(), "no NodeClaim recorded for node %s", node)

						By(fmt.Sprintf("Waiting for NodeClaim %s to be deleted", nodeClaim))
						Eventually(func() bool {
							_, err := dynClient.Resource(utils.NodeClaimGVR).Get(ctx, nodeClaim, metav1.GetOptions{})
							return apierrors.IsNotFound(err)
						}, scalingNodeRemovalTimeout, 15*time.Second).Should(BeTrue(),
							"NodeClaim %s should be deleted", nodeClaim)

						By(fmt.Sprintf("Waiting for node %s to be deleted", node))
						Eventually(func() bool {
							_, err := clientset.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
							return apierrors.IsNotFound(err)
						}, scalingNodeRemovalTimeout, 15*time.Second).Should(BeTrue(),
							"node %s should be deleted", node)
					}
				})

				It("F2: Ready model pods shrink to match the new replica count", func() {
					// See C1 for why Ready model pods stand in for EPP's
					// inference_pool_ready_pods gauge.
					expectReadyModelPods(ctx, scalingValues, int(base.replicas), scalingEndpointTimeout)
				})

				It("G1: remaining pods continue to serve traffic after scale-down", func() {
					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())

					beforeSnap, err := utils.ScrapeModelPodMetric(ctx, clientset, scalingValues, "vllm:request_success_total")
					Expect(err).NotTo(HaveOccurred())

					By("Driving a short moderate-rate burst strictly below threshold")
					burst := &utils.LoadGenerator{
						Gateway:     gateway,
						Model:       scalingModel,
						Prompt:      "hello",
						Concurrency: scalingSubThresholdConcurrency,
						MaxTokens:   scalingMaxTokens,
					}
					burst.Start(ctx)
					time.Sleep(15 * time.Second)
					burst.Stop()
					stats := burst.Stats()
					// The load-generator's own 5xx counter is the canonical
					// signal for "no errors". We deliberately do NOT scrape
					// EPP /metrics for `inference_extension_scheduler_attempts_total`
					// here: EPP v1.3.x rejects pod-proxy scrapes (401, see C1).
					Expect(stats.Errors5xx).To(BeNumerically("==", 0),
						"no 5xx expected post-scale-down, got stats=%+v", stats)
					Expect(stats.Success).To(BeNumerically(">", 0))

					afterSnap, err := utils.ScrapeModelPodMetric(ctx, clientset, scalingValues, "vllm:request_success_total")
					Expect(err).NotTo(HaveOccurred())
					diff := utils.DiffSnapshots(beforeSnap, afterSnap)
					var incremented int
					for _, v := range diff {
						if v > 0 {
							incremented++
						}
					}
					Expect(incremented).To(BeNumerically(">=", 1),
						"at least one remaining pod should serve traffic, diff=%+v", diff)
				})

				It("G2: continuous background stream saw no 5xx during the scale-down transition", func() {
					stats := lowLoad.Stats()
					GinkgoWriter.Printf("Continuous background stats across scale-down window: %+v\n", stats)
					Expect(stats.Errors5xx).To(BeNumerically("==", 0),
						"continuous background stream must not observe any 5xx, stats=%+v", stats)

					// Allow a handful of transport-level reconnects; be strict about server errors.
					Expect(stats.Success).To(BeNumerically(">", 0),
						"background worker must have completed successful requests, stats=%+v", stats)
				})

				It("returns to zero and releases GPU NodeClaims after all work clears", func() {
					lowLoad.Stop()
					Eventually(func() (int32, error) {
						return utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
					}, base.keda.PollingInterval+base.keda.CooldownPeriod+scaleToZeroProvisioningMargin, 5*time.Second).
						Should(Equal(int32(0)))
					expectReadyModelPods(ctx, scalingValues, 0, scalingPodRemovalTimeout)
					Expect(assertEPPReady(ctx, scalingNamespace, scalingModel)).To(Succeed())
					expectNoNewNodeClaims(ctx, nodeClaimsBeforeInstall, scalingNodeRemovalTimeout)
					parked = true
				})
			},
		)

		Describe("Anti-Flapping", func() {

			var (
				ctx  context.Context
				base struct {
					replicas   int32
					pods       map[string]string
					nodeClaims []string
					keda       utils.KEDAParams
				}
			)

			BeforeAll(func() {
				ctx = context.Background()
				Expect(utils.UpgradeModelDeployment(ctx, scalingValues)).To(Succeed())
				Expect(utils.SetInferenceSetReplicas(ctx, scalingModel, scalingNamespace, 1)).To(Succeed())
				Expect(utils.WaitForInferenceSetReady(ctx, scalingValues, scalingProvisionTimeout)).To(Succeed())
				Eventually(func() error {
					return utils.CheckChatSuccess(ctx, gateway, scalingModel, utils.WithMaxTokens(scalingMaxTokens))
				}, scalingProvisionTimeout, 10*time.Second).Should(Succeed())
			})

			BeforeEach(func() {
				ctx = context.Background()
				var err error

				base.keda, err = utils.GetKEDAParams(ctx, scalingModel, scalingNamespace)
				Expect(err).NotTo(HaveOccurred())
				base.replicas, err = utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
				Expect(err).NotTo(HaveOccurred())
				base.pods = expectReadyModelPods(ctx, scalingValues, int(base.replicas), scalingPodRemovalTimeout)
				base.nodeClaims = snapshotNodeClaimNames(ctx)
				GinkgoWriter.Printf("Anti-flapping baseline: replicas=%d pods=%v nodeClaims=%d\n",
					base.replicas, base.pods, len(base.nodeClaims))
			})

			AfterEach(func() {
				By("Restoring baseline replicas and waiting for cooldown to clear")
				if err := utils.SetInferenceSetReplicas(
					context.Background(), scalingModel, scalingNamespace, base.replicas); err != nil {
					GinkgoWriter.Printf("warning: restore replicas: %v\n", err)
				}
				// Let the HPA rate-limit policies expire before the next case.
				// (Only pays the full window when a test actually caused a scale event.)
				time.Sleep(max(base.keda.ScaleUpRatePeriod, base.keda.ScaleDownRatePeriod))
			})

			It("H1: stays at baseline when pressure is held below threshold", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())
				dynClient, err := utils.GetDynamicClient()
				Expect(err).NotTo(HaveOccurred())

				By("Running sub-threshold load")
				load := &utils.LoadGenerator{
					Gateway:     gateway,
					Model:       scalingModel,
					Prompt:      "hello",
					Concurrency: scalingSubThresholdConcurrency,
					MaxTokens:   scalingMaxTokens,
				}
				load.Start(ctx)
				defer load.Stop()

				observeWindow := max(base.keda.ScaleUpTotalWait, base.keda.ScaleDownTotalWait)
				// Every 15s confirm that nothing scaled — replicas, model pods,
				// and NodeClaims all stay at baseline — and track the queue.
				sampleEvery := 15 * time.Second
				deadline := time.Now().Add(observeWindow)
				var maxSeen float64
				for time.Now().Before(deadline) {
					snap, err := utils.ScrapeModelPodMetric(ctx, clientset, scalingValues, "vllm:num_requests_waiting")
					Expect(err).NotTo(HaveOccurred())
					for _, v := range snap {
						if v > maxSeen {
							maxSeen = v
						}
					}

					cur, err := utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
					Expect(err).NotTo(HaveOccurred())
					Expect(cur).To(Equal(base.replicas),
						"replicas must remain at baseline %d, got %d", base.replicas, cur)

					pods, err := utils.ListModelPods(ctx, clientset, scalingValues)
					Expect(err).NotTo(HaveOccurred())
					for _, pod := range pods {
						Expect(base.pods).To(HaveKey(pod.Name),
							"no model pod should be added; baseline=%v got %s", base.pods, pod.Name)
					}

					nodeClaims, err := utils.ListNodeClaims(ctx, dynClient)
					Expect(err).NotTo(HaveOccurred())
					added := utils.NewNodeClaimNames(base.nodeClaims, nodeClaims)
					Expect(added).To(BeEmpty(), "no NodeClaim should be added; got %v", added)

					time.Sleep(sampleEvery)
				}
				GinkgoWriter.Printf("Max num_requests_waiting during H1: %v (threshold=%d)\n",
					maxSeen, base.keda.Threshold)
				Expect(maxSeen).To(BeNumerically("<", float64(base.keda.Threshold)),
					"H1 must keep load strictly below threshold to be meaningful")
			})

			It("H2: HPA scale-up stabilization gates immediate re-scale-up after a scale-down", func() {
				// H2 runs an independent full Scale-Up → Scale-Down cycle, then
				// immediately re-applies pressure and verifies that:
				//   - replicas hold at baseline for the full HPA scale-up
				//     stabilization window after tDown (safety / no oscillation),
				//   - replicas DO scale up again once the window has elapsed
				//     plus any HPA rate-limit period (liveness / not stuck).
				//
				// NOTE on KEDA semantics: spec.cooldownPeriod only gates
				// scale-to-zero (it kicks in when the last trigger reports
				// inactive and minReplicaCount is 0). With minReplicaCount>=1,
				// HPA stabilization and rate-limit policies are independent gates.
				// Liveness also allows the configured scale-up rate-limit period.

				By("Scale-Up: driving pressure")
				bulk := &utils.LoadGenerator{
					Gateway:     gateway,
					Model:       scalingModel,
					Prompt:      "please explain the theory of relativity in as much detail as possible",
					Concurrency: scalingPressureConcurrency,
					MaxTokens:   scalingMaxTokens,
				}
				bulk.Start(ctx)

				target := base.replicas + 1
				Eventually(func() (int32, error) {
					return utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
				}, base.keda.ScaleUpTotalWait+90*time.Second, 5*time.Second).
					Should(BeNumerically(">=", target),
						"H2 setup: KEDA should scale up to >=%d", target)

				By("Scale-Down: stopping load and waiting for replicas to return")
				bulk.Stop()
				Eventually(func() (int32, error) {
					return utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
				}, base.keda.ScaleDownTotalWait+3*time.Minute, 15*time.Second).
					Should(Equal(base.replicas),
						"H2 setup: KEDA should scale back down to baseline %d", base.replicas)
				tDown := time.Now()
				GinkgoWriter.Printf("H2: scale-down completed at t=%s\n", tDown.Format(time.RFC3339))

				By("Immediately re-applying pressure and observing the anti-flapping window")
				post := &utils.LoadGenerator{
					Gateway:     gateway,
					Model:       scalingModel,
					Prompt:      "please explain the theory of relativity in as much detail as possible",
					Concurrency: scalingPressureConcurrency,
					MaxTokens:   scalingMaxTokens,
				}
				post.Start(ctx)
				defer post.Stop()

				// Three observation zones, anchored at tDown:
				//
				//   strict zone:      [0, antiFlapBoundary - slack)
				//                     replicas MUST stay at baseline. Any
				//                     scale-up here is a real anti-flapping
				//                     violation.
				//
				//   permissive zone:  [antiFlapBoundary - slack, antiFlapBoundary + slack)
				//                     HPA evaluates on its own cadence and
				//                     there's clock skew between this test
				//                     process and the apiserver/HPA, so a
				//                     scale-up may legitimately land anywhere
				//                     in this band. We neither require nor
				//                     forbid scale-up here.
				//
				//   liveness zone:    [antiFlapBoundary + slack, endAt]
				//                     replicas MUST eventually reach >= base+1,
				//                     proving scaling resumes after the gates.
				//
				// Sample at min(PollingInterval/3, 5s) so the strict zone has
				// at least 5 sample points even at the default 60s window.
				slack := base.keda.PollingInterval / 3
				if slack < 10*time.Second {
					slack = 10 * time.Second
				}
				strictEnd := tDown.Add(base.keda.ScaleUpStabilization - slack)
				livenessStart := tDown.Add(base.keda.ScaleUpStabilization + slack)
				endAt := tDown.Add(base.keda.ScaleUpStabilization + base.keda.ScaleUpTotalWait + 60*time.Second)

				sampleEvery := base.keda.PollingInterval / 3
				if sampleEvery > 5*time.Second {
					sampleEvery = 5 * time.Second
				}
				if sampleEvery < time.Second {
					sampleEvery = time.Second
				}

				type sample struct {
					elapsed  time.Duration
					replicas int32
				}
				var samples []sample
				for time.Now().Before(endAt) {
					r, err := utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
					Expect(err).NotTo(HaveOccurred())
					samples = append(samples, sample{
						elapsed:  time.Since(tDown).Round(time.Second),
						replicas: r,
					})
					time.Sleep(sampleEvery)
				}

				renderSamples := func() string {
					parts := make([]string, 0, len(samples))
					for _, s := range samples {
						parts = append(parts, fmt.Sprintf("t+%s=%d", s.elapsed, s.replicas))
					}
					return strings.Join(parts, ",")
				}
				GinkgoWriter.Printf("H2 samples: %s\n", renderSamples())

				// Safety: nothing in the strict zone may exceed baseline.
				var strictViolations []sample
				for _, s := range samples {
					sampleAt := tDown.Add(s.elapsed)
					if sampleAt.Before(strictEnd) && s.replicas != base.replicas {
						strictViolations = append(strictViolations, s)
					}
				}
				Expect(strictViolations).To(BeEmpty(),
					"replicas must stay at %d for the strict portion of the scale-up stabilization "+
						"window (until %s after tDown, i.e. stabilization-slack). violations=%v, "+
						"all samples: %s",
					base.replicas, base.keda.ScaleUpStabilization-slack, strictViolations, renderSamples())

				// Liveness: at least one sample at-or-after livenessStart must show
				// a successful re-scale-up. Otherwise either the post-load isn't
				// pressuring the pool (test is vacuous) or the scaler is stuck.
				var seenScaleUpInLiveness bool
				for _, s := range samples {
					sampleAt := tDown.Add(s.elapsed)
					if !sampleAt.Before(livenessStart) && s.replicas >= base.replicas+1 {
						seenScaleUpInLiveness = true
						break
					}
				}
				Expect(seenScaleUpInLiveness).To(BeTrue(),
					"after the anti-flapping window + slack (%s after tDown), replicas should "+
						"scale up again (allowing the HPA rate-limit period). all samples: %s",
					base.keda.ScaleUpStabilization+slack, renderSamples())
			})
		})
	},
)

// installScalingCase mirrors InstallCase for CaseScaling, with three
// differences: it creates the KAITO inference ConfigMap the case references,
// it waits up to scalingProvisionTimeout (a fresh GPU node plus model load can
// exceed InferenceSetReadyTimeout), and the warm-up completion is bounded.
func installScalingCase(ctx context.Context) deploy.GatewayEndpoint {
	ns := CaseNamespace(CaseScaling)
	Expect(ns).NotTo(BeEmpty(), "case %q has no namespace declared in CaseDeployments", CaseScaling)
	Expect(utils.EnsureNamespace(ctx, ns)).To(Succeed(), "failed to ensure namespace %s", ns)

	clientset, err := utils.GetK8sClientset()
	Expect(err).NotTo(HaveOccurred())
	Expect(utils.EnsureConfigMap(ctx, clientset, ns, scalingVLLMConfigName, scalingVLLMConfig)).To(Succeed())

	gateway, err := utils.OpenGateway(ctx, ns, CaseGatewayName(CaseScaling))
	Expect(err).NotTo(HaveOccurred(), "failed to open gateway for case %s", CaseScaling)

	utils.GetClusterClient(utils.TestingCluster)
	deployments := CaseDeployments[CaseScaling]
	for _, d := range deployments {
		By(fmt.Sprintf("Installing modeldeployment %s (model=%s) in %s", d.Name, d.Model, d.Namespace))
		Expect(utils.InstallModelDeployment(ctx, d)).To(Succeed(), "failed to install modeldeployment for %s", d.Name)
	}
	for _, d := range deployments {
		By(fmt.Sprintf("Waiting up to %s for modeldeployment %s to become ready", scalingProvisionTimeout, d.Name))
		Expect(utils.WaitForInferenceSetReady(ctx, d, scalingProvisionTimeout)).
			To(Succeed(), "modeldeployment %s not ready", d.Name)

		By(fmt.Sprintf("Waiting for gateway routing to be ready for deployment %s", d.Name))
		Eventually(func() error {
			utils.ForgetNamespaceAuthHeaders(d.Namespace)
			return utils.CheckChatSuccess(ctx, gateway, d.Name, utils.WithMaxTokens(scalingMaxTokens))
		}, scalingProvisionTimeout, 10*time.Second).Should(Succeed(),
			"gateway should route to deployment %s successfully", d.Name)
	}
	return gateway
}

// expectReadyModelPods waits until the deployment has exactly want model pods,
// all Ready, and returns their pod → node placement.
func expectReadyModelPods(ctx context.Context, values deploy.ModelDeploymentValues, want int, timeout time.Duration) map[string]string {
	clientset, err := utils.GetK8sClientset()
	Expect(err).NotTo(HaveOccurred())
	var placements map[string]string
	Eventually(func(g Gomega) {
		pods, err := utils.ListModelPods(ctx, clientset, values)
		g.Expect(err).NotTo(HaveOccurred())
		ready := utils.ReadyPods(pods)
		g.Expect(pods).To(HaveLen(want), "model pods: %v", utils.PodPlacements(pods))
		g.Expect(ready).To(HaveLen(want), "Ready model pods: %v of %v",
			utils.PodPlacements(ready), utils.PodPlacements(pods))
		placements = utils.PodPlacements(ready)
	}, timeout, 5*time.Second).Should(Succeed())
	return placements
}

// currentNodeClaimsByNode returns node name → NodeClaim name for every
// NodeClaim that has registered a node.
func currentNodeClaimsByNode(ctx context.Context) map[string]string {
	dynClient, err := utils.GetDynamicClient()
	Expect(err).NotTo(HaveOccurred())
	nodeClaims, err := utils.ListNodeClaims(ctx, dynClient)
	Expect(err).NotTo(HaveOccurred())
	return utils.NodeClaimsByNode(nodeClaims)
}

// snapshotNodeClaimNames returns the names of all NodeClaims in the cluster.
func snapshotNodeClaimNames(ctx context.Context) []string {
	dynClient, err := utils.GetDynamicClient()
	Expect(err).NotTo(HaveOccurred())
	nodeClaims, err := utils.ListNodeClaims(ctx, dynClient)
	Expect(err).NotTo(HaveOccurred())
	return utils.NodeClaimNames(nodeClaims)
}

// expectNoNewNodeClaims waits until every NodeClaim not in before is gone,
// including NodeClaims that never registered a node (e.g. a scale-up cancelled
// before its GPU node joined). It attributes NodeClaims by snapshot rather
// than by name, so it assumes nothing else provisions nodes while the
// Scaling specs run serially.
func expectNoNewNodeClaims(ctx context.Context, before []string, timeout time.Duration) {
	dynClient, err := utils.GetDynamicClient()
	Expect(err).NotTo(HaveOccurred())
	By(fmt.Sprintf("Waiting up to %s for NodeClaims created by the case to be removed", timeout))
	Eventually(func(g Gomega) {
		nodeClaims, err := utils.ListNodeClaims(ctx, dynClient)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(utils.NewNodeClaimNames(before, nodeClaims)).To(BeEmpty(),
			"NodeClaims created during the case must be removed")
	}, timeout, 30*time.Second).Should(Succeed())
}

func isNodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func scaledObjectReady(object map[string]interface{}) bool {
	conditions, found, err := unstructured.NestedSlice(object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, condition := range conditions {
		fields, ok := condition.(map[string]interface{})
		if ok && fields["type"] == "Ready" && fields["status"] == "True" {
			return true
		}
	}
	return false
}

func assertEPPReady(ctx context.Context, namespace, modelName string) error {
	clientset, err := utils.GetK8sClientset()
	if err != nil {
		return err
	}
	eppName := utils.EPPServiceName(modelName)
	deployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, eppName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if deployment.Status.ReadyReplicas < 1 {
		return fmt.Errorf("EPP Deployment %s/%s has no ready replicas", namespace, eppName)
	}
	selector := "inferencepool=" + eppName
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}
	if len(utils.ReadyPods(pods.Items)) > 0 {
		return nil
	}
	return fmt.Errorf("no Running/Ready EPP pod matches %q; matching pods=%v", selector, pods.Items)
}
