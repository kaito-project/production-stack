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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

const (
	shadowNamespace = "kaito-shadow"
	testNamespace   = "default"
)

var modelNames = []string{"falcon-7b-instruct", "ministral-3-3b-instruct"}

var _ = Describe("GPU Node Mocker", utils.GinkgoLabelSmoke, func() {

	Context("Framework validation", utils.GinkgoLabelSmoke, func() {
		It("should have the test framework properly initialised", func() {
			Expect(true).To(BeTrue(), "framework sanity check")
		})

		It("should have e2e utility constants defined", func() {
			Expect(utils.E2eNamespace).To(Equal("production-stack-e2e"))
			Expect(utils.PollInterval).To(BeNumerically(">", 0))
			Expect(utils.PollTimeout).To(BeNumerically(">", 0))
		})
	})
})

var _ = Describe("InferenceSet and InferencePool lifecycle", utils.GinkgoLabelInfra, func() {

	Context("InferenceSet resources", func() {
		It("should have InferenceSets deployed in the cluster", func() {
			dynClient, err := utils.GetDynamicClient()
			Expect(err).NotTo(HaveOccurred())

			for _, name := range modelNames {
				By(fmt.Sprintf("checking InferenceSet %q exists", name))
				is, err := dynClient.Resource(utils.InferenceSetGVR).Namespace(testNamespace).
					Get(context.Background(), name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "InferenceSet %q should exist", name)
				Expect(is.GetName()).To(Equal(name))
			}
		})

		It("should have InferenceSets with ready replicas matching desired count", func() {
			dynClient, err := utils.GetDynamicClient()
			Expect(err).NotTo(HaveOccurred())

			for _, name := range modelNames {
				By(fmt.Sprintf("checking InferenceSet %q replicas", name))
				is, err := dynClient.Resource(utils.InferenceSetGVR).Namespace(testNamespace).
					Get(context.Background(), name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				spec := is.Object["spec"].(map[string]interface{})
				desired, ok := spec["replicas"]
				Expect(ok).To(BeTrue(), "spec.replicas should be set")

				status, ok := is.Object["status"].(map[string]interface{})
				Expect(ok).To(BeTrue(), "status should be present")
				ready, ok := status["readyReplicas"]
				Expect(ok).To(BeTrue(), "status.readyReplicas should be present")

				Expect(ready).To(BeEquivalentTo(desired),
					"InferenceSet %q readyReplicas should match desired replicas", name)
			}
		})
	})

	Context("InferencePool resources", func() {
		It("should have auto-created an InferencePool for each InferenceSet", func() {
			dynClient, err := utils.GetDynamicClient()
			Expect(err).NotTo(HaveOccurred())

			for _, name := range modelNames {
				poolName := name + "-inferencepool"
				By(fmt.Sprintf("checking InferencePool %q exists", poolName))
				pool, err := dynClient.Resource(utils.InferencePoolGVR).Namespace(testNamespace).
					Get(context.Background(), poolName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred(), "InferencePool %q should exist", poolName)
				Expect(pool.GetName()).To(Equal(poolName))
			}
		})

		It("should have EPP pods running for each InferencePool", func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			for _, name := range modelNames {
				eppName := name + "-inferencepool-epp"
				By(fmt.Sprintf("checking EPP pods for %q", eppName))
				pods, err := clientset.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{})
				Expect(err).NotTo(HaveOccurred())

				var runningEPP int
				for _, pod := range pods.Items {
					if pod.Name == eppName || (len(pod.Name) > len(eppName) && pod.Name[:len(eppName)+1] == eppName+"-") {
						if pod.Status.Phase == "Running" {
							runningEPP++
						}
					}
				}
				Expect(runningEPP).To(BeNumerically(">=", 1),
					"at least one EPP pod should be Running for %q", eppName)
			}
		})
	})
})

var _ = Describe("Fake node and shadow pod lifecycle", utils.GinkgoLabelInfra, func() {

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

			pods, err := clientset.CoreV1().Pods(shadowNamespace).List(context.Background(), metav1.ListOptions{
				LabelSelector: "kaito.sh/managed-by=gpu-mocker",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(pods.Items).NotTo(BeEmpty(), "shadow pods should exist in %s", shadowNamespace)

			for _, pod := range pods.Items {
				By(fmt.Sprintf("checking shadow pod %q status", pod.Name))
				Expect(pod.Status.Phase).To(Equal("Running"),
					"shadow pod %q should be Running", pod.Name)
				Expect(pod.Labels).To(HaveKey("kaito.sh/shadow-pod-for"),
					"shadow pod %q should have shadow-pod-for label", pod.Name)
			}
		})

		It("should have shadow pods with both inference-sim and tokenizer containers", func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			pods, err := clientset.CoreV1().Pods(shadowNamespace).List(context.Background(), metav1.ListOptions{
				LabelSelector: "kaito.sh/managed-by=gpu-mocker",
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
				Expect(containerNames).To(ContainElement("inference-sim"),
					"shadow pod %q should have inference-sim container", pod.Name)
				Expect(containerNames).To(ContainElement("uds-tokenizer"),
					"shadow pod %q should have uds-tokenizer container", pod.Name)
			}
		})
	})

	Context("Original pod status patching", func() {
		It("should have original inference pods patched to Running with shadow pod IPs", func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			for _, model := range modelNames {
				label := fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", model)
				By(fmt.Sprintf("checking original pods for %q", model))

				pods, err := clientset.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{
					LabelSelector: label,
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(pods.Items).NotTo(BeEmpty(),
					"inference pods for %q should exist", model)

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

var _ = Describe("Unknown model handling", utils.GinkgoLabelRouting, func() {

	Context("Non-existent model request", func() {
		It("should return 404 with an OpenAI-compatible error for an unknown model", func() {
			gatewayURL, err := utils.GetGatewayURL()
			Expect(err).NotTo(HaveOccurred())

			resp, err := utils.SendChatCompletion(gatewayURL, "non-existent-model-xyz")
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusNotFound))

			errResp, err := utils.ParseErrorResponse(resp)
			Expect(err).NotTo(HaveOccurred())
			Expect(errResp.Error.Code).To(Equal("model_not_found"))
			Expect(errResp.Error.Type).To(Equal("invalid_request_error"))
			Expect(errResp.Error.Message).NotTo(BeEmpty())
		})
	})
})
