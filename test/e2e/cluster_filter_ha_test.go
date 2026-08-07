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

// BBR cluster-filter HA is one of the four Smoke groups backed by the
// reusable, ordinary-Go test/e2e/scenarios library (see
// scenarios/cluster_filter_ha.go for the full rationale, issue #89). This
// file is a thin Ginkgo adapter: Setup / Cleanup and every assertion's
// actual logic (including the replica-scaling choreography) live in the
// library. This suite perturbs the shared cluster-wide BBR Deployment, so
// it MUST stay decorated Serial — no other spec may run while BBR is
// degraded.
var _ = Describe("BBR cluster-filter HA",
	Ordered, Serial, utils.GinkgoLabelOutage, utils.GinkgoLabelSmoke, func() {
		// CaseClusterFilterHA deployment values — non-auth (mirrors the
		// gpu-mocker case): this group exercises the cluster-wide BBR
		// Deployment's HA, not the model pool's or ext_authz's.
		clusterFilterHADeployment := CaseDeployments[CaseClusterFilterHA][0]

		lifecycle := utils.NewHelmLifecycle()
		cfg := scenarios.ClusterFilterHAConfig{
			Namespace: CaseNamespace(CaseClusterFilterHA),
			Deployment: scenarios.DeploymentSpec{
				Name:         clusterFilterHADeployment.Name,
				Model:        clusterFilterHADeployment.Model,
				Replicas:     clusterFilterHADeployment.Replicas,
				InstanceType: clusterFilterHADeployment.InstanceType,
			},
			Lifecycle: lifecycle,
			Chat:      utils.ChatClientHelm{},
			Kube:      utils.KubeOpsHelm{},
			Logger:    utils.GinkgoLogger{},
		}

		var (
			ctx         context.Context
			state       scenarios.ClusterFilterHAState
			groupFailed bool
		)

		BeforeAll(func() {
			ctx = context.Background()
			// Assume failure until Setup proves otherwise, so a
			// BeforeAll failure (e.g. mid-way through provisioning)
			// is treated as a failed group: AfterAll must then
			// preserve whatever was partially created for diagnostics
			// instead of cleaning it up or operating on zero-value
			// names.
			groupFailed = true
			var err error
			state, err = scenarios.ClusterFilterHASetup(ctx, cfg)
			Expect(err).NotTo(HaveOccurred(), "cluster-filter-HA group setup should succeed")
			groupFailed = false
		})

		AfterEach(func() {
			if CurrentSpecReport().Failed() {
				groupFailed = true
			}
		})

		AfterAll(func() {
			// Best-effort: restore BBR to its original (captured during
			// Setup) replica count unconditionally, so a failed spec
			// cannot leave the shared cluster-wide dataplane degraded
			// for the rest of the suite. This is independent of (and
			// runs before) the group's own namespace/deployment cleanup
			// below.
			scenarios.TeardownBBR(ctx, state)

			err := scenarios.Cleanup(ctx, cfg.Lifecycle, cfg.Logger, state.Resources(),
				!groupFailed, nil, utils.DiagnosticsHelm{})
			Expect(err).NotTo(HaveOccurred(), "cluster-filter-HA group cleanup should succeed")
		})

		It("renders the BBR Deployment with >= 2 replicas and pod anti-affinity", func() {
			Expect(scenarios.AssertBBRHighAvailabilityRendered(ctx, state)).To(Succeed())
		})

		It("configures an active gRPC readiness probe on the BBR health port", func() {
			Expect(scenarios.AssertBBRReadinessProbeConfigured(ctx, state)).To(Succeed())
		})

		It("keeps serving prompts while running at a single replica", func() {
			Expect(scenarios.AssertServesAtSingleReplica(ctx, state)).To(Succeed())
		})

		It("recovers to the full replica count after running degraded", func() {
			Expect(scenarios.AssertRecoversToFullReplicaCount(ctx, state)).To(Succeed())
		})

		It("fails closed (no silent 404) when all BBR replicas are down", func() {
			Expect(scenarios.AssertFailsClosedWhenAllReplicasDown(ctx, state)).To(Succeed())
		})
	})
