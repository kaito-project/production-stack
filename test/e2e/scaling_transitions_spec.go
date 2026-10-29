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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
	"github.com/kaito-project/production-stack/test/e2e/utils"
)

const (
	// transitionSettleTimeout bounds control-plane convergence after a chart
	// upgrade: the scaler creating or deleting the ScaledObject, KEDA creating
	// or deleting its HPA, and KEDA or the HPA applying a new replica bound.
	transitionSettleTimeout = 3 * time.Minute

	// transitionHoldMargin is added to a KEDA window before asserting that a
	// state held across it.
	transitionHoldMargin = 30 * time.Second

	// transitionServeTimeout covers gateway convergence after a model pod
	// becomes Ready, including an EPP rollout when flow control toggles.
	transitionServeTimeout = 5 * time.Minute
)

// The transition specs change the autoscaling configuration of a running or
// parked deployment through the chart, the way a user would, and assert the
// state the deployment converges to. Each spec first establishes its own
// starting state, so ContinueOnFailure lets one failing transition leave the
// others meaningful. Setup may pin .spec.replicas directly; the transition
// under test only uses chart upgrades.
var _ = Describe("InferenceSet Scaling — Configuration Transitions",
	Ordered, Serial, ContinueOnFailure,
	utils.GinkgoLabelScaling, utils.GinkgoLabelScalingTransitions, utils.GinkgoLabelStandardK8sOnly, func() {

		scalingValues := CaseDeployments[CaseScalingTransitions][0]
		model := scalingValues.Name
		namespace := CaseNamespace(CaseScalingTransitions)

		var (
			ctx                     context.Context
			gateway                 deploy.GatewayEndpoint
			nodeClaimsBeforeInstall []string
		)

		// The deployer renders replicas only in manual mode and leaves it empty
		// under autoscaling, so KEDA owns the live count and switching back to
		// manual always reapplies the declared count.
		autoscaled := func(minReplicas, maxReplicas int64) deploy.ModelDeploymentValues {
			values := scalingValues
			values.MinReplicas, values.MaxReplicas = minReplicas, maxReplicas
			return values
		}
		manual := func(replicas int64) deploy.ModelDeploymentValues {
			values := scalingValues
			values.EnableScaling = false
			values.Replicas = replicas
			return values
		}
		scaleToZero := func() deploy.ModelDeploymentValues {
			return scaleToZeroScalingValues(scalingValues, 1)
		}

		replicas := func() (int32, error) {
			return utils.GetInferenceSetReplicas(ctx, model, namespace)
		}
		kedaParams := func() utils.KEDAParams {
			params, err := utils.GetKEDAParams(ctx, model, namespace)
			Expect(err).NotTo(HaveOccurred())
			return params
		}
		expectReplicas := func(want int32, timeout time.Duration) {
			Eventually(replicas, timeout, 5*time.Second).Should(Equal(want),
				"InferenceSet %s/%s .spec.replicas should converge to %d", namespace, model, want)
		}
		holdReplicas := func(want int32, duration time.Duration) {
			Consistently(replicas, duration, 5*time.Second).Should(Equal(want),
				"InferenceSet %s/%s .spec.replicas must hold at %d", namespace, model, want)
		}
		expectServes := func() {
			Eventually(func() error {
				return utils.CheckChatSuccess(ctx, gateway, model, utils.WithMaxTokens(scalingMaxTokens))
			}, transitionServeTimeout, 10*time.Second).Should(Succeed(), "gateway should serve %s", model)
		}
		upgrade := func(values deploy.ModelDeploymentValues) {
			if values.EnableScaling {
				By(fmt.Sprintf("Upgrading %s: autoscaling min=%d max=%d (replicas unset)",
					model, values.MinReplicas, values.MaxReplicas))
			} else {
				By(fmt.Sprintf("Upgrading %s: manual replicas=%d", model, values.Replicas))
			}
			Expect(utils.UpgradeModelDeployment(ctx, values)).To(Succeed())
		}
		pressureLoad := func() *utils.LoadGenerator {
			return &utils.LoadGenerator{
				Gateway:     gateway,
				Model:       model,
				Prompt:      "please explain the theory of relativity in as much detail as possible",
				Concurrency: scalingPressureConcurrency,
				MaxTokens:   scalingMaxTokens,
			}
		}

		// expectScaledObjectReady waits for the scaler to (re)provision the
		// ScaledObject with the given bounds and for KEDA to report it Ready.
		expectScaledObjectReady := func(minReplicas, maxReplicas int64, timeout time.Duration) {
			Eventually(func(g Gomega) {
				so, err := getScaledObject(ctx, namespace, model)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(so).NotTo(BeNil(), "ScaledObject %s/%s must exist", namespace, model)
				g.Expect(scaledObjectBounds(so)).To(Equal([2]int64{minReplicas, maxReplicas}),
					"ScaledObject min/max replica counts")
				g.Expect(scaledObjectReady(so.Object)).To(BeTrue(),
					"ScaledObject must be Ready; status=%v", so.Object["status"])
			}, timeout, 5*time.Second).Should(Succeed())
		}

		// expectAutoscalingRemoved waits for the scaler to delete the
		// ScaledObject and for KEDA to delete the HPA it owned, so nothing
		// overrides the manually configured replica count.
		expectAutoscalingRemoved := func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				so, err := getScaledObject(ctx, namespace, model)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(so).To(BeNil(), "ScaledObject %s/%s must be removed", namespace, model)
				_, err = clientset.AutoscalingV2().HorizontalPodAutoscalers(namespace).
					Get(ctx, "keda-hpa-"+model, metav1.GetOptions{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
					"KEDA HPA keda-hpa-%s must be removed, err=%v", model, err)
			}, transitionSettleTimeout, 5*time.Second).Should(Succeed())
		}

		// expectGPUCapacityMatchesModelPods waits until every NodeClaim created
		// during the case backs a node running one of the model pods, so a
		// replica removed by a transition has also released its GPU node.
		expectGPUCapacityMatchesModelPods := func(timeout time.Duration) {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())
			dynClient, err := utils.GetDynamicClient()
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				pods, err := utils.ListModelPods(ctx, clientset, scalingValues)
				g.Expect(err).NotTo(HaveOccurred())
				modelNodes := map[string]bool{}
				for _, pod := range pods {
					modelNodes[pod.Spec.NodeName] = true
				}
				nodeClaims, err := utils.ListNodeClaims(ctx, dynClient)
				g.Expect(err).NotTo(HaveOccurred())
				backing := map[string]bool{}
				for node, claim := range utils.NodeClaimsByNode(nodeClaims) {
					if modelNodes[node] {
						backing[claim] = true
					}
				}
				var extra []string
				for _, claim := range utils.NewNodeClaimNames(nodeClaimsBeforeInstall, nodeClaims) {
					if !backing[claim] {
						extra = append(extra, claim)
					}
				}
				g.Expect(extra).To(BeEmpty(),
					"NodeClaims created by the case must back a model pod; model pods=%v", utils.PodPlacements(pods))
			}, timeout, 30*time.Second).Should(Succeed())
		}

		// modelPodUIDs identifies the model pods by UID, so a replaced pod with
		// a reused name is still detected.
		modelPodUIDs := func() (map[string]types.UID, error) {
			clientset, err := utils.GetK8sClientset()
			if err != nil {
				return nil, err
			}
			pods, err := utils.ListModelPods(ctx, clientset, scalingValues)
			if err != nil {
				return nil, err
			}
			uids := make(map[string]types.UID, len(pods))
			for _, pod := range pods {
				uids[pod.Name] = pod.UID
			}
			return uids, nil
		}

		// autoscaledStateMatches reports whether the ScaledObject already has
		// the requested bounds and the deployment already runs n Ready replicas.
		autoscaledStateMatches := func(values deploy.ModelDeploymentValues, n int32) bool {
			so, err := getScaledObject(ctx, namespace, model)
			if err != nil || so == nil || !scaledObjectReady(so.Object) ||
				scaledObjectBounds(so) != [2]int64{values.MinReplicas, values.MaxReplicas} {
				return false
			}
			current, err := replicas()
			if err != nil || current != n {
				return false
			}
			clientset, err := utils.GetK8sClientset()
			if err != nil {
				return false
			}
			pods, err := utils.ListModelPods(ctx, clientset, scalingValues)
			return err == nil && len(pods) == int(n) && len(utils.ReadyPods(pods)) == int(n)
		}

		// establishManual starts a spec from manual mode with n Ready replicas.
		establishManual := func(n int64) {
			By(fmt.Sprintf("Establishing manual mode with %d replica(s)", n))
			upgrade(manual(n))
			expectAutoscalingRemoved()
			// The rendered count may already match the previous release, in
			// which case Helm does not touch .spec.replicas; pin the live count
			// for a deterministic start.
			Expect(utils.SetInferenceSetReplicas(ctx, model, namespace, int32(n))).To(Succeed())
			expectReadyModelPods(ctx, scalingValues, int(n), scalingProvisionTimeout)
			expectGPUCapacityMatchesModelPods(scalingNodeRemovalTimeout)
			expectServes()
		}

		// establishAutoscaled starts a spec from autoscaling with n Ready
		// replicas inside the configured bounds. A matching live state is kept
		// to avoid needless GPU churn. Otherwise it passes through manual mode,
		// which deletes the HPA and its recommendation and scale-event history,
		// so a stale recommendation cannot resize the deployment while the
		// spec establishes its baseline.
		establishAutoscaled := func(values deploy.ModelDeploymentValues, n int32) {
			By(fmt.Sprintf("Establishing autoscaling min=%d max=%d with %d replica(s)",
				values.MinReplicas, values.MaxReplicas, n))
			if !autoscaledStateMatches(values, n) {
				establishManual(int64(n))
			}
			upgrade(values)
			expectScaledObjectReady(values.MinReplicas, values.MaxReplicas, transitionSettleTimeout)
			// Dropping replicas from the chart lets the CRD default (1) apply;
			// pin the count this spec starts from.
			Expect(utils.SetInferenceSetReplicas(ctx, model, namespace, n)).To(Succeed())
			expectReplicas(n, transitionSettleTimeout)
			expectReadyModelPods(ctx, scalingValues, int(n), scalingProvisionTimeout)
			expectGPUCapacityMatchesModelPods(scalingNodeRemovalTimeout)
		}

		// establishParked starts a spec parked at zero with its GPU released
		// and the EPP still serving the queue. replicas is not rendered under
		// autoscaling, so later autoscaling upgrades leave .spec.replicas at 0.
		establishParked := func() {
			By("Establishing a parked deployment (minReplicas=0, no demand)")
			values := scaleToZero()
			upgrade(values)
			expectScaledObjectReady(0, 1, transitionSettleTimeout)
			params := kedaParams()
			expectReplicas(0, params.PollingInterval+max(params.InitialCooldownPeriod, params.CooldownPeriod)+
				transitionSettleTimeout+scaleToZeroProvisioningMargin)
			expectReadyModelPods(ctx, scalingValues, 0, scalingPodRemovalTimeout)
			Expect(assertEPPReady(ctx, namespace, model)).To(Succeed())
			expectGPUCapacityMatchesModelPods(scalingNodeRemovalTimeout)
		}

		BeforeAll(func() {
			ctx = context.Background()
			nodeClaimsBeforeInstall = snapshotNodeClaimNames(ctx)
			gateway = installScalingCase(ctx, CaseScalingTransitions)
		})

		AfterAll(func() {
			UninstallCase(CaseScalingTransitions)
			if nodeClaimsBeforeInstall != nil {
				expectNoNewNodeClaims(context.Background(), nodeClaimsBeforeInstall, scalingNodeRemovalTimeout)
			}
		})

		It("manual → manual: scales to the new fixed count and back, with no autoscaling objects", func() {
			establishManual(1)

			By("Raising the manual replica count to 2")
			upgrade(manual(2))
			expectReplicas(2, transitionSettleTimeout)
			expectReadyModelPods(ctx, scalingValues, 2, scalingProvisionTimeout)
			expectAutoscalingRemoved()
			expectServes()

			By("Lowering the manual replica count to 1")
			upgrade(manual(1))
			expectReplicas(1, transitionSettleTimeout)
			expectReadyModelPods(ctx, scalingValues, 1, scalingPodRemovalTimeout)
			expectGPUCapacityMatchesModelPods(scalingNodeRemovalTimeout)
			expectAutoscalingRemoved()
			expectServes()
		})

		It("manual (2) → autoscaling (min 2): settles at the autoscaling floor and keeps serving", func() {
			establishManual(2)

			// Known limitation: replicas leaves the rendered manifest when
			// autoscaling is enabled, so the CRD default (1) can replace the
			// manual count until KEDA enforces the floor. The spec accepts that
			// dip and asserts only where the deployment settles.
			upgrade(autoscaled(2, 2))
			expectScaledObjectReady(2, 2, transitionSettleTimeout)
			expectReplicas(2, transitionSettleTimeout)
			expectReadyModelPods(ctx, scalingValues, 2, scalingProvisionTimeout)
			expectGPUCapacityMatchesModelPods(scalingNodeRemovalTimeout)
			expectServes()
		})

		It("manual → autoscaling (min 1): adopts the running replica without a restart, then scales on demand", func() {
			establishManual(1)
			before, err := modelPodUIDs()
			Expect(err).NotTo(HaveOccurred())

			target := autoscaled(1, 2)
			upgrade(target)
			expectScaledObjectReady(1, 2, transitionSettleTimeout)
			params := kedaParams()

			By("Holding idle across two polling intervals: same replica count, same pod")
			Consistently(func(g Gomega) {
				current, err := replicas()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(current).To(Equal(int32(1)))
				uids, err := modelPodUIDs()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(uids).To(Equal(before), "enabling autoscaling must not replace the running model pod")
			}, 2*params.PollingInterval+transitionHoldMargin, 5*time.Second).Should(Succeed())

			By("Driving queue pressure: the adopted deployment scales up")
			load := pressureLoad()
			load.Start(ctx)
			defer load.Stop()
			expectReplicas(2, params.ScaleUpTotalWait+90*time.Second)
			expectReadyModelPods(ctx, scalingValues, 2, scalingProvisionTimeout)
		})

		It("lowering maxReplicas below the current count scales down to the new ceiling and releases the GPU", func() {
			// The previous spec may leave one or two replicas, and its HPA may
			// still be scaling down; pressure below drives either to two, so only
			// the bounds need establishing.
			if so, err := getScaledObject(ctx, namespace, model); err != nil || so == nil ||
				!scaledObjectReady(so.Object) || scaledObjectBounds(so) != [2]int64{1, 2} {
				establishAutoscaled(autoscaled(1, 2), 1)
			}
			params := kedaParams()

			By("Holding the deployment at max=2 with queue pressure")
			load := pressureLoad()
			load.Start(ctx)
			defer load.Stop()
			expectReplicas(2, params.ScaleUpTotalWait+90*time.Second)
			expectReadyModelPods(ctx, scalingValues, 2, scalingProvisionTimeout)

			lowered := autoscaled(1, 1)
			upgrade(lowered)
			expectScaledObjectReady(1, 1, transitionSettleTimeout)
			expectReplicas(1, transitionSettleTimeout)
			expectReadyModelPods(ctx, scalingValues, 1, scalingPodRemovalTimeout)
			expectGPUCapacityMatchesModelPods(scalingNodeRemovalTimeout)

			By("Keeping pressure on: the new ceiling holds")
			holdReplicas(1, params.ScaleUpTotalWait+transitionHoldMargin)
			// A single probe under 40-way pressure can legitimately get 429.
			load.Stop()
			expectServes()
		})

		It("raising minReplicas above the current count scales up to the new floor", func() {
			establishAutoscaled(autoscaled(1, 2), 1)

			// replicas is not rendered under autoscaling, so only KEDA enforcing
			// the new floor can add the second replica.
			raised := autoscaled(2, 2)
			upgrade(raised)
			expectScaledObjectReady(2, 2, transitionSettleTimeout)
			expectReplicas(2, transitionSettleTimeout)
			expectReadyModelPods(ctx, scalingValues, 2, scalingProvisionTimeout)

			By("Holding idle: the HPA must not scale below the new floor")
			holdReplicas(2, 2*kedaParams().PollingInterval+transitionHoldMargin)
			expectGPUCapacityMatchesModelPods(scalingNodeRemovalTimeout)
		})

		It("autoscaling → manual: removes the ScaledObject and HPA, converges to the manual count, and stops autoscaling", func() {
			establishAutoscaled(autoscaled(2, 2), 2)
			params := kedaParams()

			upgrade(manual(1))
			expectAutoscalingRemoved()
			expectReplicas(1, transitionSettleTimeout)
			expectReadyModelPods(ctx, scalingValues, 1, scalingPodRemovalTimeout)
			expectGPUCapacityMatchesModelPods(scalingNodeRemovalTimeout)
			expectServes()

			By("Driving queue pressure: manual mode must not scale up")
			load := pressureLoad()
			load.Start(ctx)
			defer load.Stop()
			holdReplicas(1, params.ScaleUpTotalWait+transitionHoldMargin)
			expectReadyModelPods(ctx, scalingValues, 1, scalingEndpointTimeout)
		})

		It("manual → autoscaling (min 0): keeps the idle replica through the initial cooldown, then parks", func() {
			establishManual(1)

			enabledAt := time.Now()
			upgrade(scaleToZero())
			expectScaledObjectReady(0, 1, transitionSettleTimeout)
			params := kedaParams()
			Expect(params.InitialCooldownPeriod).To(BeNumerically(">", 0),
				"the case must configure an initial cooldown to make this transition observable")

			// The ScaledObject is created after enabledAt, so ending the hold one
			// polling interval before enabledAt+initialCooldown stays inside the grace.
			if hold := time.Until(enabledAt.Add(params.InitialCooldownPeriod - params.PollingInterval)); hold > 0 {
				By(fmt.Sprintf("Holding the idle replica for %s of the initial cooldown", hold.Round(time.Second)))
				holdReplicas(1, hold)
			}
			// Budget includes the EPP rollout triggered by enabling flow control.
			expectReplicas(0, params.InitialCooldownPeriod+params.PollingInterval+
				scaleToZeroProvisioningMargin+transitionSettleTimeout)
			expectReadyModelPods(ctx, scalingValues, 0, scalingPodRemovalTimeout)
			Expect(assertEPPReady(ctx, namespace, model)).To(Succeed())
			expectGPUCapacityMatchesModelPods(scalingNodeRemovalTimeout)
		})

		It("parked: raising minReplicas 0 → 1 restores one serving replica", func() {
			establishParked()

			// replicas is not rendered under autoscaling, so Helm leaves .spec.replicas
			// at 0 and only KEDA enforcing the new floor can restore the replica.
			upgrade(autoscaled(1, 2))
			Eventually(func(g Gomega) {
				so, err := getScaledObject(ctx, namespace, model)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(so).NotTo(BeNil())
				g.Expect(scaledObjectBounds(so)).To(Equal([2]int64{1, 2}))
				g.Expect(scaledObjectReady(so.Object)).To(BeTrue(),
					"ScaledObject must not report trigger errors at the new floor; status=%v", so.Object["status"])
				current, err := replicas()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(current).To(Equal(int32(1)), "KEDA must raise .spec.replicas to minReplicas")
			}, transitionSettleTimeout, 5*time.Second).Should(Succeed())
			Expect(utils.WaitForInferenceSetReady(ctx, scalingValues, scalingProvisionTimeout)).To(Succeed())
			expectReadyModelPods(ctx, scalingValues, 1, scalingEndpointTimeout)
			expectServes()
		})

		It("parked → manual: restores the declared replica count", func() {
			establishParked()

			upgrade(manual(1))
			expectAutoscalingRemoved()
			expectReplicas(1, transitionSettleTimeout)
			Expect(utils.WaitForInferenceSetReady(ctx, scalingValues, scalingProvisionTimeout)).To(Succeed())
			expectReadyModelPods(ctx, scalingValues, 1, scalingEndpointTimeout)
			expectServes()
		})
	},
)

// getScaledObject returns the scaler-managed ScaledObject, or nil when it
// does not exist.
func getScaledObject(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	dynClient, err := utils.GetDynamicClient()
	if err != nil {
		return nil, err
	}
	so, err := dynClient.Resource(utils.ScaledObjectGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return so, err
}

// scaledObjectBounds returns the ScaledObject's [minReplicaCount, maxReplicaCount].
func scaledObjectBounds(so *unstructured.Unstructured) [2]int64 {
	minReplicas, _, _ := unstructured.NestedInt64(so.Object, "spec", "minReplicaCount")
	maxReplicas, _, _ := unstructured.NestedInt64(so.Object, "spec", "maxReplicaCount")
	return [2]int64{minReplicas, maxReplicas}
}
