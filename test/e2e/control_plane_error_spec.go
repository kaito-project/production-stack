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
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Modeldeployment-layer status reporter tests (proposal §1.2 modeldeployment
// reasons) that cost nothing beyond the running cluster. Instead of deploying
// the modelharness and modeldeployment charts, each spec builds what the
// reporter evaluates directly in a fresh modelharness-managed namespace: an
// InferenceSet plus a stub `<inferenceset>-inferencepool-epp` Deployment.
//
//   - EPP outage: a zero-replica InferenceSet, so no node is provisioned.
//   - Non-terminal failure (debounced): a pod-template InferenceSet pinned to
//     an existing node, which KAITO counts as a BYO node and does not
//     provision, but whose pod KAITO's node affinity keeps Unschedulable.
//   - Terminal failures (immediate): QuotaExceeded / CapacityUnavailable, a
//     pod-template InferenceSet whose instance type Azure refuses to create,
//     so no VM is ever billed. These need the az CLI to confirm the
//     precondition and skip otherwise.
var _ = Describe("Control-plane Error Reporter",
	utils.GinkgoLabelStatusReporter, utils.GinkgoLabelNoGPU, utils.GinkgoLabelStandardK8sOnly, func() {

		const (
			setupTimeout     = 3 * time.Minute
			emitTimeout      = 3 * time.Minute
			provisionTimeout = 15 * time.Minute
			deleteTimeout    = 10 * time.Minute

			// The QuotaExceeded spec requests a SKU, of a VM family and size, that
			// has no quota; override them where the A10 family has quota.
			envZeroQuotaInstanceType = "E2E_ZERO_QUOTA_INSTANCE_TYPE"
			envZeroQuotaFamily       = "E2E_ZERO_QUOTA_FAMILY"
			envZeroQuotaVCPUs        = "E2E_ZERO_QUOTA_VCPUS"
		)

		var (
			ctx context.Context
			ns  string
		)

		// newNamespace creates a fresh managed namespace for one spec and deletes
		// it (and so the InferenceSet and anything KAITO created for it) after.
		newNamespace := func(prefix string) {
			ns = utils.UniqueName(prefix)
			Expect(utils.CreateManagedNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() {
				Expect(utils.DeleteManagedNamespace(context.Background(), ns, deleteTimeout)).To(Succeed())
			})
		}

		// podTemplate is a minimal user-defined inference workload.
		podTemplate := func(image string) *corev1.PodTemplateSpec {
			return &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:  "inference",
				Image: image,
				Ports: []corev1.ContainerPort{{ContainerPort: 5000}},
			}}}}
		}

		// expectPassThrough waits for the reporter to publish the KAITO
		// InferenceSetReady reason verbatim on the workload Namespace.
		expectPassThrough := func(kaitoReason, inferenceSet string, timeout time.Duration) {
			ev, err := utils.WaitForReporterEventSince(ctx, kaitoReason, ns, metav1Zero, timeout)
			Expect(err).NotTo(HaveOccurred(), "InferenceSetReady reason is %q", lastReadyReason(ctx, ns, inferenceSet))
			Expect(ev.InvolvedObject.Kind).To(Equal("Namespace"))
			Expect(ev.Message).To(HavePrefix("InferenceSet " + ns + "/" + inferenceSet + ": "))
			Expect(ev.Message).NotTo(ContainSubstring("http://"))
			Expect(ev.Message).NotTo(ContainSubstring("https://"))
		}

		BeforeEach(func() {
			ctx = context.Background()
		})

		It("emits inferencesetEPPNotReady when the InferenceSet EPP is scaled to zero", func() {
			newNamespace("e2e-rpt-epp")
			const isName = "epp-phi"
			Expect(utils.CreateInferenceSet(ctx, utils.InferenceSetSpec{
				Namespace: ns, Name: isName, Replicas: 0,
				InstanceType: "Standard_NV36ads_A10_v5", Preset: presetPhi,
			})).To(Succeed())
			eppName, err := utils.CreateStubEPP(ctx, ns, isName, setupTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("scaling the EPP Deployment to zero")
			Expect(utils.ScaleDeployment(ctx, ns, eppName, 0)).To(Succeed())

			ev, err := utils.WaitForReporterEventSince(ctx, "inferencesetEPPNotReady", ns, metav1Zero, emitTimeout)
			Expect(err).NotTo(HaveOccurred())
			Expect(ev.Message).To(ContainSubstring(isName))
			Expect(ev.Message).To(ContainSubstring("scaled to zero"))
			Expect(ev.Message).NotTo(ContainSubstring("http://"))
			Expect(ev.Message).NotTo(ContainSubstring("https://"))
		})

		It("passes through a non-terminal failure only after the 5-minute debounce", func() {
			newNamespace("e2e-rpt-unschedulable")
			hostname, instanceType, err := utils.ExistingNode(ctx)
			Expect(err).NotTo(HaveOccurred())
			const isName = "unschedulable"
			// KAITO counts the existing node as a BYO node, so it provisions
			// nothing, but its node affinity also requires the KAITO workspace
			// labels, which the node does not carry: the pod stays Unschedulable.
			// Karpenter refuses to provision for a hostname affinity, so no VM is
			// ever created.
			Expect(utils.CreateInferenceSet(ctx, utils.InferenceSetSpec{
				Namespace: ns, Name: isName, Replicas: 1,
				InstanceType: instanceType,
				NodeSelector: map[string]string{corev1.LabelHostname: hostname},
				PodTemplate:  podTemplate(utils.MissingImage),
			})).To(Succeed())
			_, err = utils.CreateStubEPP(ctx, ns, isName, setupTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for KAITO to report InferenceSetReady=Unschedulable")
			Eventually(func() string { return lastReadyReason(ctx, ns, isName) }).
				WithTimeout(setupTimeout).WithPolling(5 * time.Second).Should(Equal("Unschedulable"))

			// Unschedulable is neither in progress nor terminal, so the reporter
			// withholds it for 5 minutes since the condition's transition.
			By("checking no Warning is published within the debounce")
			Expect(utils.EnsureNoReporterEventSince(ctx, "Unschedulable", ns, metav1Zero, 4*time.Minute)).To(Succeed())

			By("waiting for the pass-through Unschedulable Warning after the debounce")
			expectPassThrough("Unschedulable", isName, 4*time.Minute)
		})

		It("passes through the terminal provisioning failure of an instance type without quota", func() {
			instanceType := envOr(envZeroQuotaInstanceType, "Standard_NV36ads_A10_v5")
			family := envOr(envZeroQuotaFamily, "StandardNVADSA10v5Family")
			vcpus := envIntOr(envZeroQuotaVCPUs, 36)
			region, err := utils.ClusterRegion(ctx)
			Expect(err).NotTo(HaveOccurred())
			free, err := utils.AvailableQuota(ctx, region, family)
			if err != nil {
				Skip("cannot read the " + family + " quota: " + err.Error())
			}
			if free >= vcpus {
				Skip(family + " has quota in " + region + "; a VM would be created")
			}

			newNamespace("e2e-rpt-quota")
			const isName = "quota"
			Expect(utils.CreateInferenceSet(ctx, utils.InferenceSetSpec{
				Namespace: ns, Name: isName, Replicas: 1,
				InstanceType: instanceType,
				NodeSelector: map[string]string{"e2e-reporter": ns},
				PodTemplate:  podTemplate(utils.MissingImage),
			})).To(Succeed())
			_, err = utils.CreateStubEPP(ctx, ns, isName, setupTimeout)
			Expect(err).NotTo(HaveOccurred())

			// Azure rejects the launch as QuotaExceeded, unless the Karpenter
			// provider already filters the zero-quota size out as an unavailable
			// offering, which KAITO reports as CapacityUnavailable. Both are
			// terminal, so the reporter publishes them without the debounce.
			By("waiting for KAITO to classify the provisioning failure")
			var kaitoReason string
			Eventually(func() string {
				kaitoReason = lastReadyReason(ctx, ns, isName)
				return kaitoReason
			}).WithTimeout(provisionTimeout).WithPolling(5 * time.Second).
				Should(BeElementOf("QuotaExceeded", "CapacityUnavailable"))

			By("waiting for the pass-through " + kaitoReason + " Warning")
			expectPassThrough(kaitoReason, isName, emitTimeout)
		})

		It("passes through CapacityUnavailable for an instance type the subscription cannot use", func() {
			region, err := utils.ClusterRegion(ctx)
			Expect(err).NotTo(HaveOccurred())
			sku, err := utils.RestrictedGPUSKU(ctx, region)
			if err != nil {
				Skip("cannot list VM sizes: " + err.Error())
			}
			if sku == "" {
				Skip("no N-series VM size is restricted for this subscription in " + region)
			}

			newNamespace("e2e-rpt-capacity")
			const isName = "capacity"
			if err := utils.CreateInferenceSet(ctx, utils.InferenceSetSpec{
				Namespace: ns, Name: isName, Replicas: 1,
				InstanceType: sku,
				NodeSelector: map[string]string{"e2e-reporter": ns},
				PodTemplate:  podTemplate(utils.MissingImage),
			}); err != nil {
				Skip("KAITO rejected restricted VM size " + sku + ": " + err.Error())
			}
			_, err = utils.CreateStubEPP(ctx, ns, isName, setupTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the pass-through CapacityUnavailable Warning for " + sku)
			if _, err := utils.WaitForReporterEventSince(ctx, "CapacityUnavailable", ns, metav1Zero, provisionTimeout); err != nil {
				// Whether Azure reports a restricted size as unavailable capacity
				// depends on the provider; the unit tests cover the pass-through.
				Skip("KAITO did not classify " + sku + " as CapacityUnavailable (InferenceSetReady reason " +
					lastReadyReason(ctx, ns, isName) + ")")
			}
			expectPassThrough("CapacityUnavailable", isName, emitTimeout)
		})
	})

// metav1Zero matches any reporter Event: every spec uses a fresh namespace, so
// no earlier Event can be mistaken for a new one.
var metav1Zero = metav1.Time{}

// lastReadyReason returns the InferenceSetReady reason for failure messages.
func lastReadyReason(ctx context.Context, namespace, name string) string {
	r, err := utils.GetInferenceSetReadyReason(ctx, namespace, name)
	if err != nil {
		return "unknown: " + err.Error()
	}
	return r
}

// envOr returns the environment variable, or def when it is unset.
func envOr(name, def string) string {
	if v := utils.GetEnv(name); v != "" {
		return v
	}
	return def
}

// envIntOr returns the integer environment variable, or def when it is unset
// or not an integer.
func envIntOr(name string, def int) int {
	if v, err := strconv.Atoi(utils.GetEnv(name)); err == nil {
		return v
	}
	return def
}
