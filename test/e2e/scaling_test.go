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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Scaling tests target a single model to keep assertions deterministic.
// Scale-Up / Scale-Down are correctness properties of one pool at a time.
//
// The deployment Name / Namespace are resolved per-Describe from the
// CaseDeployments table (see cases.go) — that table is the single source
// of truth for the model name carried in OpenAI requests and matched by
// the gateway. CaseScaling does NOT enable AuthAPIKeyEnabled, so the
// modelharness chart leaves ext_authz off for this namespace and plain
// SendChatCompletion* / LoadGenerator (no Authorization header) is the
// correct probe.

const (
	// Concurrency for queue-pressure workloads. The CaseScaling baseline
	// is 1 replica with KEDA threshold=10 and the inference simulator's
	// max-num-seqs=5, so the single pod has 5 running slots and any
	// extra in-flight requests pile into the queue. Setting concurrency
	// to 40 leaves ~35 requests waiting on that pod — well above the
	// threshold — guaranteeing scale-up fires.
	scalingPressureConcurrency = 40

	// Concurrency for below-threshold workload. Chosen so the queue settles
	// strictly below threshold=10 even with the scheduler's load spread.
	scalingSubThresholdConcurrency = 4

	// Rate of the background low-rate stream used for "no 5xx during scale-down".
	scalingLowRate = 1.0 // requests per second

	// Margins applied on top of the KEDA-derived Eventually timeouts.
	scalingNodeReadyTimeout = 2 * time.Minute
	scalingEndpointTimeout  = 1 * time.Minute
)

