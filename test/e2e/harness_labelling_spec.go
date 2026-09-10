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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Harness labelling tests verify the §3 charts/modelharness labelling
// requirement of the end-to-end error-handling proposal:
//
//   - the workload Namespace carries the
//     `productionstack.kaito.sh/managed-by: modelharness` discovery label
//     the productionstack-status-reporter selects on; and
//   - every harness-owned object carries the stable
//     `kaito.sh/owned-by: modelharness` ownership label.
//
// These assertions run against the live objects the modelharness Helm
// chart renders, so a regression in the labels helper (or an object
// added without the common labels) is caught immediately.
var _ = Describe("ModelHarness Labelling", utils.GinkgoLabelInferenceSet, func() {
	var (
		ctx       context.Context
		namespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		utils.GetClusterClient(utils.TestingCluster)
		namespace = generateNamespace("e2e-harness-labels")

		By("Installing modelharness chart with auth + network policy enabled")
		// EnsureNamespace stamps the discovery label on the workload
		// namespace and installs the harness with auth enabled, so this
		// case exercises the ext-authz EnvoyFilter + APIKey objects too.
		Expect(utils.EnsureNamespace(ctx, namespace)).To(Succeed())
	})

	AfterEach(func() {
		By("Uninstalling modelharness chart")
		if err := utils.DeleteNamespace(ctx, namespace); err != nil {
			GinkgoWriter.Printf("Cleanup warning: %v\n", err)
		}
	})

	It("stamps the managed-by discovery label on the workload Namespace", func() {
		// A managed control plane provisions the workload namespace itself and
		// stamps only its own marker (kubernetes.azure.com/managedByAIManager),
		// leaving the discovery label the status reporter selects on absent.
		// Unskip once that is settled on the AI Manager side.
		Skip("AI Manager does not stamp the discovery label on namespaces it provisions")

		cl := utils.TestingCluster.KubeClient
		ns := &corev1.Namespace{}
		Expect(cl.Get(ctx, types.NamespacedName{Name: namespace}, ns)).To(Succeed())
		Expect(ns.Labels).To(HaveKeyWithValue(
			"productionstack.kaito.sh/managed-by", "modelharness"),
			"workload namespace must carry the reporter discovery label")
	})

	// The Namespace above is a built-in kind; everything asserted here is a CR
	// the harness renders, which a managed control plane does not let the caller
	// read.
	It("stamps kaito.sh/owned-by: modelharness on every harness-owned object",
		utils.GinkgoLabelStandardK8sOnly, func() {
			cl := utils.TestingCluster.KubeClient
			gatewayName := namespace

			// Each entry: a harness-owned object identified by GVK + name.
			owned := []struct {
				desc string
				gvk  schema.GroupVersionKind
				name string
			}{
				{
					desc: "Gateway",
					gvk:  schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"},
					name: gatewayName,
				},
				{
					desc: "catch-all EnvoyFilter",
					gvk:  schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1alpha3", Kind: "EnvoyFilter"},
					name: "model-not-found-direct",
				},
				{
					desc: "gateway error local-reply EnvoyFilter",
					gvk:  schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1alpha3", Kind: "EnvoyFilter"},
					name: "gateway-filter-outage-local-reply",
				},
				{
					desc: "CiliumNetworkPolicy",
					gvk:  schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"},
					name: "inference-pods-ingress",
				},
				{
					desc: "ext-authz EnvoyFilter",
					gvk:  schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1alpha3", Kind: "EnvoyFilter"},
					name: "apikey-ext-authz",
				},
				{
					desc: "APIKey",
					gvk:  schema.GroupVersionKind{Group: "kaito.sh", Version: "v1alpha1", Kind: "APIKey"},
					name: "default",
				},
			}

			for _, o := range owned {
				By("verifying " + o.desc + " carries the ownership label")
				obj := &unstructured.Unstructured{}
				obj.SetGroupVersionKind(o.gvk)
				Eventually(func() map[string]string {
					if err := cl.Get(ctx, types.NamespacedName{Namespace: namespace, Name: o.name}, obj); err != nil {
						return nil
					}
					return obj.GetLabels()
				}, utils.InferenceSetReadyTimeout, utils.PollInterval).Should(
					HaveKeyWithValue("kaito.sh/owned-by", "modelharness"),
					"%s must carry kaito.sh/owned-by: modelharness", o.desc)
			}
		})
})
