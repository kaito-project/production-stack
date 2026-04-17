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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// InferenceSet lifecycle tests verify that the utility functions correctly
// create an InferenceSet and its associated routing resources (DestinationRule,
// HTTPRoute). These resources were previously deployed as static manifests by
// the E2E workflow; they are now created per-test via the utils helpers.

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

var _ = Describe("InferenceSet Lifecycle", utils.GinkgoLabelInferenceSet, func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
		utils.GetClusterClient(utils.TestingCluster)
	})

	Context("Create InferenceSet with routing resources", func() {
		const (
			modelName   = "falcon-7b-instruct"
			gatewayName = "inference-gateway"
		)

		var namespace string

		BeforeEach(func() {
			namespace = generateNamespace("e2e-inferenceset-create")
			createNamespace(ctx, namespace)
		})

		AfterEach(func() {
			By("Cleaning up InferenceSet and routing resources")
			err := utils.CleanupInferenceSetWithRouting(ctx, utils.TestingCluster.KubeClient, modelName, namespace)
			if err != nil {
				GinkgoWriter.Printf("Cleanup warning: %v\n", err)
			}
			deleteNamespace(ctx, namespace)
		})

		It("should create InferenceSet and auto-create DestinationRule and HTTPRoute after readiness", func() {
			cfg := utils.DefaultInferenceSetConfig(modelName)
			cfg.Namespace = namespace

			By("Creating InferenceSet with routing via utility function")
			err := utils.CreateInferenceSetWithRouting(ctx, utils.TestingCluster.KubeClient, cfg)
			Expect(err).NotTo(HaveOccurred())

			cl := utils.TestingCluster.KubeClient

			By("Verifying InferenceSet exists with correct spec")
			is := &unstructured.Unstructured{}
			is.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "kaito.sh",
				Version: "v1alpha1",
				Kind:    "InferenceSet",
			})
			err = cl.Get(ctx, types.NamespacedName{Name: modelName, Namespace: namespace}, is)
			Expect(err).NotTo(HaveOccurred())

			replicas, found, _ := unstructured.NestedInt64(is.Object, "spec", "replicas")
			Expect(found).To(BeTrue())
			Expect(replicas).To(Equal(int64(2)))

			presetName, found, _ := unstructured.NestedString(is.Object, "spec", "template", "inference", "preset", "name")
			Expect(found).To(BeTrue())
			Expect(presetName).To(Equal(modelName))

			instanceType, found, _ := unstructured.NestedString(is.Object, "spec", "template", "resource", "instanceType")
			Expect(found).To(BeTrue())
			Expect(instanceType).To(Equal("Standard_NV36ads_A10_v5"))

			By("Verifying DestinationRule exists with correct spec")
			dr := &unstructured.Unstructured{}
			dr.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "networking.istio.io",
				Version: "v1",
				Kind:    "DestinationRule",
			})
			eppName := utils.EPPServiceName(modelName)
			err = cl.Get(ctx, types.NamespacedName{Name: eppName, Namespace: namespace}, dr)
			Expect(err).NotTo(HaveOccurred())

			host, found, _ := unstructured.NestedString(dr.Object, "spec", "host")
			Expect(found).To(BeTrue())
			Expect(host).To(Equal(eppName))

			tlsMode, found, _ := unstructured.NestedString(dr.Object, "spec", "trafficPolicy", "tls", "mode")
			Expect(found).To(BeTrue())
			Expect(tlsMode).To(Equal("SIMPLE"))

			By("Verifying HTTPRoute exists with correct spec")
			hr := &unstructured.Unstructured{}
			hr.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "gateway.networking.k8s.io",
				Version: "v1",
				Kind:    "HTTPRoute",
			})
			err = cl.Get(ctx, types.NamespacedName{Name: modelName + "-route", Namespace: namespace}, hr)
			Expect(err).NotTo(HaveOccurred())

			// Verify the HTTPRoute references the correct gateway.
			parentRefs, found, _ := unstructured.NestedSlice(hr.Object, "spec", "parentRefs")
			Expect(found).To(BeTrue())
			Expect(parentRefs).To(HaveLen(1))
			parentRef, ok := parentRefs[0].(map[string]interface{})
			Expect(ok).To(BeTrue())
			Expect(parentRef["name"]).To(Equal(gatewayName))

			// Verify the HTTPRoute routes to the correct InferencePool.
			rules, found, _ := unstructured.NestedSlice(hr.Object, "spec", "rules")
			Expect(found).To(BeTrue())
			Expect(rules).To(HaveLen(1))
			rule, ok := rules[0].(map[string]interface{})
			Expect(ok).To(BeTrue())
			backendRefs, ok := rule["backendRefs"].([]interface{})
			Expect(ok).To(BeTrue())
			Expect(backendRefs).To(HaveLen(1))
			backendRef, ok := backendRefs[0].(map[string]interface{})
			Expect(ok).To(BeTrue())
			Expect(backendRef["name"]).To(Equal(utils.InferencePoolName(modelName)))
			Expect(backendRef["kind"]).To(Equal("InferencePool"))
		})
	})

	Context("Cleanup InferenceSet resources", func() {
		const modelName = "ministral-3-3b-instruct"

		var namespace string

		BeforeEach(func() {
			namespace = generateNamespace("e2e-inferenceset-cleanup")
			createNamespace(ctx, namespace)
		})

		AfterEach(func() {
			deleteNamespace(ctx, namespace)
		})

		It("should create and then successfully clean up all resources", func() {
			cfg := utils.DefaultInferenceSetConfig(modelName)
			cfg.Namespace = namespace
			cl := utils.TestingCluster.KubeClient

			By("Creating InferenceSet with routing")
			err := utils.CreateInferenceSetWithRouting(ctx, cl, cfg)
			Expect(err).NotTo(HaveOccurred())

			By("Cleaning up all resources")
			err = utils.CleanupInferenceSetWithRouting(ctx, cl, modelName, namespace)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying InferenceSet is deleted")
			is := &unstructured.Unstructured{}
			is.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "kaito.sh",
				Version: "v1alpha1",
				Kind:    "InferenceSet",
			})
			err = cl.Get(ctx, types.NamespacedName{Name: modelName, Namespace: namespace}, is)
			Expect(err).To(HaveOccurred())

			By("Verifying DestinationRule is deleted")
			dr := &unstructured.Unstructured{}
			dr.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "networking.istio.io",
				Version: "v1",
				Kind:    "DestinationRule",
			})
			err = cl.Get(ctx, types.NamespacedName{Name: utils.EPPServiceName(modelName), Namespace: namespace}, dr)
			Expect(err).To(HaveOccurred())

			By("Verifying HTTPRoute is deleted")
			hr := &unstructured.Unstructured{}
			hr.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "gateway.networking.k8s.io",
				Version: "v1",
				Kind:    "HTTPRoute",
			})
			err = cl.Get(ctx, types.NamespacedName{Name: modelName + "-route", Namespace: namespace}, hr)
			Expect(err).To(HaveOccurred())
		})
	})
})
