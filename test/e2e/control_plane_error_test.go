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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Modeldeployment-layer (per-InferenceSet) status reporter tests (issue #87,
// proposal §1.2 inferenceset reasons). These perturb the request-path
// components owned by a single InferenceSet in a deployed case namespace and
// assert the reporter surfaces the matching inferenceset* Warning on the
// workload Namespace (the reporter's involvedObject is always cluster-scoped).
//
// The perturbations are namespace-scoped to one case (EPP Deployment, per-case
// Gateway), so this suite does not need to be Serial.
var _ = Describe("Control-plane Error Reporter",
	Ordered, utils.GinkgoLabelStatusReporter, func() {

		const (
			emitTimeout  = 3 * time.Minute
			clearTimeout = 5 * time.Minute
		)

		var (
			ctx       context.Context
			caseNS    string
			modelName string
		)

		BeforeAll(func() {
			ctx = context.Background()
			InstallCase(CaseControlPlaneError)
			caseNS = CaseNamespace(CaseControlPlaneError)
			modelName = CaseDeployments[CaseControlPlaneError][0].Name
		})

		AfterAll(func() {
			UninstallCase(CaseControlPlaneError)
		})

		It("emits inferencesetEPPNotReady when the InferenceSet EPP is scaled to zero", func() {
			cs, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			By("locating the EPP Deployment for the InferenceSet")
			// charts/modeldeployment renders exactly one Deployment (the EPP),
			// stamped only with the kaito.sh/inferenceset identifying label —
			// it carries no app.kubernetes.io/component label. Select the same
			// way the reporter's eppNotReady does so the test and the SUT agree.
			deps, err := cs.AppsV1().Deployments(caseNS).List(ctx, metav1.ListOptions{
				LabelSelector: "kaito.sh/inferenceset=" + modelName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(deps.Items).NotTo(BeEmpty(), "EPP Deployment should exist for %s", modelName)
			eppName := deps.Items[0].Name

			DeferCleanup(func() {
				_ = utils.ScaleDeployment(ctx, caseNS, eppName, 1)
				_ = utils.WaitForDeploymentReplicas(ctx, caseNS, eppName, 1, clearTimeout)
			})

			By("scaling the EPP Deployment to zero")
			Expect(utils.ScaleDeployment(ctx, caseNS, eppName, 0)).To(Succeed())

			ev, err := utils.WaitForReporterEvent(ctx, "inferencesetEPPNotReady", caseNS, emitTimeout)
			Expect(err).NotTo(HaveOccurred())
			Expect(ev.Message).To(ContainSubstring(modelName))
		})

		It("emits inferencesetRouteNotReady when the InferencePool backend is deleted", func() {
			dyn, err := utils.GetDynamicClient()
			Expect(err).NotTo(HaveOccurred())
			poolName := utils.InferencePoolName(modelName)

			By("deleting the InferencePool referenced by the HTTPRoute backendRef")
			// Removing the backend InferencePool makes the per-case HTTPRoute's
			// parent status report ResolvedRefs=False (BackendNotFound), which
			// is exactly what the reporter's routeNotReady check surfaces as
			// inferencesetRouteNotReady.
			//
			// No per-test restore is registered: this is the last spec in the
			// Ordered container and AfterAll's UninstallCase deletes the whole
			// case namespace (helm uninstall tolerates the already-deleted
			// InferencePool). Reinstalling the case in a DeferCleanup here would
			// race that namespace teardown and fail with "namespace is being
			// terminated".
			Expect(dyn.Resource(utils.InferencePoolGVR).Namespace(caseNS).
				Delete(ctx, poolName, metav1.DeleteOptions{})).To(Succeed())

			_, err = utils.WaitForReporterEvent(ctx, "inferencesetRouteNotReady", caseNS, emitTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})
