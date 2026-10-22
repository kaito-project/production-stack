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
	"strings"
	"time"

	kaitoconsts "github.com/kaito-project/kaito/pkg/utils/consts"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator/modeldeployment"
	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Modeldeployment-layer status reporter tests (proposal §1.2 modeldeployment
// reasons) that cost nothing beyond the running cluster. Instead of deploying
// the modelharness and modeldeployment charts, each spec builds what the
// reporter evaluates directly in a fresh modelharness-managed namespace. Only
// the EPP spec creates a stub `<inferenceset>-inferencepool-epp` Deployment:
// the reporter does not report a missing EPP, which the other specs verify.
//
//   - EPP outage: a zero-replica InferenceSet, so no node is provisioned.
//   - Non-terminal failure (debounced): a pod-template InferenceSet pinned to
//     an existing node, which KAITO counts as a BYO node and does not
//     provision, but whose pod KAITO's node affinity keeps Unschedulable.
//   - Terminal failures (immediate): a pod-template InferenceSet whose
//     instance type has no quota (QuotaExceeded / CapacityUnavailable), and one
//     whose instance type the subscription cannot use, which Azure accepts and
//     then fails asynchronously (a Karpenter AsyncProvisioningError). No VM is
//     ever billed. These need a real node provisioner — they skip when
//     gpu-node-mocker fakes provisioning — and the az CLI to confirm the
//     precondition, and skip otherwise.
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

			// publishLatency bounds how long the reporter takes to publish a
			// terminal reason once KAITO reports it: one resync plus the E2E
			// startup grace, with slack.
			publishLatency = 2 * time.Minute
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

		// requireRealProvisioner skips a spec that needs a cloud provisioning
		// failure on a cluster where gpu-node-mocker fakes node provisioning.
		requireRealProvisioner := func() {
			mocked, err := utils.GPUNodeMockerDeployed(ctx)
			Expect(err).NotTo(HaveOccurred())
			if mocked {
				Skip("gpu-node-mocker fakes node provisioning, so no cloud provisioning failure can occur")
			}
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

			By("waiting for KAITO to report InferenceSetReady=Unschedulable")
			Eventually(func() string { return lastReadyReason(ctx, ns, isName) }).
				WithTimeout(setupTimeout).WithPolling(5 * time.Second).Should(Equal("Unschedulable"))

			// Unschedulable is neither in progress nor terminal, so the reporter
			// withholds it for 5 minutes since the condition's transition.
			By("checking no Warning is published within the debounce")
			Expect(utils.EnsureNoReporterEventSince(ctx, "Unschedulable", ns, metav1Zero, 4*time.Minute)).To(Succeed())

			By("waiting for the pass-through Unschedulable Warning after the debounce")
			expectPassThrough("Unschedulable", isName, 4*time.Minute)

			By("checking the missing EPP Deployment was never reported")
			_, found, err := utils.FindReporterEvent(ctx, "inferencesetEPPNotReady", ns)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeFalse(), "a missing EPP Deployment must not be reported")
		})

		It("passes through the terminal provisioning failure of an instance type without quota", func() {
			requireRealProvisioner()
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

		It("passes through an AsyncProvisioningError for an instance type the subscription cannot use", func() {
			requireRealProvisioner()
			region, err := utils.ClusterRegion(ctx)
			Expect(err).NotTo(HaveOccurred())
			sku, err := utils.RestrictedGPUSKU(ctx, region)
			if err != nil {
				Skip("cannot list VM sizes: " + err.Error())
			}
			if sku == "" {
				Skip("no N-series VM size is restricted for this subscription in " + region)
			}

			newNamespace("e2e-rpt-async")
			const isName = "async"
			if err := utils.CreateInferenceSet(ctx, utils.InferenceSetSpec{
				Namespace: ns, Name: isName, Replicas: 1,
				InstanceType: sku,
				NodeSelector: map[string]string{"e2e-reporter": ns},
				PodTemplate:  podTemplate(utils.MissingImage),
			}); err != nil {
				Skip("KAITO rejected restricted VM size " + sku + ": " + err.Error())
			}

			// Azure accepts the AKS machine and then fails its creation
			// asynchronously, so the Karpenter provider publishes an
			// AsyncProvisioningError, which KAITO classifies into a terminal reason
			// and keeps on InferenceSetReady with the original error in front of
			// the message.
			By("waiting for KAITO to report the AsyncProvisioningError of " + sku)
			var cond utils.ReadyCondition
			isAsync := func() bool {
				return kaitoconsts.IsTerminatedReason(cond.Reason) && strings.Contains(cond.Message, "AsyncProvisioningError")
			}
			// Polled by hand rather than with Eventually: running out of time is a
			// Skip (the async precondition was not met), not a failure.
			deadline := time.Now().Add(provisionTimeout)
			for time.Now().Before(deadline) {
				if cond, err = utils.GetInferenceSetReadyCondition(ctx, ns, isName); err == nil && isAsync() {
					break
				}
				time.Sleep(5 * time.Second)
			}
			if !isAsync() {
				Skip("KAITO did not report an AsyncProvisioningError for " + sku + " (InferenceSetReady " +
					cond.Reason + ": " + cond.Message + ")")
			}
			reportedAt := time.Now()

			By("waiting for the pass-through " + cond.Reason + " Warning")
			ev, err := utils.WaitForReporterEventSince(ctx, cond.Reason, ns, metav1Zero, emitTimeout)
			Expect(err).NotTo(HaveOccurred())
			Expect(ev.Message).To(HavePrefix("InferenceSet " + ns + "/" + isName + ": "))
			Expect(ev.Message).To(ContainSubstring("AsyncProvisioningError"))
			// A terminal reason bypasses the debounce of non-terminal failures,
			// which is anchored on the condition's lastTransitionTime (when the
			// InferenceSet first went not-ready). It is observable only when KAITO
			// reported the reason early enough that a debounced Warning would come
			// later than an immediate one.
			debounceEnd := cond.LastTransitionTime.Add(modeldeployment.FailureGracePeriod)
			if reportedAt.Add(publishLatency).Before(debounceEnd) {
				Expect(ev.FirstTimestamp.Time).To(BeTemporally("<", debounceEnd),
					"a terminal reason must be published without the debounce")
			} else {
				AddReportEntry("debounce bypass not observable",
					"KAITO reported "+cond.Reason+" at "+reportedAt.Format(time.RFC3339)+
						", too close to the end of the debounce at "+debounceEnd.Format(time.RFC3339))
			}
		})
	})

// metav1Zero matches any reporter Event: every spec uses a fresh namespace, so
// no earlier Event can be mistaken for a new one.
var metav1Zero = metav1.Time{}

// lastReadyReason returns the InferenceSetReady reason for failure messages.
func lastReadyReason(ctx context.Context, namespace, name string) string {
	cond, err := utils.GetInferenceSetReadyCondition(ctx, namespace, name)
	if err != nil {
		return "unknown: " + err.Error()
	}
	return cond.Reason
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
