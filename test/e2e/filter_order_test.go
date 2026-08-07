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

// Filter execution order is one of the four Smoke groups backed by the
// reusable, ordinary-Go test/e2e/scenarios library (see
// scenarios/filter_order.go for the full P0/P1/P2 matrix and rationale).
// This file is a thin Ginkgo adapter: Setup / Cleanup and every
// assertion's actual logic (including the Envoy /config_dump parsing and
// all Kubernetes/HTTP interactions) live in the library, injected here
// via the Helm-backed Lifecycle/ChatClient/KubeOps adapters. Individual
// `It`s are preserved 1:1 with the library's named assertions (A1, A2,
// A2-sanity, B2, B1, F1, D2, E1, A3, D3, E2) so CI reporting stays
// per-check.
var _ = Describe("Filter execution order",
	Ordered, utils.GinkgoLabelFilterOrder, utils.GinkgoLabelSmoke, func() {
		// CaseFilterOrder deployment values — Setup forces
		// AuthAPIKeyEnabled=true regardless of what's declared here,
		// since the full ext_authz -> bbr -> ext_proc(EPP) -> router
		// chain must be exercised (mirrors cases.go's pre-existing
		// AuthAPIKeyEnabled: true for CaseFilterOrder).
		filterOrderDeployment := CaseDeployments[CaseFilterOrder][0]

		lifecycle := utils.NewHelmLifecycle()
		cfg := scenarios.FilterOrderConfig{
			Namespace: CaseNamespace(CaseFilterOrder),
			Deployment: scenarios.DeploymentSpec{
				Name:         filterOrderDeployment.Name,
				Model:        filterOrderDeployment.Model,
				Replicas:     filterOrderDeployment.Replicas,
				InstanceType: filterOrderDeployment.InstanceType,
			},
			Lifecycle: lifecycle,
			Chat:      utils.ChatClientHelm{},
			Kube:      utils.KubeOpsHelm{},
			Logger:    utils.GinkgoLogger{},
		}

		var (
			ctx         context.Context
			state       scenarios.FilterOrderState
			groupFailed bool
		)

		BeforeAll(func() {
			ctx = context.Background()
			// Assume failure until Setup proves otherwise, so a
			// BeforeAll failure (e.g. mid-way through provisioning) is
			// treated as a failed group: AfterAll must then preserve
			// whatever was partially created for diagnostics instead of
			// cleaning it up or operating on zero-value names.
			groupFailed = true
			var err error
			state, err = scenarios.FilterOrderSetup(ctx, cfg)
			Expect(err).NotTo(HaveOccurred(), "filter-order group setup should succeed")
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
			Expect(err).NotTo(HaveOccurred(), "filter-order group cleanup should succeed")
		})

		// ─────────────────────────────────────────────────────────────
		// P0 — ext_authz precedes router / catch-all
		// ─────────────────────────────────────────────────────────────

		Context("P0: ext_authz precedes router / catch-all", func() {
			It("A1: unauth'd + unknown model returns 401, not 404", func() {
				Expect(scenarios.AssertUnauthUnknownModelReturns401(ctx, state)).To(Succeed())
			})

			It("A2: unauth'd request never reaches BBR", func() {
				Expect(scenarios.AssertUnauthRequestNeverReachesBBR(ctx, state)).To(Succeed())
			})

			It("A2 sanity: authenticated request DOES exercise BBR", func() {
				Expect(scenarios.AssertAuthedRequestExercisesBBR(ctx, state)).To(Succeed())
			})
		})

		// ─────────────────────────────────────────────────────────────
		// P0 — bbr precedes EPP, full chain delivers traffic
		// ─────────────────────────────────────────────────────────────

		Context("P0: bbr precedes EPP, full chain delivers traffic", func() {
			It("B2: N valid requests increase vLLM request_success_total by >=N", func() {
				Expect(scenarios.AssertValidRequestsIncreaseSuccessTotal(ctx, state, 5)).To(Succeed())
			})

			It("B1: at least one shadow pod's per-pod counter increased", func() {
				Expect(scenarios.AssertAtLeastOneShadowPodServed(ctx, state)).To(Succeed())
			})
		})

		// ─────────────────────────────────────────────────────────────
		// P0 — Static proof of filter chain order via Envoy admin
		// /config_dump on the Gateway pod.
		// ─────────────────────────────────────────────────────────────

		Context("P0: Envoy filter chain order (config_dump)", func() {
			It("F1: HCM filter order is ext_authz -> bbr -> ext_proc -> router", func() {
				Expect(scenarios.AssertEnvoyFilterOrder(ctx, state)).To(Succeed())
			})
		})

		// ─────────────────────────────────────────────────────────────
		// P1 — bbr <-> router interaction, invalid-key + unknown-model
		// ─────────────────────────────────────────────────────────────

		Context("P1: catch-all preserves auth, BBR controls header injection", func() {
			It("D2: invalid API key + unknown model returns 401, not 404", func() {
				Expect(scenarios.AssertInvalidKeyUnknownModelReturns401(ctx, state)).To(Succeed())
			})

			It("E1: authed request with missing model field returns 400 invalid_request_body", func() {
				Expect(scenarios.AssertMissingModelFieldReturns400(ctx, state)).To(Succeed())
			})
		})

		// ─────────────────────────────────────────────────────────────
		// P2 — Coverage of secondary properties
		// ─────────────────────────────────────────────────────────────

		Context("P2: EPP isolation, catch-all transits BBR, content-type handling", func() {
			It("A3: unauth'd request never reaches the EPP pod", func() {
				Expect(scenarios.AssertUnauthRequestNeverReachesEPP(ctx, state)).To(Succeed())
			})

			It("D3: authed + unknown model still transits BBR (catch-all path)", func() {
				Expect(scenarios.AssertAuthedUnknownModelTransitsBBR(ctx, state)).To(Succeed())
			})

			It("E2: non-JSON Content-Type does not cause 5xx", func() {
				Expect(scenarios.AssertNonJSONContentTypeNo5xx(ctx, state)).To(Succeed())
			})
		})
	})
