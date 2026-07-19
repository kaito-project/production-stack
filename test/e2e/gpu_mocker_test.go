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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

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

			It("should have shadow pods with the llm-d-inference-sim container and no tokenizer sidecar", func() {
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
					// The UDS tokenizer sidecar was removed: llm-d-inference-sim
					// forces its built-in (dummy) tokenizer via
					// force-dummy-tokenizer, so no external tokenizer container exists.
					Expect(containerNames).NotTo(ContainElement("uds-tokenizer"),
						"shadow pod %q should not have a uds-tokenizer sidecar", pod.Name)
				}
			})

			It("should create a shadow pod for a KAITO Workspace (StatefulSet) pod", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				ctx := context.Background()

				// The gpu-mocker case deploys InferenceSet (modeldeployment)
				// pods, which already provisioned at least one Ready fake node.
				// KAITO Workspace pods (owned by a StatefulSet, labelled
				// kaito.sh/workspace) take a different provisioning path but
				// must still get a shadow pod. Synthesize a Workspace-style pod
				// bound to one of THIS case's fake nodes and assert the
				// gpu-node-mocker mirrors it just like an InferenceSet pod.
				//
				// Scoping the target to a fake node that hosts a pod in
				// caseNamespace keeps cases running in parallel isolated.
				deploymentName := caseDeployments[0].Name
				origPods, err := clientset.CoreV1().Pods(caseNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", deploymentName),
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(origPods.Items).NotTo(BeEmpty(),
					"need at least one original pod in %s for deployment %q", caseNamespace, deploymentName)

				var fakeNodeName string
				for _, p := range origPods.Items {
					if strings.HasPrefix(p.Spec.NodeName, "fake-") {
						fakeNodeName = p.Spec.NodeName
						break
					}
				}
				Expect(fakeNodeName).NotTo(BeEmpty(),
					"no original pod for %q is scheduled on a fake- node yet", deploymentName)

				const wsName = "e2e-workspace-shadow"
				wsPodName := wsName + "-0"

				wsPod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      wsPodName,
						Namespace: caseNamespace,
						// KAITO Workspace StatefulSet pods carry this label
						// instead of inferenceset.kaito.sh/created-by.
						Labels: map[string]string{"kaito.sh/workspace": wsName},
					},
					Spec: corev1.PodSpec{
						// Bind directly to the fake node (no kubelet runs there)
						// so the pod stays Pending — exactly the state the
						// ShadowPodReconciler mirrors. A blanket toleration
						// clears the fake node's sku=gpu taint.
						NodeName:    fakeNodeName,
						Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
						Containers: []corev1.Container{{
							Name:  "model",
							Image: "mcr.microsoft.com/oss/kubernetes/pause:3.6",
							Args: []string{
								"--model", "microsoft/phi-4-mini-instruct",
								"--served-model-name", "phi-4-mini-instruct",
							},
							Ports: []corev1.ContainerPort{{ContainerPort: 5000}},
						}},
					},
				}

				By(fmt.Sprintf("creating synthetic Workspace pod %s/%s bound to fake node %q",
					caseNamespace, wsPodName, fakeNodeName))
				_, err = clientset.CoreV1().Pods(caseNamespace).Create(ctx, wsPod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				// Deleting the original pod cascades the shadow pod and its
				// ConfigMap via native Kubernetes GC (OwnerReference).
				DeferCleanup(func() {
					_ = clientset.CoreV1().Pods(caseNamespace).Delete(
						context.Background(), wsPodName, metav1.DeleteOptions{})
				})

				shadowName := "shadow-" + caseNamespace + "-" + wsPodName

				By(fmt.Sprintf("waiting for shadow pod %q to be created and Running", shadowName))
				Eventually(func() error {
					shadow, err := clientset.CoreV1().Pods(caseNamespace).Get(ctx, shadowName, metav1.GetOptions{})
					if err != nil {
						return fmt.Errorf("get shadow pod %q: %w", shadowName, err)
					}
					if shadow.Labels["kaito.sh/managed-by"] != "gpu-mocker" {
						return fmt.Errorf("shadow pod %q missing managed-by label", shadowName)
					}
					if _, ok := shadow.Labels["kaito.sh/shadow-pod-for"]; !ok {
						return fmt.Errorf("shadow pod %q missing shadow-pod-for label", shadowName)
					}
					// The ownership label drives modelharness NetworkPolicy
					// selection and must be present on Workspace shadow pods too.
					if shadow.Labels["kaito.sh/owned-by"] != "modeldeployment" {
						return fmt.Errorf("shadow pod %q missing owned-by label", shadowName)
					}
					var ownedByWsPod bool
					for _, ref := range shadow.OwnerReferences {
						if ref.Kind == "Pod" && ref.Name == wsPodName {
							ownedByWsPod = true
							break
						}
					}
					if !ownedByWsPod {
						return fmt.Errorf("shadow pod %q should be owned by Workspace pod %q", shadowName, wsPodName)
					}
					if shadow.Status.Phase != corev1.PodRunning || shadow.Status.PodIP == "" {
						return fmt.Errorf("shadow pod %q not Running yet (phase=%s)", shadowName, shadow.Status.Phase)
					}
					return nil
				}, 3*time.Minute, 10*time.Second).Should(Succeed(),
					"gpu-node-mocker should create a Running shadow pod for the Workspace pod")

				By("verifying the original Workspace pod is patched to Running with the shadow pod IP")
				Eventually(func() error {
					orig, err := clientset.CoreV1().Pods(caseNamespace).Get(ctx, wsPodName, metav1.GetOptions{})
					if err != nil {
						return err
					}
					if orig.Status.Phase != corev1.PodRunning {
						return fmt.Errorf("workspace pod %q phase = %s, want Running", wsPodName, orig.Status.Phase)
					}
					if orig.Status.PodIP == "" {
						return fmt.Errorf("workspace pod %q has no podIP", wsPodName)
					}
					if _, ok := orig.Annotations["kaito.sh/shadow-pod-ref"]; !ok {
						return fmt.Errorf("workspace pod %q missing shadow-pod-ref annotation", wsPodName)
					}
					return nil
				}, 2*time.Minute, 10*time.Second).Should(Succeed(),
					"original Workspace pod should be patched to Running with the shadow pod IP")
			})

			It("should self-heal a shadow pod that is deleted for a Running original pod", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				ctx := context.Background()

				// Pick an original inference pod already adopted and patched to
				// Running on a fake node in THIS case's namespace (parallel-
				// isolation pattern). Deleting its shadow pod faithfully
				// simulates the real AKS node hosting the shadow pod being
				// recreated: the shadow pod carries an OwnerReference to the
				// original pod (not vice-versa), so the original is left stuck
				// in a patched-Running state pointing at a dead IP.
				deploymentName := caseDeployments[0].Name
				origPods, err := clientset.CoreV1().Pods(caseNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", deploymentName),
				})
				Expect(err).NotTo(HaveOccurred())

				var origName string
				for _, p := range origPods.Items {
					if strings.HasPrefix(p.Spec.NodeName, "fake-") &&
						p.Status.Phase == corev1.PodRunning &&
						p.Annotations["kaito.sh/shadow-pod-ref"] != "" {
						origName = p.Name
						break
					}
				}
				Expect(origName).NotTo(BeEmpty(),
					"need an adopted Running original pod on a fake node for %q", deploymentName)

				shadowName := "shadow-" + caseNamespace + "-" + origName

				// Record the current shadow pod's UID so we can prove a *fresh*
				// one is created rather than the old one lingering.
				oldShadow, err := clientset.CoreV1().Pods(caseNamespace).Get(ctx, shadowName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "shadow pod %q should exist before deletion", shadowName)
				oldUID := oldShadow.UID

				By(fmt.Sprintf("deleting shadow pod %q to simulate the real node being recreated", shadowName))
				err = clientset.CoreV1().Pods(caseNamespace).Delete(ctx, shadowName,
					metav1.DeleteOptions{GracePeriodSeconds: ptr.To(int64(0))})
				Expect(err).NotTo(HaveOccurred())

				By("waiting for the controller to recreate a fresh Running shadow pod")
				Eventually(func() error {
					s, err := clientset.CoreV1().Pods(caseNamespace).Get(ctx, shadowName, metav1.GetOptions{})
					if err != nil {
						return fmt.Errorf("get shadow pod %q: %w", shadowName, err)
					}
					if s.UID == oldUID {
						return fmt.Errorf("shadow pod %q still has the old UID; not recreated yet", shadowName)
					}
					if s.Status.Phase != corev1.PodRunning || s.Status.PodIP == "" {
						return fmt.Errorf("recreated shadow pod %q not Running yet (phase=%s)", shadowName, s.Status.Phase)
					}
					return nil
				}, 3*time.Minute, 5*time.Second).Should(Succeed(),
					"a fresh shadow pod should be recreated and become Running")

				By("verifying the original pod is re-patched to the recreated shadow pod IP")
				Eventually(func() error {
					newShadow, err := clientset.CoreV1().Pods(caseNamespace).Get(ctx, shadowName, metav1.GetOptions{})
					if err != nil {
						return err
					}
					orig, err := clientset.CoreV1().Pods(caseNamespace).Get(ctx, origName, metav1.GetOptions{})
					if err != nil {
						return err
					}
					if orig.Status.Phase != corev1.PodRunning {
						return fmt.Errorf("original pod %q phase = %s, want Running", origName, orig.Status.Phase)
					}
					if orig.Status.PodIP != newShadow.Status.PodIP {
						return fmt.Errorf("original pod %q podIP = %q, want recreated shadow IP %q",
							origName, orig.Status.PodIP, newShadow.Status.PodIP)
					}
					return nil
				}, 2*time.Minute, 5*time.Second).Should(Succeed(),
					"original pod should be re-patched to the recreated shadow pod's IP")
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

		Context("Terminating pod reaping", func() {
			It("should force-delete a KAITO pod stuck Terminating on a fake node", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				ctx := context.Background()

				// A pod bound to a fake node has no real kubelet to finalize
				// deletion. A normal delete sets metadata.deletionTimestamp and
				// then waits forever for the (absent) kubelet, so the pod hangs
				// in Terminating while still holding the fake node's single
				// nvidia.com/gpu — blocking the replacement pod (Pending
				// deadlock). The FakeNodePodReaper stands in for the missing
				// kubelet: once the grace period elapses it force-deletes the
				// pod. Reaching NotFound on a fake-node pod is therefore proof
				// the reaper ran — without it the pod would hang indefinitely.
				//
				// Scope the target to a fake node hosting a pod in caseNamespace
				// so cases running in parallel stay isolated.
				deploymentName := caseDeployments[0].Name
				origPods, err := clientset.CoreV1().Pods(caseNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", deploymentName),
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(origPods.Items).NotTo(BeEmpty(),
					"need at least one original pod in %s for deployment %q", caseNamespace, deploymentName)

				var fakeNodeName string
				for _, p := range origPods.Items {
					if strings.HasPrefix(p.Spec.NodeName, "fake-") {
						fakeNodeName = p.Spec.NodeName
						break
					}
				}
				Expect(fakeNodeName).NotTo(BeEmpty(),
					"no original pod for %q is scheduled on a fake- node yet", deploymentName)

				const wsName = "e2e-reaper-terminating"
				wsPodName := wsName + "-0"
				gracePeriod := int64(30)

				wsPod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      wsPodName,
						Namespace: caseNamespace,
						// KAITO Workspace StatefulSet pods carry this label; the
						// reaper gates on it (or inferenceset.kaito.sh/created-by).
						Labels: map[string]string{"kaito.sh/workspace": wsName},
					},
					Spec: corev1.PodSpec{
						// Bind directly to the fake node (no kubelet runs there).
						NodeName:    fakeNodeName,
						Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
						Containers: []corev1.Container{{
							Name:  "model",
							Image: "mcr.microsoft.com/oss/kubernetes/pause:3.6",
							Args: []string{
								"--model", "microsoft/phi-4-mini-instruct",
								"--served-model-name", "phi-4-mini-instruct",
							},
							Ports: []corev1.ContainerPort{{ContainerPort: 5000}},
						}},
					},
				}

				By(fmt.Sprintf("creating synthetic Workspace pod %s/%s bound to fake node %q",
					caseNamespace, wsPodName, fakeNodeName))
				_, err = clientset.CoreV1().Pods(caseNamespace).Create(ctx, wsPod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				// Safety net if the reaper does not act or an assertion fails
				// midway: force-remove the pod so it never leaks into other specs.
				DeferCleanup(func() {
					_ = clientset.CoreV1().Pods(caseNamespace).Delete(
						context.Background(), wsPodName,
						metav1.DeleteOptions{GracePeriodSeconds: ptr.To(int64(0))})
				})

				// Let the ShadowPodReconciler drive the pod to Running, mirroring
				// a live KAITO inference pod occupying the fake node's GPU.
				By("waiting for the pod to be patched to Running by the shadow controller")
				Eventually(func() error {
					p, err := clientset.CoreV1().Pods(caseNamespace).Get(ctx, wsPodName, metav1.GetOptions{})
					if err != nil {
						return err
					}
					if p.Status.Phase != corev1.PodRunning {
						return fmt.Errorf("pod %q phase = %s, want Running", wsPodName, p.Status.Phase)
					}
					return nil
				}, 3*time.Minute, 10*time.Second).Should(Succeed(),
					"synthetic Workspace pod should be patched to Running before deletion")

				By(fmt.Sprintf("deleting the pod with a %ds grace period", gracePeriod))
				err = clientset.CoreV1().Pods(caseNamespace).Delete(ctx, wsPodName,
					metav1.DeleteOptions{GracePeriodSeconds: &gracePeriod})
				Expect(err).NotTo(HaveOccurred())

				By("verifying the pod first hangs in Terminating (deletionTimestamp set, still present)")
				Eventually(func() error {
					p, err := clientset.CoreV1().Pods(caseNamespace).Get(ctx, wsPodName, metav1.GetOptions{})
					if err != nil {
						return fmt.Errorf("pod not yet observed Terminating: %w", err)
					}
					if p.DeletionTimestamp == nil {
						return fmt.Errorf("pod %q has no deletionTimestamp yet", wsPodName)
					}
					return nil
				}, 15*time.Second, 1*time.Second).Should(Succeed(),
					"pod should enter Terminating on the fake node before the grace period elapses")

				By("waiting for the FakeNodePodReaper to force-delete the pod after the grace period")
				Eventually(func() bool {
					_, err := clientset.CoreV1().Pods(caseNamespace).Get(ctx, wsPodName, metav1.GetOptions{})
					return errors.IsNotFound(err)
				}, 2*time.Minute, 5*time.Second).Should(BeTrue(),
					"FakeNodePodReaper should force-delete the pod stuck Terminating on fake node %q", fakeNodeName)
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
				err = clientset.CoreV1().Pods(ownerPodNS).Delete(ctx, ownerPodName, metav1.DeleteOptions{})
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

				// The present-but-unmatched half of the split catch-all
				// names the gateway as the at-fault component.
				Expect(resp.Header.Get("x-kaito-error-source")).To(Equal("gateway"))

				errResp, err := utils.ParseErrorResponse(resp)
				Expect(err).NotTo(HaveOccurred())
				Expect(errResp.ErrorCode()).To(Equal("model_not_found"))
				Expect(errResp.Error.Type).To(Equal("invalid_request_error"))
				Expect(errResp.Error.Message).NotTo(BeEmpty())
			})
		})
	})
})
