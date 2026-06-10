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
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// BBR cluster-filter high-availability tests (issue #89).
//
// What this verifies (and why):
//
//	BBR ext_proc is a CLUSTER-SCOPE singleton on the hot path of every
//	inference request. Running a single replica makes it a single point
//	of failure, and a fail-closed cluster (failure_mode_allow: false)
//	would turn any single-replica blip into a cluster-wide
//	`502 bbr_unavailable` outage. Issue #89 hardens this by:
//
//	  1. rendering the BBR Deployment with >= 2 replicas + pod
//	     anti-affinity (charts/.../body-based-routing/values.yaml,
//	     templates/bbr.yaml), and
//	  2. wiring active gRPC health checking (k8s readiness probe on the
//	     health port -> Istio EDS endpoint set) + passive outlier
//	     detection (templates/istio.yaml) with max_ejection_percent < 100
//
//	so that the loss of ONE replica is a transparent failover and the
//	fail-closed `502` path only fires when ALL replicas are down.
//
//	This suite perturbs the shared kaito-system BBR Deployment, so it is
//	decorated Serial — no other spec may run while BBR is degraded.
//
// Scope note: the structured `502 bbr_unavailable` envelope (+
// `x-kaito-error-source: bbr` header) is produced by the cluster-wide
// `local_reply` EnvoyFilter, which belongs to the separate BBR
// fail-closed work that issue #89 depends on (Test Plan: bbr_outage_test.go).
// It is intentionally NOT asserted here; the all-replicas-down case below
// asserts only that the request fails closed (5xx, never a silent 404).
var _ = Describe("BBR cluster-filter HA",
	Ordered, Serial, utils.GinkgoLabelClusterFilterHA, utils.GinkgoLabelSmoke, func() {

		const (
			bbrNamespace  = "kaito-system"
			bbrDeployment = "body-based-router"
			bbrHealthPort = 9005
		)

		var (
			ctx       context.Context
			caseURL   string
			modelName string
		)

		// sendChat drives a plain (no-auth) chat completion against this
		// case's Gateway — CaseClusterFilterHA does not enable ext_authz.
		sendChat := func() (*http.Response, error) {
			return utils.SendChatCompletionWithRetry(caseURL, modelName)
		}

		BeforeAll(func() {
			ctx = context.Background()
			caseURL = InstallCase(CaseClusterFilterHA)
			modelName = CaseDeployments[CaseClusterFilterHA][0].Name

			// BBR must be HA before we start removing replicas.
			By("waiting for BBR to reach >= 2 ready replicas")
			Eventually(func() (int32, error) {
				_, ready, err := utils.GetDeploymentReplicas(ctx, bbrNamespace, bbrDeployment)
				return ready, err
			}, 3*time.Minute, utils.PollInterval).Should(BeNumerically(">=", int32(2)),
				"BBR Deployment must run >= 2 ready replicas for HA (issue #89)")

			// Confirm the baseline request path is healthy through BBR.
			By("confirming the baseline request path returns 200")
			Eventually(func() error {
				resp, err := sendChat()
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					body, _ := utils.ReadResponseBody(resp)
					return fmt.Errorf("expected 200, got %d: %s", resp.StatusCode, string(body))
				}
				return nil
			}, 5*time.Minute, 10*time.Second).Should(Succeed(),
				"baseline request path through BBR should return 200")
		})

		AfterAll(func() {
			// Best-effort: restore BBR to its HA replica count so a failed
			// spec cannot leave the shared dataplane degraded for the rest
			// of the suite.
			_ = utils.ScaleDeployment(ctx, bbrNamespace, bbrDeployment, 2)
			_ = utils.WaitForDeploymentReplicas(ctx, bbrNamespace, bbrDeployment, 2, 3*time.Minute)
			UninstallCase(CaseClusterFilterHA)
		})

		It("renders the BBR Deployment with >= 2 replicas and pod anti-affinity", func() {
			cs, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			d, err := cs.AppsV1().Deployments(bbrNamespace).Get(ctx, bbrDeployment, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "BBR Deployment should exist in %s", bbrNamespace)

			Expect(d.Spec.Replicas).NotTo(BeNil())
			Expect(*d.Spec.Replicas).To(BeNumerically(">=", int32(2)),
				"BBR spec.replicas must be >= 2 (schema minimum)")

			aff := d.Spec.Template.Spec.Affinity
			Expect(aff).NotTo(BeNil(), "BBR pod spec must set affinity")
			Expect(aff.PodAntiAffinity).NotTo(BeNil(),
				"BBR pod spec must set podAntiAffinity to spread replicas across nodes")
			hasTerm := len(aff.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) > 0 ||
				len(aff.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0
			Expect(hasTerm).To(BeTrue(),
				"podAntiAffinity must declare at least one (preferred or required) spread term")
		})

		It("configures an active gRPC readiness probe on the BBR health port", func() {
			cs, err := utils.GetK8sClientset()
			Expect(err).NotTo(HaveOccurred())

			d, err := cs.AppsV1().Deployments(bbrNamespace).Get(ctx, bbrDeployment, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			var probed bool
			for _, c := range d.Spec.Template.Spec.Containers {
				if c.Name != "bbr" {
					continue
				}
				Expect(c.ReadinessProbe).NotTo(BeNil(), "bbr container must define a readinessProbe")
				Expect(c.ReadinessProbe.GRPC).NotTo(BeNil(),
					"bbr readinessProbe must be a gRPC probe (active grpc.health.v1.Health check)")
				Expect(c.ReadinessProbe.GRPC.Port).To(Equal(int32(bbrHealthPort)),
					"bbr readinessProbe must target the health port %d, not the ext_proc port", bbrHealthPort)
				probed = true
			}
			Expect(probed).To(BeTrue(), "bbr container not found on the BBR Deployment")
		})

		It("keeps serving prompts while running at a single replica", func() {
			// Scale (don't delete a pod) to reach the degraded state: a
			// deleted pod is reconciled back by the Deployment controller
			// almost immediately, so a delete-based burst would race the
			// replacement becoming Ready and might never actually run
			// against a single replica. Scaling to 1 deterministically
			// pins the degraded state for the duration of the burst.
			By("scaling BBR down to a single replica to hold the degraded state")
			Expect(utils.ScaleDeployment(ctx, bbrNamespace, bbrDeployment, 1)).To(Succeed())

			// Wait until the cluster is genuinely degraded: exactly one
			// replica Ready (the second has drained from the Istio EDS
			// endpoint set), so the burst below is not racing convergence.
			By("waiting until exactly one BBR replica is ready")
			Eventually(func() (int32, error) {
				_, ready, err := utils.GetDeploymentReplicas(ctx, bbrNamespace, bbrDeployment)
				return ready, err
			}, 2*time.Minute, utils.PollInterval).Should(Equal(int32(1)),
				"BBR should hold at exactly one ready replica")

			// With one healthy replica we must NEVER observe:
			//   * 404 -> BBR failed OPEN and the request fell through the
			//     catch-all (model_not_found) without header injection, or
			//   * 502 -> the fail-closed bbr_unavailable path, which must
			//     fire only when ALL replicas are down.
			// Because the degraded state is now steady (no EDS churn), the
			// surviving replica is expected to serve EVERY request.
			By("verifying the surviving replica serves every request (no 404, no 502)")
			const burst = 15
			var ok, notFound, badGateway, other int
			for i := 0; i < burst; i++ {
				resp, err := sendChat()
				if err != nil {
					other++
					time.Sleep(time.Second)
					continue
				}
				switch resp.StatusCode {
				case http.StatusOK:
					ok++
				case http.StatusNotFound:
					notFound++
				case http.StatusBadGateway:
					badGateway++
				default:
					other++
				}
				resp.Body.Close()
				time.Sleep(time.Second)
			}
			GinkgoWriter.Printf("single-replica burst: ok=%d notFound=%d badGateway=%d other=%d\n",
				ok, notFound, badGateway, other)

			Expect(notFound).To(Equal(0),
				"a single healthy replica must not cause 404 fall-through (BBR must stay fail-closed, not fail-open)")
			Expect(badGateway).To(Equal(0),
				"a single healthy replica must not trip the 502 bbr_unavailable path")
			// Allow a single transient miss (port-forward blip, model timeout)
			// that lands in `other`: the load-bearing HA invariants above are
			// no 404 fall-through and no premature 502, not a strict 100% pass
			// rate. Demanding ok == burst makes this spec flaky on unrelated
			// transport hiccups.
			Expect(ok).To(BeNumerically(">=", burst-1),
				"a single healthy BBR replica should serve virtually every request")
		})

		It("recovers to the full replica count after running degraded", func() {
			By("scaling BBR back to its HA replica count")
			Expect(utils.ScaleDeployment(ctx, bbrNamespace, bbrDeployment, 2)).To(Succeed())
			Eventually(func() (int32, error) {
				_, ready, err := utils.GetDeploymentReplicas(ctx, bbrNamespace, bbrDeployment)
				return ready, err
			}, 3*time.Minute, utils.PollInterval).Should(BeNumerically(">=", int32(2)),
				"BBR should return to >= 2 ready replicas after scaling back up")
		})

		It("fails closed (no silent 404) when all BBR replicas are down", func() {
			By("scaling BBR to zero replicas")
			Expect(utils.ScaleDeployment(ctx, bbrNamespace, bbrDeployment, 0)).To(Succeed())
			Eventually(func() (int32, error) {
				_, ready, err := utils.GetDeploymentReplicas(ctx, bbrNamespace, bbrDeployment)
				return ready, err
			}, 2*time.Minute, utils.PollInterval).Should(Equal(int32(0)),
				"BBR should report zero ready replicas after scaling to zero")

			// With every replica gone and failure_mode_allow: false, the
			// request must fail CLOSED (5xx) — it must NOT silently fall
			// through to a 404 model_not_found, which is exactly the
			// ambiguity issue #89 / the proposal close. (The structured
			// `502 bbr_unavailable` envelope is asserted separately by the
			// fail-closed work; here we only assert fail-closed semantics.)
			By("verifying requests fail closed (5xx, never a silent 404)")
			Eventually(func() (int, error) {
				resp, err := sendChat()
				if err != nil {
					return 0, err
				}
				defer resp.Body.Close()
				return resp.StatusCode, nil
			}, 2*time.Minute, 5*time.Second).Should(SatisfyAll(
				BeNumerically(">=", 500),
				Not(Equal(http.StatusNotFound)),
			), "an all-replicas-down BBR must fail closed, not fall through to 404 model_not_found")

			By("restoring BBR to >= 2 replicas")
			Expect(utils.ScaleDeployment(ctx, bbrNamespace, bbrDeployment, 2)).To(Succeed())
			Expect(utils.WaitForDeploymentReplicas(ctx, bbrNamespace, bbrDeployment, 2, 3*time.Minute)).
				To(Succeed(), "BBR should return to its HA replica count")

			By("confirming the request path recovers to 200")
			Eventually(func() (int, error) {
				resp, err := sendChat()
				if err != nil {
					return 0, err
				}
				defer resp.Body.Close()
				return resp.StatusCode, nil
			}, 3*time.Minute, 5*time.Second).Should(Equal(http.StatusOK),
				"request path should return 200 once BBR is healthy again")
		})
	})