var _ = Describe("InferenceSet Scaling — Infra",
	Ordered, utils.GinkgoLabelScaling, utils.GinkgoLabelNightly, func() {

		// Per-case deployment owned by scaling_test.go (see cases.go).
		// Resolved as Describe-local values so the table lookup happens
		// inside the suite (matching the pattern in model_routing_test.go),
		// not at package init.
		caseDeployments := CaseDeployments[CaseScaling]
		scalingModel := caseDeployments[0].Name
		scalingNamespace := CaseNamespace(CaseScaling)

		// gatewayURL routes into the CaseScaling deployment's Gateway.
		// Resolved in the outer BeforeAll once per suite run so all nested
		// Describes (Scale-Up → Scale-Down, Anti-Flapping) share it.
		var gatewayURL string

		BeforeAll(func() {
			gatewayURL = InstallCase(CaseScaling)
		})

		AfterAll(func() {
			UninstallCase(CaseScaling)
		})

		// Baseline state shared by the Ordered Scale-Up → Scale-Down flow.
		type baseline struct {
			replicas  int32
			inventory utils.ScalingInventory
			keda      utils.KEDAParams
		}

		Describe("Scale-Up → Scale-Down End-to-End",
			Ordered, utils.GinkgoLabelScaleUp, utils.GinkgoLabelScaleDown, func() {

				var (
					ctx      context.Context
					base     baseline
					bulkLoad *utils.LoadGenerator
					lowLoad  *utils.LoadGenerator
					// State carried across Its.
					newFakeNode    string
					newShadowPod   string
					newNodeClaim   string
					newLease       string
					inventoryAfter utils.ScalingInventory
				)

				BeforeAll(func() {
					ctx = context.Background()
					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())
					dynClient, err := utils.GetDynamicClient()
					Expect(err).NotTo(HaveOccurred())

					// The shared BeforeSuite installs the CaseScaling
					// InferenceSet via the modeldeployment Helm chart at
					// Replicas=1 with EnableScaling=true, so KEDA's
					// ScaledObject and the baseline pool are already in
					// place by the time we run. We only need to read the
					// effective KEDA params and snapshot the baseline
					// inventory — no replica patching, no warm-up — see
					// cases.go::CaseScaling for the source of truth.
					By("Reading effective KEDA parameters from the ScaledObject")
					base.keda, err = utils.GetKEDAParams(ctx, scalingModel, scalingNamespace)
					Expect(err).NotTo(HaveOccurred(),
						"ScaledObject for %s must exist (CaseScaling.EnableScaling must be true)", scalingModel)
					Expect(base.keda.Threshold).To(BeNumerically(">", 0),
						"KEDA threshold must be set")
					GinkgoWriter.Printf(
						"KEDA params: threshold=%d polling=%s cooldown=%s upStab=%s downStab=%s "+
							"→ scaleUpWait=%s scaleDownWait=%s\n",
						base.keda.Threshold, base.keda.PollingInterval, base.keda.CooldownPeriod,
						base.keda.ScaleUpStabilization, base.keda.ScaleDownStabilization,
						base.keda.ScaleUpTotalWait, base.keda.ScaleDownTotalWait)

					By("Capturing baseline replicas and inventory")
					base.replicas, err = utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
					Expect(err).NotTo(HaveOccurred())
					Expect(base.replicas).To(BeNumerically(">=", 1),
						"BeforeSuite must have installed %s with replicas>=1", scalingModel)
					GinkgoWriter.Printf("Baseline replicas for %s: %d\n", scalingModel, base.replicas)

					base.inventory, err = utils.SnapshotScalingInventory(ctx, clientset, dynClient, scalingNamespace, scalingModel)
					Expect(err).NotTo(HaveOccurred())
					Expect(base.inventory.ShadowPodNames).To(HaveLen(int(base.replicas)),
						"expected %d shadow pods at baseline, got %v",
						base.replicas, base.inventory.ShadowPodNames)
					GinkgoWriter.Printf("Baseline inventory: %d fake-nodes, %d shadow-pods, %d NodeClaims, %d leases\n",
						len(base.inventory.FakeNodeNames), len(base.inventory.ShadowPodNames),
						len(base.inventory.NodeClaimNames), len(base.inventory.LeaseNames))

					By("Starting low-rate background stream for the scale-down transition window")
					lowLoad = &utils.LoadGenerator{
						GatewayURL: gatewayURL,
						Model:      scalingModel,
						Prompt:     "hi",
						Rate:       scalingLowRate,
					}
					lowLoad.Start(ctx)
				})

				AfterAll(func() {
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
					if base.replicas <= 0 {
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
						GatewayURL:  gatewayURL,
						Model:       scalingModel,
						Prompt:      "please explain the theory of relativity in as much detail as possible",
						Concurrency: scalingPressureConcurrency,
					}
					bulkLoad.Start(ctx)

					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())

					By(fmt.Sprintf("Waiting for at least one pod's vllm:num_requests_waiting > %d",
						base.keda.Threshold))
					Eventually(func(g Gomega) {
						snap, err := utils.ScrapeModelMetric(ctx, clientset, scalingNamespace, scalingModel, "vllm:num_requests_waiting")
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
				})

				It("A2: KEDA bumps InferenceSet.spec.replicas", func() {
					expected := base.replicas + 1
					By(fmt.Sprintf("Waiting for .spec.replicas to reach %d", expected))
					Eventually(func() (int32, error) {
						return utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
					}, base.keda.ScaleUpTotalWait+60*time.Second, 5*time.Second).
						Should(BeNumerically(">=", expected),
							"KEDA should bump replicas from %d to >=%d within %s",
							base.replicas, expected, base.keda.ScaleUpTotalWait+60*time.Second)
				})

				It("B1: a new Fake Node becomes Ready with the correct labels", func() {
					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())
					dynClient, err := utils.GetDynamicClient()
					Expect(err).NotTo(HaveOccurred())

					Eventually(func(g Gomega) {
						inv, err := utils.SnapshotScalingInventory(ctx, clientset, dynClient, scalingNamespace, scalingModel)
						g.Expect(err).NotTo(HaveOccurred())
						diff := utils.DiffInventory(base.inventory, inv)
						g.Expect(diff.AddedFakeNodes).NotTo(BeEmpty(),
							"expected at least one new fake node, got inventory=%+v", inv)
						// Pick the first new fake node and verify Ready + labels.
						newFakeNode = diff.AddedFakeNodes[0]
						node, err := clientset.CoreV1().Nodes().Get(ctx, newFakeNode, metav1.GetOptions{})
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(node.Labels).To(HaveKeyWithValue("kaito.sh/fake-node", "true"))
						g.Expect(node.Labels).To(HaveKey("kaito.sh/workspace"))
						g.Expect(utils.IsFakeNodeReady(node)).To(BeTrue(),
							"new fake node %s must be Ready=True without unreachable taint", newFakeNode)
					}, scalingNodeReadyTimeout, 5*time.Second).Should(Succeed())
					GinkgoWriter.Printf("New fake node: %s\n", newFakeNode)
				})

				It("B2: a new Shadow Pod becomes Running with both containers", func() {
					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())
					dynClient, err := utils.GetDynamicClient()
					Expect(err).NotTo(HaveOccurred())

					Eventually(func(g Gomega) {
						inv, err := utils.SnapshotScalingInventory(ctx, clientset, dynClient, scalingNamespace, scalingModel)
						g.Expect(err).NotTo(HaveOccurred())
						diff := utils.DiffInventory(base.inventory, inv)
						g.Expect(diff.AddedShadowPods).NotTo(BeEmpty(),
							"expected a new shadow pod, got inventory=%+v", inv)
						newShadowPod = diff.AddedShadowPods[0]
						pod, err := clientset.CoreV1().Pods(scalingNamespace).Get(
							ctx, newShadowPod, metav1.GetOptions{})
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning),
							"shadow pod %s should be Running", newShadowPod)
						g.Expect(pod.Status.PodIP).NotTo(BeEmpty(),
							"shadow pod %s must have a CNI PodIP", newShadowPod)

						// uds-tokenizer runs as a native sidecar (init container with
						// restartPolicy=Always), llm-d-inference-sim runs as a regular
						// container. Aggregate readiness from both Spec.Containers and
						// Spec.InitContainers, then map names to their *Status entries.
						containers := map[string]bool{}
						for _, c := range pod.Spec.Containers {
							containers[c.Name] = false
						}
						for _, c := range pod.Spec.InitContainers {
							containers[c.Name] = false
						}
						for _, cs := range pod.Status.ContainerStatuses {
							containers[cs.Name] = cs.Ready
						}
						for _, cs := range pod.Status.InitContainerStatuses {
							containers[cs.Name] = cs.Ready
						}
						g.Expect(containers).To(HaveKey("llm-d-inference-sim"))
						g.Expect(containers).To(HaveKey("uds-tokenizer"))
						for name, ready := range containers {
							g.Expect(ready).To(BeTrue(),
								"container %s of shadow pod %s should be ready", name, newShadowPod)
						}
					}, scalingNodeReadyTimeout, 5*time.Second).Should(Succeed())
					GinkgoWriter.Printf("New shadow pod: %s\n", newShadowPod)
				})

				It("B3: the original pod is patched with the shadow pod's CNI IP", func() {
					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())

					shadowPod, err := clientset.CoreV1().Pods(scalingNamespace).
						Get(ctx, newShadowPod, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					// Annotation `kaito.sh/original-pod` is `<namespace>/<name>` and
					// is the canonical reference back to the original pod.
					originalRef := shadowPod.Annotations["kaito.sh/original-pod"]
					Expect(originalRef).NotTo(BeEmpty(),
						"shadow pod %s must carry kaito.sh/original-pod annotation", newShadowPod)
					parts := strings.SplitN(originalRef, "/", 2)
					Expect(parts).To(HaveLen(2),
						"kaito.sh/original-pod should be ns/name, got %q", originalRef)
					origNS, origName := parts[0], parts[1]
					shadowIP := shadowPod.Status.PodIP
					Expect(shadowIP).NotTo(BeEmpty())

					Eventually(func(g Gomega) {
						orig, err := clientset.CoreV1().Pods(origNS).
							Get(ctx, origName, metav1.GetOptions{})
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(orig.Status.Phase).To(Equal(corev1.PodRunning),
							"original pod %s should be Running", origName)
						g.Expect(orig.Status.PodIP).To(Equal(shadowIP),
							"original pod podIP should match shadow IP")
						g.Expect(orig.Annotations).To(HaveKey("kaito.sh/shadow-pod-ref"),
							"original pod should carry shadow-pod-ref annotation")
					}, scalingEndpointTimeout, 3*time.Second).Should(Succeed())
				})

				It("C1: inference_pool_ready_pods grows to match the new replica count", func() {
					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())

					Eventually(func(g Gomega) {
						current, err := utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
						g.Expect(err).NotTo(HaveOccurred())
						// Count Ready inference-pool pods directly via the Kubernetes
						// API instead of scraping the EPP /metrics endpoint. EPP
						// v1.3.x enables controller-runtime's metrics auth filter,
						// which rejects requests routed through the apiserver
						// pod-proxy (the proxy strips the Authorization header).
						// Counting pods with the InferencePool's selector that
						// have PodReady=True is semantically equivalent to the
						// `inference_pool_ready_pods` gauge that EPP exports.
						ready, err := utils.CountInferencePoolReadyPods(ctx, clientset, scalingModel, scalingNamespace)
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(ready).To(BeNumerically(">=", int(current)),
							"ready inference-pool pods (%d) should match replicas (%d)", ready, current)
					}, scalingEndpointTimeout+1*time.Minute, 5*time.Second).Should(Succeed())
				})

				It("D1: the new shadow pod actually serves traffic", func() {
					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())

					By("Scraping per-pod vllm:request_success_total")
					var lastSnap utils.PodMetricSnapshot
					Eventually(func(g Gomega) {
						snap, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, scalingNamespace, scalingModel)
						g.Expect(err).NotTo(HaveOccurred())
						lastSnap = snap
						v, ok := snap[newShadowPod]
						g.Expect(ok).To(BeTrue(),
							"new shadow pod %s must appear in per-pod metrics", newShadowPod)
						g.Expect(v).To(BeNumerically(">", 0),
							"new shadow pod %s should have vllm:request_success_total > 0", newShadowPod)
					}, 90*time.Second, 5*time.Second).Should(Succeed())
					GinkgoWriter.Printf("Per-pod success counters after scale-up: %+v\n", lastSnap)

					// Capture the post-scale-up inventory for Scale-Down asserts.
					dynClient, err := utils.GetDynamicClient()
					Expect(err).NotTo(HaveOccurred())
					inventoryAfter, err = utils.SnapshotScalingInventory(ctx, clientset, dynClient, scalingNamespace, scalingModel)
					Expect(err).NotTo(HaveOccurred())
					diff := utils.DiffInventory(base.inventory, inventoryAfter)
					if len(diff.AddedNodeClaims) > 0 {
						newNodeClaim = diff.AddedNodeClaims[0]
					}
					if len(diff.AddedLeases) > 0 {
						newLease = diff.AddedLeases[0]
					}
					GinkgoWriter.Printf("Scale-up added: fakeNodes=%v shadowPods=%v nodeClaims=%v leases=%v\n",
						diff.AddedFakeNodes, diff.AddedShadowPods, diff.AddedNodeClaims, diff.AddedLeases)
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
						snap, err := utils.ScrapeModelMetric(ctx, clientset, scalingNamespace, scalingModel,
							"vllm:num_requests_waiting")
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(snap).NotTo(BeEmpty(),
							"expected at least one shadow pod to expose vllm:num_requests_waiting")
						var m float64
						for _, v := range snap {
							if v > m {
								m = v
							}
						}
						// The queue must still be loaded — bulk load is
						// running at concurrency=40 against ≤2 pods × 5
						// running slots. If we don't see a non-zero queue,
						// either the simulator isn't reporting the metric
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

					By("Waiting for vllm:num_requests_waiting == 0 on every shadow pod")
					// We don't wait for running==0 because lowLoad keeps 1 req/s running.
					Eventually(func(g Gomega) {
						snap, err := utils.ScrapeModelMetric(ctx, clientset, scalingNamespace, scalingModel,
							"vllm:num_requests_waiting")
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(snap).NotTo(BeEmpty(),
							"expected at least one shadow pod in metrics snapshot")
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
						g.Expect(r).To(Equal(base.replicas),
							"replicas should drop to %d, now %d", base.replicas, r)
					}, base.keda.ScaleDownTotalWait+2*time.Minute, 15*time.Second).Should(Succeed())
					_ = last
				})

				It("F1: excess shadow-pod, fake-node, NodeClaim, and lease are cleaned up", func() {
					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())
					dynClient, err := utils.GetDynamicClient()
					Expect(err).NotTo(HaveOccurred())

					Eventually(func(g Gomega) {
						// The extra shadow pod must be gone.
						_, err := clientset.CoreV1().Pods(scalingNamespace).
							Get(ctx, newShadowPod, metav1.GetOptions{})
						g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
							"shadow pod %s should be deleted, got err=%v", newShadowPod, err)

						// The extra fake node must be gone.
						_, err = clientset.CoreV1().Nodes().Get(ctx, newFakeNode, metav1.GetOptions{})
						g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
							"fake node %s should be deleted, got err=%v", newFakeNode, err)

						// The lease for the fake node must be GC'd.
						if newLease != "" {
							_, err = clientset.CoordinationV1().Leases("kube-node-lease").
								Get(ctx, newLease, metav1.GetOptions{})
							g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
								"lease %s should be GC'd, got err=%v", newLease, err)
						}

						// The extra NodeClaim must be gone (or at least marked deleting).
						if newNodeClaim != "" {
							_, err = dynClient.Resource(utils.NodeClaimGVR).
								Get(ctx, newNodeClaim, metav1.GetOptions{})
							g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
								"NodeClaim %s should be deleted, got err=%v", newNodeClaim, err)
						}
					}, 8*time.Minute, 5*time.Second).Should(Succeed())

					By("Verifying baseline inventory is preserved")
					inv, err := utils.SnapshotScalingInventory(ctx, clientset, dynClient, scalingNamespace, scalingModel)
					Expect(err).NotTo(HaveOccurred())
					Expect(inv.ShadowPodNames).To(HaveLen(len(base.inventory.ShadowPodNames)),
						"baseline shadow pods should be preserved")
					Expect(inv.FakeNodeNames).To(HaveLen(len(base.inventory.FakeNodeNames)),
						"baseline fake nodes should be preserved")
					for _, name := range base.inventory.ShadowPodNames {
						Expect(inv.ShadowPodNames).To(ContainElement(name),
							"baseline shadow pod %s must still exist", name)
					}
				})

				It("F2: inference_pool_ready_pods shrinks to match the new replica count", func() {
					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())

					Eventually(func(g Gomega) {
						// See C1 for why we count Ready pods directly via the
						// Kubernetes API instead of scraping EPP /metrics.
						ready, err := utils.CountInferencePoolReadyPods(ctx, clientset, scalingModel, scalingNamespace)
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(ready).To(Equal(int(base.replicas)),
							"ready inference-pool pods should be %d, got %d", base.replicas, ready)
					}, scalingEndpointTimeout, 5*time.Second).Should(Succeed())
				})

				It("G1: remaining pods continue to serve traffic after scale-down", func() {
					clientset, err := utils.GetK8sClientset()
					Expect(err).NotTo(HaveOccurred())

					beforeSnap, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, scalingNamespace, scalingModel)
					Expect(err).NotTo(HaveOccurred())

					By("Driving a short moderate-rate burst strictly below threshold")
					burst := &utils.LoadGenerator{
						GatewayURL:  gatewayURL,
						Model:       scalingModel,
						Prompt:      "hello",
						Concurrency: scalingSubThresholdConcurrency,
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

					afterSnap, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, scalingNamespace, scalingModel)
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

				It("G2: low-rate stream saw no 5xx during the scale-down transition", func() {
					stats := lowLoad.Stats()
					GinkgoWriter.Printf("Low-rate stats across scale-down window: %+v\n", stats)
					Expect(stats.Errors5xx).To(BeNumerically("==", 0),
						"background low-rate stream must not observe any 5xx, stats=%+v", stats)

					// Allow a handful of transport-level reconnects; be strict about server errors.
					Expect(stats.Total).To(BeNumerically(">", 0),
						"low-rate generator must have issued some requests")
				})
			},
		)

		Describe("Anti-Flapping", utils.GinkgoLabelAntiFlapping, func() {

			var (
				ctx  context.Context
				base struct {
					replicas  int32
					inventory utils.ScalingInventory
					keda      utils.KEDAParams
				}
			)

			BeforeEach(func() {
				ctx = context.Background()
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())
				dynClient, err := utils.GetDynamicClient()
				Expect(err).NotTo(HaveOccurred())

				base.keda, err = utils.GetKEDAParams(ctx, scalingModel, scalingNamespace)
				Expect(err).NotTo(HaveOccurred())
				base.replicas, err = utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
				Expect(err).NotTo(HaveOccurred())
				base.inventory, err = utils.SnapshotScalingInventory(ctx, clientset, dynClient, scalingNamespace, scalingModel)
				Expect(err).NotTo(HaveOccurred())
				GinkgoWriter.Printf("Anti-flapping baseline: replicas=%d fakeNodes=%d shadowPods=%d\n",
					base.replicas, len(base.inventory.FakeNodeNames), len(base.inventory.ShadowPodNames))
			})

			AfterEach(func() {
				By("Restoring baseline replicas and waiting for cooldown to clear")
				if err := utils.SetInferenceSetReplicas(
					context.Background(), scalingModel, scalingNamespace, base.replicas); err != nil {
					GinkgoWriter.Printf("warning: restore replicas: %v\n", err)
				}
				// Passively wait one cooldown so the next case starts fresh.
				// (Only pays the full window when a test actually caused a scale event.)
				time.Sleep(base.keda.CooldownPeriod)
			})

			It("H1: stays at baseline when pressure is held below threshold", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())
				dynClient, err := utils.GetDynamicClient()
				Expect(err).NotTo(HaveOccurred())

				By("Running sub-threshold load")
				load := &utils.LoadGenerator{
					GatewayURL:  gatewayURL,
					Model:       scalingModel,
					Prompt:      "hello",
					Concurrency: scalingSubThresholdConcurrency,
				}
				load.Start(ctx)
				defer load.Stop()

				observeWindow := base.keda.PollingInterval + base.keda.CooldownPeriod
				// Sample num_requests_waiting every 15s and confirm it stays below threshold.
				sampleEvery := 15 * time.Second
				deadline := time.Now().Add(observeWindow)
				var maxSeen float64
				for time.Now().Before(deadline) {
					snap, err := utils.ScrapeModelMetric(ctx, clientset, scalingNamespace, scalingModel,
						"vllm:num_requests_waiting")
					Expect(err).NotTo(HaveOccurred())
					for _, v := range snap {
						if v > maxSeen {
							maxSeen = v
						}
					}
					time.Sleep(sampleEvery)
				}
				GinkgoWriter.Printf("Max num_requests_waiting during H1: %v (threshold=%d)\n",
					maxSeen, base.keda.Threshold)
				Expect(maxSeen).To(BeNumerically("<", float64(base.keda.Threshold)),
					"H1 must keep load strictly below threshold to be meaningful")

				By("Verifying replicas and inventory did not change")
				cur, err := utils.GetInferenceSetReplicas(ctx, scalingModel, scalingNamespace)
				Expect(err).NotTo(HaveOccurred())
				Expect(cur).To(Equal(base.replicas),
					"replicas must remain at baseline %d, got %d", base.replicas, cur)

				inv, err := utils.SnapshotScalingInventory(ctx, clientset, dynClient, scalingNamespace, scalingModel)
				Expect(err).NotTo(HaveOccurred())
				diff := utils.DiffInventory(base.inventory, inv)
				Expect(diff.AddedFakeNodes).To(BeEmpty(),
					"no fake node should be added; got %v", diff.AddedFakeNodes)
				Expect(diff.AddedShadowPods).To(BeEmpty(),
					"no shadow pod should be added; got %v", diff.AddedShadowPods)
				Expect(diff.AddedNodeClaims).To(BeEmpty(),
					"no NodeClaim should be added; got %v", diff.AddedNodeClaims)
			})

			It("H2: HPA scale-up stabilization gates immediate re-scale-up after a scale-down", func() {
				// H2 runs an independent full Scale-Up → Scale-Down cycle, then
				// immediately re-applies pressure and verifies that:
				//   - replicas hold at baseline for the full HPA scale-up
				//     stabilization window after tDown (safety / no oscillation),
				//   - replicas DO scale up again once the window has elapsed
				//     plus one polling interval of slack (liveness / not stuck).
				//
				// NOTE on KEDA semantics: spec.cooldownPeriod only gates
				// scale-to-zero (it kicks in when the last trigger reports
				// inactive and minReplicaCount is 0). With minReplicaCount>=1,
				// the actual anti-oscillation gate after a scale-down is the
				// HPA scale-up stabilizationWindowSeconds, which keda-kaito-scaler
				// sets via spec.advanced.horizontalPodAutoscalerConfig.behavior.

				By("Scale-Up: driving pressure")
				bulk := &utils.LoadGenerator{
					GatewayURL:  gatewayURL,
					Model:       scalingModel,
					Prompt:      "please explain the theory of relativity in as much detail as possible",
					Concurrency: scalingPressureConcurrency,
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
					GatewayURL:  gatewayURL,
					Model:       scalingModel,
					Prompt:      "please explain the theory of relativity in as much detail as possible",
					Concurrency: scalingPressureConcurrency,
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
				//                     proving the stabilization window was the
				//                     only gate.
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
						"scale up again (proves stabilization was the only gate). all samples: %s",
					base.keda.ScaleUpStabilization+slack, renderSamples())
			})
		})
	},
)
