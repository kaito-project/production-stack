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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
	"github.com/kaito-project/production-stack/test/e2e/utils"
)

const scaleToZeroProvisioningMargin = 2 * time.Minute

var _ = Describe("InferenceSet Scale To Zero",
	Ordered, utils.GinkgoLabelScaling, utils.GinkgoLabelScaleToZero,
	utils.GinkgoLabelNightly, utils.GinkgoLabelStandardK8sOnly, func() {

		var (
			ctx       context.Context
			values    deploy.ModelDeploymentValues
			gateway   deploy.GatewayEndpoint
			keda      utils.KEDAParams
			coldLoad  *utils.LoadGenerator
			namespace string
			modelName string
		)

		BeforeAll(func() {
			ctx = context.Background()
			values = CaseDeployments[CaseScaleToZero][0]
			namespace, modelName = values.Namespace, values.Name

			Expect(utils.EnsureNamespace(ctx, namespace)).To(Succeed())
			DeferCleanup(func() {
				if coldLoad != nil {
					coldLoad.Stop()
				}
				Expect(utils.CleanupDeploymentsAndNamespace(context.Background(), []deploy.ModelDeploymentValues{values}, namespace)).To(Succeed())
			})

			var err error
			gateway, err = utils.OpenGateway(ctx, namespace, CaseGatewayName(CaseScaleToZero))
			Expect(err).NotTo(HaveOccurred())

			warmValues := values
			warmValues.Replicas = 1
			warmValues.EnableScaling = false
			Expect(utils.InstallModelDeployment(ctx, warmValues)).To(Succeed())
			Expect(utils.WaitForInferenceSetReady(ctx, warmValues, utils.InferenceSetReadyTimeout)).To(Succeed())
			Eventually(func() error {
				return utils.CheckChatSuccess(ctx, gateway, modelName)
			}, utils.InferenceSetReadyTimeout, 10*time.Second).Should(Succeed())

			Expect(utils.UpgradeModelDeployment(ctx, values)).To(Succeed())
			Eventually(func(g Gomega) {
				dynClient, err := utils.GetDynamicClient()
				g.Expect(err).NotTo(HaveOccurred())
				scaledObject, err := dynClient.Resource(utils.ScaledObjectGVR).Namespace(namespace).
					Get(ctx, modelName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				minReplicas, found, err := unstructured.NestedInt64(scaledObject.Object, "spec", "minReplicaCount")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue())
				g.Expect(minReplicas).To(Equal(int64(0)))
				maxReplicas, found, err := unstructured.NestedInt64(scaledObject.Object, "spec", "maxReplicaCount")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue())
				g.Expect(maxReplicas).To(Equal(int64(1)))
				g.Expect(scaledObjectReady(scaledObject.Object)).To(BeTrue(), "ScaledObject conditions: %v", scaledObject.Object["status"])
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			keda, err = utils.GetKEDAParams(ctx, modelName, namespace)
			Expect(err).NotTo(HaveOccurred())
		})

		It("parks an idle warm model while leaving EPP ready", func() {
			Eventually(func(g Gomega) {
				replicas, err := utils.GetInferenceSetReplicas(ctx, modelName, namespace)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(replicas).To(Equal(int32(0)))

				clientset, err := utils.GetK8sClientset()
				g.Expect(err).NotTo(HaveOccurred())
				pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: values.InferencePodSelector()})
				g.Expect(err).NotTo(HaveOccurred())
				for _, pod := range pods.Items {
					g.Expect(podReady(&pod)).To(BeFalse(), "model pod %s remains Ready at zero replicas", pod.Name)
				}
			}, keda.ScaleDownTotalWait+keda.CooldownPeriod+scaleToZeroProvisioningMargin, 5*time.Second).Should(Succeed())

			Expect(assertEPPReady(ctx, namespace, modelName)).To(Succeed())
		})

		It("activates from sustained EPP demand and serves traffic", func() {
			coldLoad = &utils.LoadGenerator{
				Gateway: gateway, Model: modelName, Prompt: "hello", Concurrency: 2,
			}
			coldLoad.Start(ctx)

			Eventually(func() (int32, error) {
				return utils.GetInferenceSetReplicas(ctx, modelName, namespace)
			}, keda.ScaleUpTotalWait+scaleToZeroProvisioningMargin, 5*time.Second).Should(Equal(int32(1)))

			Eventually(func(g Gomega) {
				replicas, err := utils.GetInferenceSetReplicas(ctx, modelName, namespace)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(replicas).To(Equal(int32(1)), "replicas returned to zero during provisioning")
				g.Expect(utils.WaitForInferenceSetReady(ctx, values, 10*time.Second)).To(Succeed())
			}, utils.InferenceSetReadyTimeout, 5*time.Second).Should(Succeed())

			coldLoad.Stop()
			stats := coldLoad.Stats()
			GinkgoWriter.Printf("cold-start load: %+v\n", stats)
			Expect(stats.Total).To(BeNumerically(">", 0))

			response, err := utils.SendChatContext(ctx, gateway, modelName)
			Expect(err).NotTo(HaveOccurred())
			Expect(response.StatusCode).To(Equal(http.StatusOK))
			parsed, err := utils.ParseChatCompletionResponse(response)
			Expect(err).NotTo(HaveOccurred())
			Expect(parsed.Model).To(Equal(modelName))
		})

		It("returns to zero after reactivated work clears", func() {
			Eventually(func() (int32, error) {
				return utils.GetInferenceSetReplicas(ctx, modelName, namespace)
			}, keda.ScaleDownTotalWait+keda.CooldownPeriod+scaleToZeroProvisioningMargin, 5*time.Second).Should(Equal(int32(0)))
			Expect(assertEPPReady(ctx, namespace, modelName)).To(Succeed())
		})
	})

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
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning && podReady(&pods.Items[i]) {
			return nil
		}
	}
	return fmt.Errorf("no Running/Ready EPP pod matches %q; matching pods=%v", selector, pods.Items)
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
