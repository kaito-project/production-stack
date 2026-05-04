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
	"math/rand"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// ModelDeployment chart tests verify that the modeldeployment Helm chart
// (charts/modeldeployment) correctly installs an InferenceSet and its
// associated GAIE artifacts (InferencePool, EPP, HTTPRoute), and that
// `helm uninstall` removes them. The chart's EPP runs with
// --secure-serving=false, so no DestinationRule is needed for the Istio
// Gateway to reach it.

// generateNamespace returns a unique namespace name with a random suffix.
func generateNamespace(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, rand.Intn(900000)+100000)
}

// createNamespace creates a Kubernetes namespace.
func createNamespace(ctx context.Context, name string) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	err := utils.TestingCluster.KubeClient.Create(ctx, ns)
	Expect(err).NotTo(HaveOccurred(), "failed to create namespace %s", name)
}

// deleteNamespace deletes a Kubernetes namespace.
func deleteNamespace(ctx context.Context, name string) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	err := utils.TestingCluster.KubeClient.Delete(ctx, ns)
	if err != nil {
		GinkgoWriter.Printf("Cleanup warning: failed to delete namespace %s: %v\n", name, err)
	}
}

var _ = Describe("ModelDeployment Chart", utils.GinkgoLabelInferenceSet, func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
		utils.GetClusterClient(utils.TestingCluster)
	})

	Context("modeldeployment chart install + uninstall", func() {
		// Pull the deployment values for this case from the per-case table
		// (see cases.go). The same deployment is shared by both It blocks
		// below — BeforeEach installs the chart in a fresh random namespace,
		// AfterEach uninstalls it and deletes the namespace.
		caseValues := CaseDeployments[CaseModelDeploymentChart][0]
		deploymentName := caseValues.Name
		preset := caseValues.Model

		var namespace string
		var gatewayName string

		BeforeEach(func() {
			namespace = generateNamespace("e2e-inferenceset")
			// Mirror the chart convention defined in
			// charts/modeldeployment/templates/_helpers.tpl
			// (and charts/modelharness): when gatewayName is empty,
			// the chart derives it as "<namespace>-gw".
			gatewayName = namespace + "-gw"
			createNamespace(ctx, namespace)

			values := caseValues
			values.Namespace = namespace
			By("Installing modeldeployment chart")
			err := utils.CreateInferenceSetWithRouting(ctx, utils.TestingCluster.KubeClient, values)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			By("Uninstalling modeldeployment chart")
			if err := utils.CleanupInferenceSetWithRouting(ctx, utils.TestingCluster.KubeClient, deploymentName, namespace); err != nil {
				GinkgoWriter.Printf("Cleanup warning: %v\n", err)
			}
			deleteNamespace(ctx, namespace)
		})

		It("should render InferenceSet + HTTPRoute with the expected spec", func() {
			cl := utils.TestingCluster.KubeClient

			By("Verifying InferenceSet exists with correct spec")
			is := &unstructured.Unstructured{}
			is.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "kaito.sh",
				Version: "v1alpha1",
				Kind:    "InferenceSet",
			})
			err := cl.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, is)
			Expect(err).NotTo(HaveOccurred())

			replicas, found, _ := unstructured.NestedInt64(is.Object, "spec", "replicas")
			Expect(found).To(BeTrue())
			Expect(replicas).To(Equal(int64(1)))

			presetName, found, _ := unstructured.NestedString(is.Object, "spec", "template", "inference", "preset", "name")
			Expect(found).To(BeTrue())
			Expect(presetName).To(Equal(preset),
				"InferenceSet preset.name should equal the chart `model` value (preset), not the deploymentName")

			instanceType, found, _ := unstructured.NestedString(is.Object, "spec", "template", "resource", "instanceType")
			Expect(found).To(BeTrue())
			Expect(instanceType).To(Equal("Standard_NV36ads_A10_v5"))

			By("Verifying HTTPRoute exists with correct spec")
			hr := &unstructured.Unstructured{}
			hr.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "gateway.networking.k8s.io",
				Version: "v1",
				Kind:    "HTTPRoute",
			})
			err = cl.Get(ctx, types.NamespacedName{Name: deploymentName + "-route", Namespace: namespace}, hr)
			Expect(err).NotTo(HaveOccurred())

			// Verify the HTTPRoute references the correct gateway.
			parentRefs, found, _ := unstructured.NestedSlice(hr.Object, "spec", "parentRefs")
			Expect(found).To(BeTrue())
			Expect(parentRefs).To(HaveLen(1))
			parentRef, ok := parentRefs[0].(map[string]interface{})
			Expect(ok).To(BeTrue())
			Expect(parentRef["name"]).To(Equal(gatewayName))

			// Verify the HTTPRoute matches the deploymentName in the
			// X-Gateway-Model-Name header (this is what user requests carry
			// in their `model` field). Multiple deployments of the same
			// preset can coexist in a namespace under distinct deploymentNames.
			rules, found, _ := unstructured.NestedSlice(hr.Object, "spec", "rules")
			Expect(found).To(BeTrue())
			Expect(rules).To(HaveLen(1))
			rule, ok := rules[0].(map[string]interface{})
			Expect(ok).To(BeTrue())
			matches, ok := rule["matches"].([]interface{})
			Expect(ok).To(BeTrue())
			Expect(matches).To(HaveLen(1))
			match := matches[0].(map[string]interface{})
			headers, ok := match["headers"].([]interface{})
			Expect(ok).To(BeTrue())
			Expect(headers).To(HaveLen(1))
			header := headers[0].(map[string]interface{})
			Expect(header["name"]).To(Equal("X-Gateway-Model-Name"))
			Expect(header["value"]).To(Equal(deploymentName),
				"HTTPRoute header match value should equal the deploymentName, not the preset")

			backendRefs, ok := rule["backendRefs"].([]interface{})
			Expect(ok).To(BeTrue())
			Expect(backendRefs).To(HaveLen(1))
			backendRef, ok := backendRefs[0].(map[string]interface{})
			Expect(ok).To(BeTrue())
			Expect(backendRef["name"]).To(Equal(utils.InferencePoolName(deploymentName)))
			Expect(backendRef["kind"]).To(Equal("InferencePool"))
		})

		It("should remove all resources after `helm uninstall`", func() {
			cl := utils.TestingCluster.KubeClient

			By("Uninstalling modeldeployment chart up-front (AfterEach will be a no-op)")
			Expect(utils.CleanupInferenceSetWithRouting(ctx, cl, deploymentName, namespace)).To(Succeed())

			By("Verifying InferenceSet is deleted")
			Eventually(func() bool {
				is := &unstructured.Unstructured{}
				is.SetGroupVersionKind(schema.GroupVersionKind{
					Group:   "kaito.sh",
					Version: "v1alpha1",
					Kind:    "InferenceSet",
				})
				err := cl.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, is)
				return apierrors.IsNotFound(err)
			}, 3*time.Minute, utils.PollInterval).Should(BeTrue(), "InferenceSet should be fully deleted")

			By("Verifying HTTPRoute is deleted")
			Eventually(func() bool {
				hr := &unstructured.Unstructured{}
				hr.SetGroupVersionKind(schema.GroupVersionKind{
					Group:   "gateway.networking.k8s.io",
					Version: "v1",
					Kind:    "HTTPRoute",
				})
				err := cl.Get(ctx, types.NamespacedName{Name: deploymentName + "-route", Namespace: namespace}, hr)
				return apierrors.IsNotFound(err)
			}, 30*time.Second, utils.PollInterval).Should(BeTrue(), "HTTPRoute should be deleted")
		})
	})
})
