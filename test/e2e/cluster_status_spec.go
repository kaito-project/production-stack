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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Cluster-layer status reporter tests (issue #87, proposal §1.2 cluster
// reasons). Each spec perturbs a shared cluster-wide control-plane Deployment
// and asserts the productionstack-status-reporter emits the matching Warning
// Event in kube-system within one reporter resync, and that the Warning ceases
// once the problem is fixed (the reporter publishes no positive recovery
// event).
//
// These perturb cluster-wide singletons every other namespace shares, so the
// suite is decorated Serial.
var _ = Describe("Cluster Status Reporter",
	Serial, utils.GinkgoLabelStatusReporter, utils.GinkgoLabelNoGPU, func() {

		const (
			// The recovery check requires no fresh Warning over this window.
			quietWindow  = 3 * time.Minute
			clearTimeout = 5 * time.Minute

			bbrDeployment = "body-based-router"

			gatewayAuthDeployment = "apikey-authz"
			gatewayAuthOperator   = "apikey-operator"

			// The reporter watches the KAITO workspace chart's local CSI
			// driver.
			localCSIManager = "csi-local-manager"

			kaitoDeployment = "kaito-workspace"
		)

		// The control-plane namespaces are discovered rather than assumed: a
		// managed control plane installs these workloads into its own add-on
		// namespace (kube-system on AI Manager).
		var (
			ctx                  context.Context
			bbrNamespace         string
			gatewayAuthNamespace string
			localCSINamespace    string
			kaitoNamespace       string
			// emitTimeout bounds the Warning for a Deployment scaled to zero.
			// It follows the running reporter's resync and startup grace: with
			// the chart's 60s grace, 3 minutes was only just enough.
			emitTimeout time.Duration
		)

		BeforeEach(func() {
			ctx = context.Background()
			timings, err := utils.GetReporterTimings(ctx)
			Expect(err).NotTo(HaveOccurred())
			emitTimeout = timings.DebouncedEmitTimeout()
			for _, target := range []struct {
				namespace  *string
				deployment string
			}{
				{&bbrNamespace, bbrDeployment},
				{&gatewayAuthNamespace, gatewayAuthDeployment},
				{&localCSINamespace, localCSIManager},
				{&kaitoNamespace, kaitoDeployment},
			} {
				ns, err := utils.DeploymentNamespace(ctx, target.deployment)
				Expect(err).NotTo(HaveOccurred())
				*target.namespace = ns
			}
		})

		// scaleToZeroAndWait scales a Deployment to zero, registers a safety-net
		// restore (so a failed assertion cannot leave the shared control plane
		// degraded), and returns the reporter Event for reason. It keeps the
		// Deployment at zero while waiting: an add-on manager may reconcile it
		// back to its declared replicas mid-wait, which would reset the
		// reporter's debounce. The restore returns the Deployment to its
		// original replica count and waits for that many ready replicas.
		scaleToZeroAndWait := func(ns, name string, original int32, reason, objectName string, since metav1.Time) corev1.Event {
			DeferCleanup(func() {
				_ = utils.ScaleDeployment(ctx, ns, name, original)
				_ = utils.WaitForDeploymentReplicas(ctx, ns, name, original, clearTimeout)
			})
			Expect(utils.ScaleDeployment(ctx, ns, name, 0)).To(Succeed())
			ev, err := utils.WaitForReporterEventHoldingScaledToZero(ctx, ns, name, reason, objectName, since, emitTimeout)
			Expect(err).NotTo(HaveOccurred())
			return ev
		}

		It("emits clusterBBRNotReady when BBR is scaled to zero and clears on recovery",
			utils.GinkgoLabelSmoke, func() {
				desired, _, err := utils.GetDeploymentReplicas(ctx, bbrNamespace, bbrDeployment)
				Expect(err).NotTo(HaveOccurred())
				baseline, err := utils.ReporterEventBaseline(ctx, "clusterBBRNotReady", "")
				Expect(err).NotTo(HaveOccurred())

				By("scaling BBR to zero and waiting for the clusterBBRNotReady Warning")
				ev := scaleToZeroAndWait(bbrNamespace, bbrDeployment, desired, "clusterBBRNotReady", "", baseline)
				Expect(ev.Message).NotTo(ContainSubstring("http://"))
				Expect(ev.Message).NotTo(ContainSubstring("https://"))

				By("restoring BBR and waiting for it to become ready")
				Expect(utils.ScaleDeployment(ctx, bbrNamespace, bbrDeployment, desired)).To(Succeed())
				Expect(utils.WaitForDeploymentReplicas(ctx, bbrNamespace, bbrDeployment, desired, clearTimeout)).To(Succeed())

				// Recovery is signalled by the Warning ceasing, not by a
				// positive event: the reporter only publishes problems. Baseline
				// the last clusterBBRNotReady once BBR is ready, then require no
				// fresh emission over a quiet window.
				By("waiting for clusterBBRNotReady to stop firing once BBR is restored")
				recoverBaseline, err := utils.ReporterEventBaseline(ctx, "clusterBBRNotReady", "")
				Expect(err).NotTo(HaveOccurred())
				Expect(utils.EnsureNoReporterEventSince(ctx, "clusterBBRNotReady", "", recoverBaseline, quietWindow)).
					To(Succeed())
			})

		It("emits clusterGatewayAuthNotReady when llm-gateway-auth is scaled to zero", func() {
			desired, _, err := utils.GetDeploymentReplicas(ctx, gatewayAuthNamespace, gatewayAuthDeployment)
			Expect(err).NotTo(HaveOccurred())

			baseline, err := utils.ReporterEventBaseline(ctx, "clusterGatewayAuthNotReady", "")
			Expect(err).NotTo(HaveOccurred())

			By("scaling llm-gateway-auth to zero and waiting for the clusterGatewayAuthNotReady Warning")
			ev := scaleToZeroAndWait(gatewayAuthNamespace, gatewayAuthDeployment, desired, "clusterGatewayAuthNotReady", "", baseline)
			Expect(ev.Message).NotTo(ContainSubstring("http://"))
			Expect(ev.Message).NotTo(ContainSubstring("https://"))
		})

		It("emits clusterKaitoControllerNotReady when the KAITO controller is scaled to zero", func() {
			desired, _, err := utils.GetDeploymentReplicas(ctx, kaitoNamespace, kaitoDeployment)
			Expect(err).NotTo(HaveOccurred())

			baseline, err := utils.ReporterEventBaseline(ctx, "clusterKaitoControllerNotReady", "")
			Expect(err).NotTo(HaveOccurred())

			By("scaling the KAITO controller to zero and waiting for the clusterKaitoControllerNotReady Warning")
			ev := scaleToZeroAndWait(kaitoNamespace, kaitoDeployment, desired, "clusterKaitoControllerNotReady", "", baseline)
			Expect(ev.Message).NotTo(ContainSubstring("http://"))
			Expect(ev.Message).NotTo(ContainSubstring("https://"))
		})

		It("emits clusterGatewayAuthNotReady when apikey-operator is scaled to zero", func() {
			desired, _, err := utils.GetDeploymentReplicas(ctx, gatewayAuthNamespace, gatewayAuthOperator)
			Expect(err).NotTo(HaveOccurred())
			baseline, err := utils.ReporterEventBaseline(ctx, "clusterGatewayAuthNotReady", "")
			Expect(err).NotTo(HaveOccurred())

			By("scaling apikey-operator to zero and waiting for the clusterGatewayAuthNotReady Warning")
			ev := scaleToZeroAndWait(gatewayAuthNamespace, gatewayAuthOperator, desired, "clusterGatewayAuthNotReady", "", baseline)
			Expect(ev.Message).To(ContainSubstring(gatewayAuthOperator))
			Expect(ev.Message).NotTo(ContainSubstring("http://"))
			Expect(ev.Message).NotTo(ContainSubstring("https://"))
		})

		It("emits clusterLocalCSIDriverNotReady when the local CSI driver manager is scaled to zero", func() {
			desired, _, err := utils.GetDeploymentReplicas(ctx, localCSINamespace, localCSIManager)
			Expect(err).NotTo(HaveOccurred())
			baseline, err := utils.ReporterEventBaseline(ctx, "clusterLocalCSIDriverNotReady", localCSINamespace)
			Expect(err).NotTo(HaveOccurred())

			By("scaling csi-local-manager to zero and waiting for the clusterLocalCSIDriverNotReady Warning")
			ev := scaleToZeroAndWait(localCSINamespace, localCSIManager, desired, "clusterLocalCSIDriverNotReady", localCSINamespace, baseline)
			Expect(ev.Message).To(ContainSubstring(localCSIManager))
			Expect(ev.Message).NotTo(ContainSubstring("http://"))
			Expect(ev.Message).NotTo(ContainSubstring("https://"))
		})
	})
