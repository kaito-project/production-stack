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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

const (
	shadowNamespace = "kaito-shadow"
	testNamespace   = "default"
	falconModel     = "falcon-7b-instruct"
	ministralModel  = "ministral-3-3b-instruct"
)

var modelNames = []string{falconModel, ministralModel}

var _ = Describe("GPU Mocker E2E", Ordered, func() {
	var ctx context.Context

	BeforeAll(func() {
		ctx = context.Background()
		utils.GetClusterClient(utils.TestingCluster)

		cl := utils.TestingCluster.KubeClient
		for _, model := range modelNames {
			cfg := utils.DefaultInferenceSetConfig(model)

			By(fmt.Sprintf("Creating InferenceSet for %s", model))
			err := utils.CreateInferenceSet(ctx, cl, cfg)
			if err != nil && !apierrors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred(), "failed to create InferenceSet for %s", model)
			}

			By(fmt.Sprintf("Waiting for InferencePool for %s", model))
			err = utils.WaitForInferenceSetReady(ctx, cl, cfg.Name, cfg.Namespace, utils.InferenceSetReadyTimeout)
			Expect(err).NotTo(HaveOccurred(), "InferenceSet %s not ready", model)

			By(fmt.Sprintf("Creating DestinationRule for %s", model))
			Eventually(func() error {
				err := utils.CreateDestinationRuleForInferenceSet(ctx, cl, cfg.Name, cfg.Namespace)
				if apierrors.IsAlreadyExists(err) {
					return nil
				}
				return err
			}, 1*time.Minute, 5*time.Second).Should(Succeed(),
				"failed to create DestinationRule for %s", model)

			By(fmt.Sprintf("Creating HTTPRoute for %s", model))
			Eventually(func() error {
				err := utils.CreateHTTPRouteForInferenceSet(ctx, cl, cfg.Name, cfg.Namespace, cfg.GatewayName)
				if apierrors.IsAlreadyExists(err) {
					return nil
				}
				return err
			}, 1*time.Minute, 5*time.Second).Should(Succeed(),
				"failed to create HTTPRoute for %s", model)
		}

		// Wait for the GPU mocker to fully reconcile: fake nodes, shadow pods,
		// and original pod status patching must all complete before the gateway
		// can route traffic to inference pods.
		clientset, err := utils.GetK8sClientset()
		Expect(err).NotTo(HaveOccurred())

		for _, model := range modelNames {
			By(fmt.Sprintf("Waiting for inference pods for %s to be Running", model))
			Eventually(func() error {
				// Use the InferenceSet's matchLabels selector to find pods.
				pods, err := clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("apps=%s", model),
				})
				if err != nil {
					return fmt.Errorf("failed to list pods: %w", err)
				}
				if len(pods.Items) == 0 {
					return fmt.Errorf("no inference pods found for %s", model)
				}
				for _, pod := range pods.Items {
					if pod.Status.Phase != "Running" {
						return fmt.Errorf("pod %s is %s, not Running", pod.Name, pod.Status.Phase)
					}
					if pod.Status.PodIP == "" {
						return fmt.Errorf("pod %s has no PodIP yet", pod.Name)
					}
				}
				return nil
			}, 5*time.Minute, 10*time.Second).Should(Succeed(),
				"inference pods for %s should be Running with PodIPs", model)
		}
	})

	AfterAll(func() {
		ctx = context.Background()
		for _, model := range modelNames {
			By(fmt.Sprintf("Cleaning up InferenceSet with routing for %s", model))
			err := utils.CleanupInferenceSetWithRouting(ctx, utils.TestingCluster.KubeClient, model, testNamespace)
			if err != nil {
				GinkgoWriter.Printf("Cleanup warning for %s: %v\n", model, err)
			}
		}
	})

	Context("GPU Node Mocker", utils.GinkgoLabelSmoke, func() {

		Context("Framework validation", utils.GinkgoLabelSmoke, func() {
			It("should have the test framework properly initialised", func() {
				Expect(true).To(BeTrue(), "framework sanity check")
			})
		})

		Context("Gateway connectivity", utils.GinkgoLabelSmoke, func() {
			It("should be reachable and return a response", func() {
				gatewayURL, err := utils.GetGatewayURL()
				Expect(err).NotTo(HaveOccurred(), "failed to discover gateway URL")

				// Retry with backoff — BBR/EPP ext_proc filters may need time
				// to establish gRPC connections after cluster setup.
				Eventually(func() error {
					resp, err := utils.SendChatCompletion(gatewayURL, falconModel)
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
					"gateway should be reachable and return 200")
			})
		})
	})

	Context("InferenceSet and InferencePool lifecycle", utils.GinkgoLabelInfra, func() {

		Context("InferenceSet lifecycle", func() {
			It("should have InferenceSet created with downstream resources", func() {
				dynClient, err := utils.GetDynamicClient()
				Expect(err).NotTo(HaveOccurred())

				for _, name := range modelNames {
					By(fmt.Sprintf("verifying InferenceSet %q exists with correct spec", name))
					is, err := dynClient.Resource(utils.InferenceSetGVR).Namespace(testNamespace).
						Get(context.Background(), name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred(), "InferenceSet %q should exist", name)
					Expect(is.GetName()).To(Equal(name))

					By(fmt.Sprintf("verifying InferenceSet %q readyReplicas matches desired", name))
					spec := is.Object["spec"].(map[string]interface{})
					desired, ok := spec["replicas"]
					Expect(ok).To(BeTrue(), "spec.replicas should be set")

					status, ok := is.Object["status"].(map[string]interface{})
					Expect(ok).To(BeTrue(), "status should be present")
					ready, ok := status["readyReplicas"]
					Expect(ok).To(BeTrue(), "status.readyReplicas should be present")
					Expect(ready).To(BeEquivalentTo(desired),
						"InferenceSet %q readyReplicas should match desired replicas", name)

					By(fmt.Sprintf("verifying InferencePool %q is auto-created", utils.InferencePoolName(name)))
					poolName := utils.InferencePoolName(name)
					pool, err := dynClient.Resource(utils.InferencePoolGVR).Namespace(testNamespace).
						Get(context.Background(), poolName, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred(), "InferencePool %q should exist", poolName)
					Expect(pool.GetName()).To(Equal(poolName))

					By(fmt.Sprintf("verifying HTTPRoute %q exists", name+"-route"))
					_, err = dynClient.Resource(utils.HTTPRouteGVR).Namespace(testNamespace).
						Get(context.Background(), name+"-route", metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred(), "HTTPRoute %q should exist", name+"-route")

					By(fmt.Sprintf("verifying DestinationRule %q exists", utils.EPPServiceName(name)))
					_, err = dynClient.Resource(utils.DestinationRuleGVR).Namespace(testNamespace).
						Get(context.Background(), utils.EPPServiceName(name), metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred(), "DestinationRule %q should exist", utils.EPPServiceName(name))
				}
			})

			It("should have EPP pods running for each InferencePool", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				for _, name := range modelNames {
					eppName := name + "-inferencepool-epp"
					By(fmt.Sprintf("checking EPP pods for %q", eppName))
					pods, err := clientset.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{
						LabelSelector: fmt.Sprintf("app=%s", eppName),
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
		})

		Context("HTTPRoute status", func() {
			It("should have HTTPRoutes with Accepted=True condition for each model", func() {
				dynClient, err := utils.GetDynamicClient()
				Expect(err).NotTo(HaveOccurred())

				for _, name := range modelNames {
					routeName := name + "-route"
					By(fmt.Sprintf("checking HTTPRoute %q has Accepted=True", routeName))

					route, err := dynClient.Resource(utils.HTTPRouteGVR).Namespace(testNamespace).
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

		Context("DestinationRules", func() {
			It("should have DestinationRules with trafficPolicy.tls.mode=SIMPLE for each EPP service", func() {
				dynClient, err := utils.GetDynamicClient()
				Expect(err).NotTo(HaveOccurred())

				for _, name := range modelNames {
					drName := utils.EPPServiceName(name)
					By(fmt.Sprintf("checking DestinationRule %q exists with SIMPLE TLS", drName))
					dr, err := dynClient.Resource(utils.DestinationRuleGVR).Namespace(testNamespace).
						Get(context.Background(), drName, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred(), "DestinationRule %q should exist", drName)
					Expect(dr.GetName()).To(Equal(drName))

					tlsMode, found, _ := unstructured.NestedString(dr.Object, "spec", "trafficPolicy", "tls", "mode")
					Expect(found).To(BeTrue(), "DestinationRule %q should have spec.trafficPolicy.tls.mode", drName)
					Expect(tlsMode).To(Equal("SIMPLE"),
						"DestinationRule %q trafficPolicy.tls.mode should be SIMPLE", drName)
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

				Eventually(func() error {
					pods, err := clientset.CoreV1().Pods(shadowNamespace).List(context.Background(), metav1.ListOptions{
						LabelSelector: "kaito.sh/managed-by=gpu-mocker",
					})
					if err != nil {
						return fmt.Errorf("failed to list shadow pods: %w", err)
					}
					if len(pods.Items) == 0 {
						return fmt.Errorf("no shadow pods found in %s", shadowNamespace)
					}
					for _, pod := range pods.Items {
						if pod.Status.Phase != "Running" {
							return fmt.Errorf("shadow pod %q is %s, not Running", pod.Name, pod.Status.Phase)
						}
						if _, ok := pod.Labels["kaito.sh/shadow-pod-for"]; !ok {
							return fmt.Errorf("shadow pod %q missing shadow-pod-for label", pod.Name)
						}
					}
					return nil
				}, 3*time.Minute, 10*time.Second).Should(Succeed(),
					"all shadow pods should be Running in %s", shadowNamespace)
			})

			It("should have shadow pods with both llm-d-inference-sim and tokenizer containers", func() {
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

				for _, model := range modelNames {
					By(fmt.Sprintf("checking original pods for %q", model))

					pods, err := clientset.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{
						LabelSelector: fmt.Sprintf("apps=%s", model),
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

	Context("Unknown model handling", utils.GinkgoLabelRouting, func() {

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
})
