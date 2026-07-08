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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Modelharness-layer status reporter tests (issue #87, proposal §1.2
// modelharness reasons + §3 namespace discovery). These create a misconfigured
// AuthorizationPolicy in a workload namespace and assert the reporter surfaces
// the matching Warning Event, and that the §3 namespace-discovery label gates
// whether any modelharness* Event is emitted at all.
var _ = Describe("Harness Status Reporter",
	utils.GinkgoLabelStatusReporter, func() {

		const (
			emitTimeout    = 3 * time.Minute
			noEmitDwell    = 90 * time.Second
			managedByLabel = "productionstack.kaito.sh/managed-by"
			bogusProvider  = "nonexistent-ext-authz-provider"
		)

		// authorizationPolicyGVR matches the resource the reporter probes for
		// the ext_authz provider check (security.istio.io/v1).
		authorizationPolicyGVR := schema.GroupVersionResource{
			Group:    "security.istio.io",
			Version:  "v1",
			Resource: "authorizationpolicies",
		}

		var (
			ctx context.Context
			ns  string
		)

		BeforeEach(func() {
			ctx = context.Background()
			ns = generateNamespace("reporter-harness")
		})

		// applyBogusAuthorizationPolicy creates an AuthorizationPolicy whose
		// CUSTOM provider references a name that is not registered in the mesh
		// extensionProviders registry — a local misconfiguration the reporter
		// must flag as modelharnessExtAuthzProviderMissing.
		applyBogusAuthorizationPolicy := func(namespace string) {
			dyn, err := utils.GetDynamicClient()
			Expect(err).NotTo(HaveOccurred())
			ap := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "security.istio.io/v1",
				"kind":       "AuthorizationPolicy",
				"metadata": map[string]interface{}{
					"name":      "reporter-bogus-ext-authz",
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"action": "CUSTOM",
					"provider": map[string]interface{}{
						"name": bogusProvider,
					},
					"rules": []interface{}{
						map[string]interface{}{},
					},
				},
			}}
			_, err = dyn.Resource(authorizationPolicyGVR).Namespace(namespace).
				Create(ctx, ap, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		It("emits modelharnessExtAuthzProviderMissing naming the affected namespace", func() {
			Expect(utils.EnsureNamespace(ctx, ns, true)).To(Succeed())
			DeferCleanup(func() { _ = utils.DeleteNamespace(ctx, ns) })

			applyBogusAuthorizationPolicy(ns)

			ev, err := utils.WaitForReporterEvent(ctx, "modelharnessExtAuthzProviderMissing", ns, emitTimeout)
			Expect(err).NotTo(HaveOccurred())
			Expect(ev.Message).To(ContainSubstring(ns),
				"event message must name the affected workload namespace")
			Expect(ev.Message).To(ContainSubstring(bogusProvider))
		})

		It("does not emit modelharness events until the discovery label is present (§3)", func() {
			clientset, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			By("creating an unlabelled namespace with a misconfigured AuthorizationPolicy")
			_, err = clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_ = clientset.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
			})
			applyBogusAuthorizationPolicy(ns)

			By("asserting no modelharness event is emitted while the label is absent")
			Expect(utils.EnsureNoReporterEvent(ctx, "modelharnessExtAuthzProviderMissing", ns, noEmitDwell)).
				To(Succeed())

			By("stamping the namespace-discovery label")
			patch := fmt.Sprintf(`{"metadata":{"labels":{%q:"modelharness"}}}`, managedByLabel)
			_, err = clientset.CoreV1().Namespaces().Patch(
				ctx, ns, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("asserting the Warning now appears once the namespace is managed")
			_, err = utils.WaitForReporterEvent(ctx, "modelharnessExtAuthzProviderMissing", ns, emitTimeout)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			// Best-effort guard in case a namespace lingers without DeferCleanup.
			clientset, err := utils.GetK8sClientset()
			if err == nil {
				if _, gerr := clientset.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); gerr == nil {
					_ = clientset.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
				} else if !apierrors.IsNotFound(gerr) {
					_ = gerr
				}
			}
		})
	})
