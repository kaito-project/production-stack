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

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/downloader"
	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Model-download sliding-window tests (proposal §1.2 / §1.4
// inferencesetModelDownloadSlow) that cost nothing beyond the running cluster.
// Each spec uses a zero-replica preset InferenceSet, so KAITO provisions
// nothing, and drives the download the reporter observes directly:
//
//   - ModelMirror path: a mock ModelMirror whose status the spec patches. It
//     requires the reporter's modelStreamingEnabled=true and KAITO's
//     ModelMirror feature gate off, so no controller reconciles the mock.
//   - vLLM self-download path: the InferenceSet opts out of streaming and a
//     stub model pod, never Ready, serves the download-speed gauge.
//
// The E2E install runs the reporter with a 60s resync, a 60s window at
// 20 MB/s, a 30s startup grace and a 120s Event refresh interval.
var _ = Describe("Model Download Slow Reporter",
	utils.GinkgoLabelStatusReporter, utils.GinkgoLabelNoGPU, utils.GinkgoLabelStandardK8sOnly, func() {

		const (
			setupTimeout  = 3 * time.Minute
			emitTimeout   = 5 * time.Minute
			deleteTimeout = 5 * time.Minute
			// refreshWait outlasts the E2E Event refresh interval plus a resync.
			refreshWait = 4 * time.Minute
			// quietWindow outlasts the E2E refresh interval plus a resync, so a
			// still-active finding would have been rewritten within it.
			quietWindow = 3*time.Minute + 30*time.Second
			resync      = 70 * time.Second

			slowBps int64 = 6_000_000
			fastBps int64 = 80_000_000
		)

		var (
			ctx    context.Context
			ns     string
			mirror string
		)

		const isName = "download-phi"

		// setup creates a managed namespace with a zero-replica preset
		// InferenceSet and its stub EPP. A streaming opt-out selects the vLLM
		// self-download path; otherwise the mock ModelMirror is created.
		setup := func(prefix string, selfDownload bool) {
			ns = utils.UniqueName(prefix)
			Expect(utils.CreateManagedNamespace(ctx, ns)).To(Succeed())
			DeferCleanup(func() {
				Expect(utils.DeleteManagedNamespace(context.Background(), ns, deleteTimeout)).To(Succeed())
			})
			spec := utils.InferenceSetSpec{
				Namespace: ns, Name: isName, Replicas: 0,
				InstanceType: "Standard_NV36ads_A10_v5", Preset: presetPhi,
			}
			if selfDownload {
				spec.Annotations = map[string]string{"kaito.sh/model-streaming": "disabled"}
			}
			Expect(utils.CreateInferenceSet(ctx, spec)).To(Succeed())
			_, err := utils.CreateStubEPP(ctx, ns, isName, setupTimeout)
			Expect(err).NotTo(HaveOccurred())
			if !selfDownload {
				mirror = downloader.ModelMirrorName(presetPhi, "")
				Expect(utils.CreateModelMirror(ctx, ns, mirror)).To(Succeed())
			}
		}

		setSpeed := func(bps int64) {
			Expect(utils.SetModelMirrorDownloading(ctx, ns, mirror, bps)).To(Succeed())
		}

		expectNoSlowEvent := func(quiet time.Duration) {
			Expect(utils.EnsureNoReporterEventSince(ctx, "inferencesetModelDownloadSlow", ns, metav1Zero, quiet)).
				To(Succeed())
		}

		BeforeEach(func() {
			ctx = context.Background()
		})

		It("emits inferencesetModelDownloadSlow for a sustained-slow ModelMirror and stops once it is Ready", func() {
			setup("e2e-rpt-mm-slow", false)
			setSpeed(slowBps)

			By("waiting for the sustained-slow Warning")
			ev, err := utils.WaitForReporterEventSince(ctx, "inferencesetModelDownloadSlow", ns, metav1Zero, emitTimeout)
			Expect(err).NotTo(HaveOccurred())
			Expect(ev.Message).To(ContainSubstring("InferenceSet " + ns + "/" + isName))
			Expect(ev.Message).To(ContainSubstring("source=modelmirror " + ns + "/" + mirror))
			Expect(ev.Message).To(ContainSubstring("worst 6.00 MB/s"))
			Expect(ev.Message).NotTo(ContainSubstring("http"))

			By("checking the Warning keeps being refreshed while the download stays slow")
			refreshed, err := utils.WaitForReporterEventSince(ctx, "inferencesetModelDownloadSlow", ns, ev.LastTimestamp, refreshWait)
			Expect(err).NotTo(HaveOccurred())
			Expect(refreshed.Count).To(BeNumerically(">", ev.Count))

			By("marking the ModelMirror Ready and checking the Warning is no longer refreshed")
			Expect(utils.SetModelMirrorReady(ctx, ns, mirror)).To(Succeed())
			time.Sleep(resync)
			baseline, err := utils.ReporterEventBaseline(ctx, "inferencesetModelDownloadSlow", ns)
			Expect(err).NotTo(HaveOccurred())
			Expect(utils.EnsureNoReporterEventSince(ctx, "inferencesetModelDownloadSlow", ns, baseline, quietWindow)).
				To(Succeed())
		})

		It("does not emit inferencesetModelDownloadSlow for a transient ModelMirror dip", func() {
			setup("e2e-rpt-mm-dip", false)
			setSpeed(fastBps)
			time.Sleep(resync)

			By("dipping below the threshold for less than one resync")
			setSpeed(slowBps)
			time.Sleep(40 * time.Second)
			setSpeed(fastBps)

			expectNoSlowEvent(quietWindow)
		})

		It("does not emit inferencesetModelDownloadSlow before the window is full", func() {
			setup("e2e-rpt-mm-partial", false)

			By("downloading slowly for less than one window, then completing")
			setSpeed(slowBps)
			time.Sleep(50 * time.Second)
			Expect(utils.SetModelMirrorReady(ctx, ns, mirror)).To(Succeed())

			expectNoSlowEvent(quietWindow)
		})

		It("emits inferencesetModelDownloadSlow for a sustained-slow vLLM self-download", func() {
			setup("e2e-rpt-vllm-slow", true)
			pod, err := utils.CreateDownloadingModelPod(ctx, ns, isName, slowBps, setupTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the sustained-slow Warning from the model pod")
			ev, err := utils.WaitForReporterEventSince(ctx, "inferencesetModelDownloadSlow", ns, metav1Zero, emitTimeout)
			Expect(err).NotTo(HaveOccurred())
			Expect(ev.Message).To(ContainSubstring("InferenceSet " + ns + "/" + isName))
			Expect(ev.Message).To(ContainSubstring("source=model-pod " + pod))
			Expect(ev.Message).To(ContainSubstring("worst 6.00 MB/s"))
		})
	})
