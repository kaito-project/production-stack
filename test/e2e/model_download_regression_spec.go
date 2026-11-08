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
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Real-GPU regression for the status reporter (proposal §1.2): deploying a
// healthy model through the modelharness and modeldeployment charts — GPU node
// provisioning, weight download and startup included — must not publish any
// reporter Warning for the workload namespace: no pass-through KAITO failure
// and no inferencesetModelDownloadSlow. This spec provisions a real GPU node;
// set E2E_GPU_INSTANCE_TYPE to a SKU with quota in the cluster's region.
//
// On the GPU node mocker the case runs without KAITO's post-load benchmark:
// the benchmark reads the inference pod's logs from its fake node, so it never
// finishes, the InferenceSet never becomes Ready, and KAITO's
// inferencesetNotReady is surfaced once it outlasts the reporter's grace
// period.
//
// The spec is Serial: any Warning in its quiet window fails it, including true
// ones caused by other specs, e.g. its EPP pod waiting for a new node, or being
// preempted by a higher-priority pod, while parallel cases fill the cluster.
var _ = Describe("Status Reporter GPU Regression",
	Ordered, Serial, utils.GinkgoLabelStatusReporter, func() {

		const quietDwell = 2 * time.Minute

		var (
			ctx     context.Context
			caseNS  string
			values  deploy.ModelDeploymentValues
			started metav1.Time
		)

		BeforeAll(func() {
			ctx = context.Background()
			mocked, err := utils.GPUNodeMockerDeployed(ctx)
			Expect(err).NotTo(HaveOccurred())
			deployments := slices.Clone(CaseDeployments[CaseModelDownloadRegression])
			if mocked {
				for i := range deployments {
					deployments[i].DisableBenchmark = true
				}
			}
			values = deployments[0]
			started = metav1.Now()
			InstallCaseDeployments(CaseModelDownloadRegression, deployments)
			caseNS = CaseNamespace(CaseModelDownloadRegression)
		})

		AfterAll(func() {
			UninstallCase(CaseModelDownloadRegression)
		})

		It("publishes no reporter Warning for a healthy deployment", func() {
			// Serving through the gateway does not mean KAITO counts the
			// replicas as Ready; wait for that too, so a deployment that never
			// becomes Ready fails here instead of racing the reporter's grace
			// period.
			By(fmt.Sprintf("Waiting for InferenceSet %s/%s to report %d ready replica(s)", caseNS, values.Name, values.Replicas))
			Eventually(func() (int32, error) {
				return utils.GetInferenceSetReadyReplicas(ctx, values.Name, caseNS)
			}, utils.InferenceSetReadyTimeout, 10*time.Second).Should(BeEquivalentTo(values.Replicas),
				"InferenceSet %s/%s must report every replica Ready", caseNS, values.Name)

			// Keep watching for a quiet dwell in case a finding was still
			// inside its debounce.
			time.Sleep(quietDwell)
			events, err := utils.ReporterEventsForObjectSince(ctx, caseNS, started)
			Expect(err).NotTo(HaveOccurred())
			for _, ev := range events {
				AddReportEntry("unexpected reporter event", ev.Reason+": "+ev.Message)
			}
			Expect(events).To(BeEmpty(), "a healthy deployment must not publish reporter Warnings")
		})
	})
