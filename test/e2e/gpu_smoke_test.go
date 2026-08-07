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

	. "github.com/onsi/ginkgo/v2" //nolint:revive // Ginkgo DSL
	. "github.com/onsi/gomega"    //nolint:revive // Gomega DSL

	"github.com/kaito-project/production-stack/test/e2e/scenarios"
	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// GPU / framework smoke is one of the four Smoke groups backed by the
// reusable, ordinary-Go test/e2e/scenarios library (see
// scenarios/gpu_smoke.go). It covers ONLY the lightweight
// framework-initialisation sanity check and gateway-reachability check —
// the InferenceSet/EPP/HTTPRoute observability, fake-node, shadow-pod,
// and garbage-collection assertions that also live under the GPU-mocker
// umbrella remain Infra/Routing-labeled in gpu_mocker_test.go /
// CaseGPUMocker and are untouched by this library.
//
// This group owns its own small, dedicated (non-auth), single-replica
// deployment (CaseGPUSmoke) so `E2E_LABEL=Smoke` runs don't pay for the
// larger GPU-mocker case's fake-node/shadow-pod setup.
var _ = Describe("GPU Framework Smoke", Ordered, utils.GinkgoLabelSmoke, func() {
	gpuSmokeDeployment := CaseDeployments[CaseGPUSmoke][0]

	lifecycle := utils.NewHelmLifecycle()
	cfg := scenarios.GPUSmokeConfig{
		Namespace: CaseNamespace(CaseGPUSmoke),
		Deployment: scenarios.DeploymentSpec{
			Name:         gpuSmokeDeployment.Name,
			Model:        gpuSmokeDeployment.Model,
			Replicas:     gpuSmokeDeployment.Replicas,
			InstanceType: gpuSmokeDeployment.InstanceType,
		},
		Lifecycle: lifecycle,
		Chat:      utils.ChatClientHelm{},
		Logger:    utils.GinkgoLogger{},
	}

	var (
		ctx         context.Context
		state       scenarios.GPUSmokeState
		groupFailed bool
	)

	BeforeAll(func() {
		ctx = context.Background()
		// Assume failure until Setup proves otherwise, so a BeforeAll
		// failure (e.g. mid-way through provisioning) is treated as a
		// failed group: AfterAll must then preserve whatever was
		// partially created for diagnostics instead of cleaning it up
		// or operating on zero-value names.
		groupFailed = true
		var err error
		state, err = scenarios.GPUSmokeSetup(ctx, cfg)
		Expect(err).NotTo(HaveOccurred(), "GPU-smoke group setup should succeed")
		groupFailed = false
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			groupFailed = true
		}
	})

	AfterAll(func() {
		err := scenarios.Cleanup(ctx, cfg.Lifecycle, cfg.Logger, state.Resources(),
			!groupFailed, nil, utils.DiagnosticsHelm{})
		Expect(err).NotTo(HaveOccurred(), "GPU-smoke group cleanup should succeed")
	})

	Context("Framework validation", func() {
		It("should have the test framework properly initialised", func() {
			Expect(scenarios.AssertFrameworkInitialised()).To(Succeed())
		})
	})

	Context("Gateway connectivity", func() {
		It("should be reachable and return a response", func() {
			Expect(scenarios.AssertGatewayReachable(ctx, state)).To(Succeed())
		})
	})
})
