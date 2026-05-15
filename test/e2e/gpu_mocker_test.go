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
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

var _ = Describe("GPU Mocker E2E", Ordered, func() {
	// Per-case deployments owned by gpu_mocker_test.go (see cases.go).
	// Installed in a dedicated namespace by BeforeAll so this case can
	// run in parallel with other Ordered Describes.
	caseDeployments := CaseDeployments[CaseGPUMocker]
	caseNamespace := CaseNamespace(CaseGPUMocker)
	suiteDeployments := caseDeployments
	falconModel := caseDeployments[0].Name

	// caseGatewayURL is the URL routing into this case's dedicated
	// Gateway. Resolved in BeforeAll.
	var caseGatewayURL string

	// sendChat forwards to the non-auth helper — the gpu-mocker case
	// no longer enables the API-key AuthorizationPolicy (see cases.go).
	sendChat := func(url, model string) (*http.Response, error) {
		return utils.SendChatCompletion(url, model)
	}

	BeforeAll(func() {
		caseGatewayURL = InstallCase(CaseGPUMocker)
	})

	AfterAll(func() {
		UninstallCase(CaseGPUMocker)
	})

	Context("GPU Node Mocker", utils.GinkgoLabelSmoke, func() {

		Context("Framework validation", utils.GinkgoLabelSmoke, func() {
			It("should have the test framework properly initialised", func() {
				Expect(true).To(BeTrue(), "framework sanity check")
			})
		})

		Context("Gateway connectivity", utils.GinkgoLabelSmoke, func() {
			It("should be reachable and return a response", func() {
				// Retry with backoff — BBR/EPP ext_proc filters may need time
				// to establish gRPC connections after cluster setup.
				Eventually(func() error {
					resp, err := sendChat(caseGatewayURL, falconModel)
					if err != nil {
						return fmt.Errorf("request failed: %w", err)
					}
					defer resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						body, _ := utils.ReadResponseBody(resp)
						return fmt.Errorf("expected 200, got %d: %s", resp.StatusCode, string(body))
					}
					return nil
				}, 5*time.Minute, 10*time.Second).Should(Succeed(),
					"case gateway should be reachable and return 200")
			})
		})
	})

	Context("InferenceSet and InferencePool lifecycle", utils.GinkgoLabelInfra, func() {

		Context("InferenceSet lifecycle", func() {
			It("should have EPP pods running for each InferencePool", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				for _, d := range suiteDeployments {
					eppName := utils.EPPServiceName(d.Name)
					By(fmt.Sprintf("checking EPP pods for %q", eppName))
					pods, err := clientset.CoreV1().Pods(caseNamespace).List(context.Background(), metav1.ListOptions{
						LabelSelector: fmt.Sprintf("inferencepool=%s", eppName),
					})
					Expect(err).NotTo(HaveOccurred())
					var runningEPP int
					for _, pod := range pods.Items {
						if pod.Status.Phase == "Running" {
							runningEPP++
						}
					}
					Expect(runningEPP).To(BeNumerically(">=", 1),
						"at least one EPP pod should be Running for %q", eppName)
				}
			})

			It("should have InferenceSet created with downstream resources", func() {
				dynClient, err := utils.GetDynamicClient()
				Expect(err).NotTo(HaveOccurred())

				for _, d := range suiteDeployments {
					name := d.Name
					By(fmt.Sprintf("verifying InferenceSet %q exists with correct spec", name))
					is, err := dynClient.Resource(utils.InferenceSetGVR).Namespace(caseNamespace).
						Get(context.Background(), name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred(), "InferenceSet %q should exist", name)
					Expect(is.GetName()).To(Equal(name))

					By(fmt.Sprintf("verifying InferenceSet %q preset.name == %q", name, d.Model))
					preset, found, _ := unstructured.NestedString(is.Object, "spec", "template", "inference", "preset", "name")
					Expect(found).To(BeTrue(), "spec.template.inference.preset.name should be set")
					Expect(preset).To(Equal(d.Model),
						"InferenceSet %q preset.name should match the explicitly-set model", name)

					// NOTE: We intentionally do not assert on
					// InferenceSet.status.readyReplicas (or the
					// InferenceSetReady condition) on the gpu-mocker setup.
					// Both are gated on Workspace pods having
					// WorkspaceConditionTypeSucceeded=True, which the
					// gpu-mocker never satisfies — it only patches the
					// original inference pod's Phase/PodIP. Real readiness
					// of this case is covered by:
					//   - Gateway connectivity smoke check (returns 200)
					//   - "shadow pods running" assertion below
					//   - "original inference pods patched to Running"
					//     assertion below
					// See https://github.com/kaito-project/kaito for the
					// InferenceSet→Workspace readiness chain.

					By(fmt.Sprintf("verifying InferencePool %q is auto-created", utils.InferencePoolName(name)))
					poolName := utils.InferencePoolName(name)
					pool, err := dynClient.Resource(utils.InferencePoolGVR).Namespace(caseNamespace).
						Get(context.Background(), poolName, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred(), "InferencePool %q should exist", poolName)
					Expect(pool.GetName()).To(Equal(poolName))

					By(fmt.Sprintf("verifying HTTPRoute %q exists", name+"-route"))
					_, err = dynClient.Resource(utils.HTTPRouteGVR).Namespace(caseNamespace).
						Get(context.Background(), name+"-route", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred(), "HTTPRoute %q should exist", name+"-route")
				}
			})
		})

		Context("HTTPRoute status", func() {
			It("should have HTTPRoutes with Accepted=True condition for each deployment", func() {
				dynClient, err := utils.GetDynamicClient()
				Expect(err).NotTo(HaveOccurred())

				for _, d := range suiteDeployments {
					routeName := d.Name + "-route"
					By(fmt.Sprintf("checking HTTPRoute %q has Accepted=True", routeName))

					route, err := dynClient.Resource(utils.HTTPRouteGVR).Namespace(caseNamespace).
						Get(context.Background(), routeName, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred(), "HTTPRoute %q should exist", routeName)

					status, ok := route.Object["status"].(map[string]interface{})
					Expect(ok).To(BeTrue(), "status should be present")

					parents, ok := status["parents"].([]interface{})
					Expect(ok).To(BeTrue(), "status.parents should be present")
					Expect(parents).NotTo(BeEmpty(), "status.parents should not be empty")

					parent := parents[0].(map[string]interface{})
					conditions, ok := parent["conditions"].([]interface{})
					Expect(ok).To(BeTrue(), "conditions should be present")

					var accepted bool
					for _, c := range conditions {
						cond := c.(map[string]interface{})
						if cond["type"] == "Accepted" && cond["status"] == "True" {
							accepted = true
							break
						}
					}
					Expect(accepted).To(BeTrue(), "HTTPRoute %q should have Accepted=True", routeName)
				}
			})
		})
	})

	Context("Fake node and shadow pod lifecycle", utils.GinkgoLabelInfra, func() {

		Context("Fake nodes", func() {
			It("should have fake nodes with correct labels", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				nodes, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{
					LabelSelector: "kaito.sh/fake-node=true",
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(nodes.Items).NotTo(BeEmpty(), "at least one fake node should exist")

				for _, node := range nodes.Items {
					By(fmt.Sprintf("validating fake node %q labels", node.Name))
					Expect(node.Labels).To(HaveKeyWithValue("kaito.sh/managed-by", "gpu-mocker"))
					Expect(node.Labels).To(HaveKeyWithValue(
						"node.kubernetes.io/exclude-from-external-load-balancers", "true"))
				}
			})

			It("should have fake nodes in Ready condition", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				nodes, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{
					LabelSelector: "kaito.sh/fake-node=true",
				})
				Expect(err).NotTo(HaveOccurred())

				for _, node := range nodes.Items {
					By(fmt.Sprintf("checking fake node %q Ready condition", node.Name))
					var ready bool
					for _, cond := range node.Status.Conditions {
						if cond.Type == "Ready" && cond.Status == "True" {
							ready = true
							break
						}
					}
					Expect(ready).To(BeTrue(), "fake node %q should be Ready", node.Name)
				}
			})
		})

		Context("Shadow pods", func() {
			It("should have shadow pods running in the shadow namespace", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				// Use field selector to skip stale Failed/Completed pods from
				// previous test runs that haven't been garbage-collected yet.
				Eventually(func() error {
					pods, err := clientset.CoreV1().Pods(caseNamespace).List(context.Background(), metav1.ListOptions{
						LabelSelector: "kaito.sh/managed-by=gpu-mocker",
						FieldSelector: "status.phase=Running",
					})
					if err != nil {
						return fmt.Errorf("failed to list shadow pods: %w", err)
					}
					if len(pods.Items) == 0 {
						return fmt.Errorf("no running shadow pods found in %s", caseNamespace)
					}
					for _, pod := range pods.Items {
						if _, ok := pod.Labels["kaito.sh/shadow-pod-for"]; !ok {
							return fmt.Errorf("shadow pod %q missing shadow-pod-for label", pod.Name)
						}
					}
					return nil
				}, 3*time.Minute, 10*time.Second).Should(Succeed(),
					"running shadow pods should exist in %s", caseNamespace)
			})

			It("should have shadow pods with both llm-d-inference-sim and tokenizer containers", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				pods, err := clientset.CoreV1().Pods(caseNamespace).List(context.Background(), metav1.ListOptions{
					LabelSelector: "kaito.sh/managed-by=gpu-mocker",
					FieldSelector: "status.phase=Running",
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(pods.Items).NotTo(BeEmpty())

				for _, pod := range pods.Items {
					By(fmt.Sprintf("checking containers in shadow pod %q", pod.Name))
					containerNames := make([]string, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
					for _, c := range pod.Spec.Containers {
						containerNames = append(containerNames, c.Name)
					}
					for _, c := range pod.Spec.InitContainers {
						containerNames = append(containerNames, c.Name)
					}
					Expect(containerNames).To(ContainElement("llm-d-inference-sim"),
						"shadow pod %q should have llm-d-inference-sim container", pod.Name)
					Expect(containerNames).To(ContainElement("uds-tokenizer"),
						"shadow pod %q should have uds-tokenizer container", pod.Name)
				}
			})
		})

		Context("Original pod status patching", func() {
			It("should have original inference pods patched to Running with shadow pod IPs", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				for _, d := range suiteDeployments {
					By(fmt.Sprintf("checking original pods for %q", d.Name))

					pods, err := clientset.CoreV1().Pods(caseNamespace).List(context.Background(), metav1.ListOptions{
						LabelSelector: fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", d.Name),
					})
					Expect(err).NotTo(HaveOccurred())
					Expect(pods.Items).NotTo(BeEmpty(),
						"inference pods for %q should exist", d.Name)

					for _, pod := range pods.Items {
						By(fmt.Sprintf("validating pod %q status", pod.Name))
						Expect(string(pod.Status.Phase)).To(Equal("Running"),
							"pod %q should be patched to Running", pod.Name)
						Expect(pod.Status.PodIP).NotTo(BeEmpty(),
							"pod %q should have a podIP from shadow pod", pod.Name)
						Expect(pod.Annotations).To(HaveKey("kaito.sh/shadow-pod-ref"),
							"pod %q should have shadow-pod-ref annotation", pod.Name)
					}
				}
			})
		})
	})

	Context("Garbage collection", utils.GinkgoLabelInfra, func() {

		Context("Fake node GC", func() {
			It("should delete orphaned fake nodes and leases when the NodeClaim is removed", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())
				dynClient, err := utils.GetDynamicClient()
				Expect(err).NotTo(HaveOccurred())

				ctx := context.Background()

				// Find a fake node hosting one of THIS case's original pods.
				// Fake nodes are cluster-scoped, so picking any fake node by
				// label alone (kaito.sh/fake-node=true) can target a node that
				// belongs to a different test case running in parallel — the
				// subsequent NodeClaim deletion would then disrupt that case's
				// pods (e.g. evicting a routing-phi inference pod) and surface
				// as flaky failures in unrelated specs (load distribution,
				// shadow pod GC). Scoping the target to a node that hosts a
				// pod in caseNamespace guarantees isolation between cases.
				deploymentName := caseDeployments[0].Name
				pods, err := clientset.CoreV1().Pods(caseNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", deploymentName),
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(pods.Items).NotTo(BeEmpty(),
					"need at least one original pod in %s for deployment %q", caseNamespace, deploymentName)

				var targetNodeName string
				for _, p := range pods.Items {
					if strings.HasPrefix(p.Spec.NodeName, "fake-") {
						targetNodeName = p.Spec.NodeName
						break
					}
				}
				Expect(targetNodeName).NotTo(BeEmpty(),
					"no original pod for %q is scheduled on a fake- node yet", deploymentName)

				targetNode, err := clientset.CoreV1().Nodes().Get(ctx, targetNodeName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "fake node %q should exist", targetNodeName)
				Expect(targetNode.Labels).To(HaveKeyWithValue("kaito.sh/fake-node", "true"),
					"node %q should be a fake node", targetNodeName)
				Expect(targetNode.Labels).To(HaveKeyWithValue("kaito.sh/managed-by", "gpu-mocker"),
					"node %q should be managed by gpu-mocker", targetNodeName)

				var ncName string
				for _, ref := range targetNode.OwnerReferences {
					if ref.Kind == "NodeClaim" {
						ncName = ref.Name
						break
					}
				}
				Expect(ncName).NotTo(BeEmpty(), "fake node %q should have a NodeClaim owner reference", targetNode.Name)

				By("verifying the lease has an OwnerReference to the NodeClaim")
				lease, err := clientset.CoordinationV1().Leases("kube-node-lease").Get(ctx, targetNode.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "lease %q should exist", targetNode.Name)
				var leaseOwnedByNC bool
				for _, ref := range lease.OwnerReferences {
					if ref.Kind == "NodeClaim" && ref.Name == ncName {
						leaseOwnedByNC = true
						break
					}
				}
				Expect(leaseOwnedByNC).To(BeTrue(),
					"lease %q should have an OwnerReference to NodeClaim %q", targetNode.Name, ncName)

				By(fmt.Sprintf("deleting NodeClaim %q (owner of fake node %q)", ncName, targetNode.Name))
				err = dynClient.Resource(utils.NodeClaimGVR).Delete(ctx, ncName, metav1.DeleteOptions{})
				Expect(err).NotTo(HaveOccurred())

				By("waiting for Kubernetes GC to delete the orphaned fake node")
				Eventually(func() bool {
					_, err := clientset.CoreV1().Nodes().Get(ctx, targetNode.Name, metav1.GetOptions{})
					return errors.IsNotFound(err)
				}, 2*time.Minute, 5*time.Second).Should(BeTrue(),
					"fake node %q should be garbage collected after NodeClaim %q deletion", targetNode.Name, ncName)

				By("verifying the associated lease is also deleted")
				Eventually(func() bool {
					_, err := clientset.CoordinationV1().Leases("kube-node-lease").Get(ctx, targetNode.Name, metav1.GetOptions{})
					return errors.IsNotFound(err)
				}, 30*time.Second, 5*time.Second).Should(BeTrue(),
					"lease %q should be garbage collected", targetNode.Name)
			})
		})

		Context("NodeClaim reconcile idempotency", func() {
			It("should not create duplicate fake Nodes or Leases when the same NodeClaim is re-applied", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())
				dynClient, err := utils.GetDynamicClient()
				Expect(err).NotTo(HaveOccurred())

				ctx := context.Background()

				// Find an existing NodeClaim to re-apply.
				nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
					LabelSelector: "kaito.sh/fake-node=true,kaito.sh/managed-by=gpu-mocker",
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(nodes.Items).NotTo(BeEmpty(), "need at least one fake node")

				targetNode := nodes.Items[0]
				var ncName string
				for _, ref := range targetNode.OwnerReferences {
					if ref.Kind == "NodeClaim" {
						ncName = ref.Name
						break
					}
				}
				Expect(ncName).NotTo(BeEmpty(),
					"fake node %q should have a NodeClaim owner reference", targetNode.Name)

				By("recording baseline fake node and lease counts")
				baselineNodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
					LabelSelector: "kaito.sh/fake-node=true,kaito.sh/managed-by=gpu-mocker",
				})
				Expect(err).NotTo(HaveOccurred())
				baselineNodeCount := len(baselineNodes.Items)

				baselineLeases, err := clientset.CoordinationV1().Leases("kube-node-lease").List(ctx, metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				// Count only leases that correspond to fake nodes.
				fakeNodeNames := make(map[string]bool)
				for _, n := range baselineNodes.Items {
					fakeNodeNames[n.Name] = true
				}
				var baselineLeaseCount int
				for _, l := range baselineLeases.Items {
					if fakeNodeNames[l.Name] {
						baselineLeaseCount++
					}
				}

				By(fmt.Sprintf("re-applying NodeClaim %q to trigger duplicate reconcile", ncName))
				nc, err := dynClient.Resource(utils.NodeClaimGVR).Get(ctx, ncName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				// Force a reconcile by updating a harmless annotation.
				annotations := nc.GetAnnotations()
				if annotations == nil {
					annotations = make(map[string]string)
				}
				annotations["e2e-test/idempotency-trigger"] = fmt.Sprintf("%d", time.Now().UnixNano())
				nc.SetAnnotations(annotations)
				_, err = dynClient.Resource(utils.NodeClaimGVR).Update(ctx, nc, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				// Allow reconcile to process.
				time.Sleep(10 * time.Second)

				By("verifying no duplicate fake Nodes were created")
				afterNodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
					LabelSelector: "kaito.sh/fake-node=true,kaito.sh/managed-by=gpu-mocker",
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(len(afterNodes.Items)).To(Equal(baselineNodeCount),
					"re-applying NodeClaim should not create duplicate fake nodes (before=%d, after=%d)",
					baselineNodeCount, len(afterNodes.Items))

				By("verifying no duplicate Leases were created")
				afterLeases, err := clientset.CoordinationV1().Leases("kube-node-lease").List(ctx, metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())
				afterFakeNodeNames := make(map[string]bool)
				for _, n := range afterNodes.Items {
					afterFakeNodeNames[n.Name] = true
				}
				var afterLeaseCount int
				for _, l := range afterLeases.Items {
					if afterFakeNodeNames[l.Name] {
						afterLeaseCount++
					}
				}
				Expect(afterLeaseCount).To(Equal(baselineLeaseCount),
					"re-applying NodeClaim should not create duplicate leases (before=%d, after=%d)",
					baselineLeaseCount, afterLeaseCount)
			})
		})

		Context("Shadow pod GC", func() {
			It("should delete shadow pods via native Kubernetes GC when the original pod is removed", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				ctx := context.Background()

				// Find a running shadow pod belonging to our case.
				shadowPods, err := clientset.CoreV1().Pods(caseNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: "kaito.sh/managed-by=gpu-mocker",
					FieldSelector: "status.phase=Running",
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(shadowPods.Items).NotTo(BeEmpty(), "need at least one running shadow pod")

				shadow := shadowPods.Items[0]

				By("verifying shadow pod has an OwnerReference to the original pod")
				Expect(shadow.OwnerReferences).NotTo(BeEmpty(),
					"shadow pod %q should have OwnerReferences", shadow.Name)
				var ownerPodName, ownerPodNS string
				for _, ref := range shadow.OwnerReferences {
					if ref.Kind == "Pod" {
						ownerPodName = ref.Name
						break
					}
				}
				Expect(ownerPodName).NotTo(BeEmpty(),
					"shadow pod %q should have an OwnerReference of kind Pod", shadow.Name)

				// Resolve the original pod namespace from annotation.
				ref, ok := shadow.Annotations["kaito.sh/original-pod"]
				Expect(ok).To(BeTrue(), "shadow pod %q should have kaito.sh/original-pod annotation", shadow.Name)
				parts := strings.SplitN(ref, "/", 2)
				Expect(parts).To(HaveLen(2), "annotation should be namespace/name")
				ownerPodNS = parts[0]

				// Record the shadow pod's UID so we can detect GC even if
				// the InferenceSet controller recreates the original pod
				// (and the ShadowPodReconciler in turn creates a new shadow
				// pod with the same deterministic name).
				oldShadowUID := shadow.UID

				By(fmt.Sprintf("deleting original pod %s/%s (owner of shadow %q)", ownerPodNS, ownerPodName, shadow.Name))
				// Force-delete: the original pod lives on a fake node with no
				// kubelet, so a graceful delete would leave it stuck in
				// Terminating indefinitely (no kubelet to confirm shutdown).
				// The GC only cascades once the owner is fully removed from
				// the API server, so we must use GracePeriodSeconds=0.
				err = clientset.CoreV1().Pods(ownerPodNS).Delete(ctx, ownerPodName, metav1.DeleteOptions{
					GracePeriodSeconds: new(int64),
				})
				Expect(err).NotTo(HaveOccurred())

				By("waiting for Kubernetes GC to delete the orphaned shadow pod")
				// The InferenceSet controller may recreate the original pod
				// after deletion. When that happens the ShadowPodReconciler
				// creates a new shadow pod with the same name but a different
				// UID (proving the old one was GC'd). Accept either outcome:
				//   - NotFound  → GC'd and not yet recreated
				//   - New UID   → GC'd and already recreated for the new original pod
				Eventually(func() bool {
					current, err := clientset.CoreV1().Pods(caseNamespace).Get(ctx, shadow.Name, metav1.GetOptions{})
					if errors.IsNotFound(err) {
						return true // shadow pod was deleted by GC
					}
					if err != nil {
						return false
					}
					return current.UID != oldShadowUID // recreated with a new UID ⇒ old one was GC'd
				}, 2*time.Minute, 5*time.Second).Should(BeTrue(),
					"shadow pod %q should be garbage collected after original pod deletion", shadow.Name)
			})
		})
	})

	Context("Unknown model handling", utils.GinkgoLabelRouting, func() {

		Context("Non-existent model request", func() {
			It("should return 404 with an OpenAI-compatible error for an unknown model", func() {
				// The catch-all `model-not-found-direct` EnvoyFilter is
				// provisioned per-namespace by the modelharness chart
				// (installed via EnsureNamespace) and patches an Envoy
				// `direct_response` (status 404 + OpenAI-compatible JSON) onto
				// the Gateway's virtual host as a catch-all route. No backend
				// Pod / Service is involved. The gpu-mocker case has
				// AuthAPIKeyEnabled=false, so no AuthorizationPolicy is
				// rendered and the probe needs no bearer token.
				resp, err := utils.SendChatCompletion(caseGatewayURL, "non-existent-model-xyz")
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(http.StatusNotFound))

				errResp, err := utils.ParseErrorResponse(resp)
				Expect(err).NotTo(HaveOccurred())
				Expect(errResp.ErrorCode()).To(Equal("model_not_found"))
				Expect(errResp.Error.Type).To(Equal("invalid_request_error"))
				Expect(errResp.Error.Message).NotTo(BeEmpty())
			})
		})
	})
})
