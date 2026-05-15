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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Filter execution order tests.
//
// What this verifies (and why):
//
//	The per-namespace Istio Gateway provisioned by charts/modelharness
//	must materialise the following Envoy HTTP filter chain on every
//	inference request:
//
//	    envoy.filters.http.ext_authz            (llm-gateway-apikey)
//	  → envoy.filters.http.ext_proc.bbr          (body-based-routing)
//	  → envoy.filters.http.ext_proc              (InferencePool/EPP)
//	  → envoy.filters.http.router                (HTTPRoute → vLLM pod
//	                                              OR catch-all 404)
//
//	If this order ever drifts the regressions are silent: e.g. the
//	`model-not-found` catch-all silently bypasses ext_authz
//	(charts/modelharness/templates/envoyfilter-not-found.yaml), or BBR
//	runs after the InferencePool ext_proc and EPP routing falls through
//	to round-robin (charts/productionstack/charts/body-based-routing/
//	values.yaml). Both have shipped at least once in main, so we treat
//	the order as a property worth its own regression suite.
//
//	The case-level deployment (CaseFilterOrder in cases.go) is
//	auth-enabled so a single namespace exercises every filter in the
//	chain.
var _ = Describe("Filter execution order",
	Ordered, utils.GinkgoLabelFilterOrder, utils.GinkgoLabelSmoke, func() {

		var (
			ctx          context.Context
			caseURL      string
			caseNS       string
			modelName    string
			apiKey       string
			hostHeader   string
			gatewayLabel string
		)

		BeforeAll(func() {
			ctx = context.Background()
			caseURL = InstallCase(CaseFilterOrder)
			caseNS = CaseNamespace(CaseFilterOrder)
			dep := CaseDeployments[CaseFilterOrder][0]
			modelName = dep.Name
			hostHeader = caseNS + ".gw.example.com"
			gatewayLabel = "gateway.networking.k8s.io/gateway-name=" + CaseGatewayName(CaseFilterOrder)

			// Bearer token used by every "happy path" request below. Issued
			// by the apikey-operator from the APIKey CR rendered by the
			// modelharness chart (see charts/modelharness/templates/apikey.yaml).
			Eventually(func() (string, error) {
				return utils.GetAPIKeyFromSecret(ctx, caseNS)
			}, 60*time.Second, 2*time.Second).ShouldNot(BeEmpty(),
				"API key Secret should be created in %s", caseNS)
			var err error
			apiKey, err = utils.GetAPIKeyFromSecret(ctx, caseNS)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			UninstallCase(CaseFilterOrder)
		})

		// sendAuth sends a /v1/chat/completions request with the given
		// bearer token. Pass "" to omit the Authorization header entirely
		// (SendChatCompletionWithAuth treats "" as "do not set header").
		sendAuth := func(model, token string) (*http.Response, error) {
			return utils.SendChatCompletionWithAuth(caseURL, model, "hello", token, hostHeader)
		}

		// sendRaw posts an arbitrary body to /v1/chat/completions, optionally
		// attaching a bearer token. Used by tests that need a malformed body
		// (missing model field, non-JSON, …) or want to drive the request
		// without going through SendChatCompletion's JSON marshaller.
		sendRaw := func(body []byte, contentType, token string) (*http.Response, error) {
			if err := utils.EnsurePortForwards(); err != nil {
				return nil, err
			}
			url := utils.ResolveGatewayURL(caseURL) + "/v1/chat/completions"
			req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", contentType)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			req.Host = hostHeader
			client := &http.Client{Timeout: utils.HTTPTimeout}
			return client.Do(req)
		}

		// ─────────────────────────────────────────────────────────────────
		// P0 — ext_authz must run before the router (and the catch-all)
		// ─────────────────────────────────────────────────────────────────

		Context("P0: ext_authz precedes router / catch-all", func() {

			// A1 — Without an Authorization header the response MUST be 401
			// even when the model is unknown. If the router executed before
			// ext_authz, the unknown-model request would hit the catch-all
			// `model-not-found-direct` EnvoyFilter and return 404; that is
			// exactly the silent-bypass regression called out in
			// charts/modelharness/templates/envoyfilter-not-found.yaml.
			It("A1: unauth'd + unknown model returns 401, not 404", func() {
				Eventually(func() int {
					resp, err := sendAuth("does-not-exist-model", "")
					if err != nil {
						return 0
					}
					defer resp.Body.Close()
					return resp.StatusCode
				}, 2*time.Minute, 5*time.Second).Should(Equal(http.StatusUnauthorized),
					"unauth'd request with an unknown model must be rejected by ext_authz (401), not by the catch-all (404)")
			})

			// A2 — An unauth'd request must NOT reach BBR. We assert this by
			// snapshotting the BBR pod's log size before and after sending
			// the unauth'd request, and grep'ing the new log lines for the
			// unique correlation prompt. BBR's default `body-field-to-header`
			// plugin logs the extracted model name (at -v=3 which the chart
			// pins), so any execution would surface in the logs.
			It("A2: unauth'd request never reaches BBR", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				bbrNS := "istio-system"
				bbrPod, err := firstRunningPod(ctx, bbrNS,
					"app.kubernetes.io/name=body-based-routing")
				Expect(err).NotTo(HaveOccurred(),
					"BBR pod should be running in %s", bbrNS)

				before, err := utils.GetPodLogs(clientset, bbrNS, bbrPod, "bbr")
				Expect(err).NotTo(HaveOccurred())
				beforeLen := len(before)

				// Use a unique model value so we can grep for it after.
				needle := fmt.Sprintf("a2-no-bbr-%d", time.Now().UnixNano())
				resp, err := sendAuth(needle, "")
				Expect(err).NotTo(HaveOccurred())
				defer resp.Body.Close()
				Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))

				// Give BBR's log writer time to flush — if it would have run.
				time.Sleep(3 * time.Second)

				after, err := utils.GetPodLogs(clientset, bbrNS, bbrPod, "bbr")
				Expect(err).NotTo(HaveOccurred())
				delta := after
				if len(after) >= beforeLen {
					delta = after[beforeLen:]
				}
				Expect(delta).NotTo(ContainSubstring(needle),
					"BBR should not have seen the unauth'd request body; found needle %q in new log slice", needle)
			})

			// B2 — Sanity counter-test for A2: a fully authenticated request
			// for the same unique model needle must *increase* BBR log
			// output. Without this, A2 could pass even if BBR was simply
			// silent.
			It("A2 sanity: authenticated request DOES exercise BBR", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				bbrNS := "istio-system"
				bbrPod, err := firstRunningPod(ctx, bbrNS,
					"app.kubernetes.io/name=body-based-routing")
				Expect(err).NotTo(HaveOccurred())

				before, err := utils.GetPodLogs(clientset, bbrNS, bbrPod, "bbr")
				Expect(err).NotTo(HaveOccurred())
				beforeLen := len(before)

				// Send a valid request and wait for it to complete.
				resp, err := sendAuth(modelName, apiKey)
				Expect(err).NotTo(HaveOccurred())
				defer resp.Body.Close()
				Expect(resp.StatusCode).To(Equal(http.StatusOK))

				time.Sleep(3 * time.Second)

				after, err := utils.GetPodLogs(clientset, bbrNS, bbrPod, "bbr")
				Expect(err).NotTo(HaveOccurred())
				Expect(len(after)).To(BeNumerically(">", beforeLen),
					"BBR log size should grow after a valid authenticated request (proves the A2 needle-absence is meaningful)")
			})
		})

		// ─────────────────────────────────────────────────────────────────
		// P0 — bbr must run before EPP / router; the full chain must
		// actually deliver requests to the workload pod (vLLM).
		// ─────────────────────────────────────────────────────────────────

		Context("P0: bbr precedes EPP, full chain delivers traffic", func() {

			// B2 — Send N valid authenticated requests, then assert that
			// vLLM's `vllm:request_success_total{model_name=<modelName>}`
			// (scraped per-pod by ScrapeRequestSuccessTotal) grew by at
			// least N. This proves: ext_authz allowed the request through →
			// BBR injected the X-Gateway-Model-Name header → the HTTPRoute
			// matched on that header → EPP picked a backend → router
			// forwarded to a real vLLM pod. If BBR ran *after* EPP, the
			// HTTPRoute would miss and the request would hit the catch-all
			// 404, never updating vLLM's success counter.
			const requestCount = 5
			It("B2: N valid requests increase vLLM request_success_total by ≥N", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				before, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNS, modelName)
				Expect(err).NotTo(HaveOccurred())

				for i := 0; i < requestCount; i++ {
					resp, err := sendAuth(modelName, apiKey)
					Expect(err).NotTo(HaveOccurred())
					Expect(resp.StatusCode).To(Equal(http.StatusOK),
						"valid auth'd request %d should succeed", i)
					resp.Body.Close()
				}

				Eventually(func() float64 {
					after, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNS, modelName)
					if err != nil {
						return -1
					}
					diff := utils.DiffSnapshots(before, after)
					return utils.TotalDelta(diff)
				}, 60*time.Second, 3*time.Second).Should(BeNumerically(">=", float64(requestCount)),
					"vLLM success counter must grow by ≥%d (proves BBR→EPP→router→pod path ran)", requestCount)
			})

			// B1 — At least one vLLM shadow pod must have received traffic.
			// Together with B2 this rules out the failure mode where EPP is
			// bypassed and the request lands on the catch-all (in which
			// case the per-pod delta map would be entirely zero).
			It("B1: at least one shadow pod's per-pod counter increased", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				before, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNS, modelName)
				Expect(err).NotTo(HaveOccurred())

				resp, err := sendAuth(modelName, apiKey)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(http.StatusOK))
				resp.Body.Close()

				Eventually(func() int {
					after, err := utils.ScrapeRequestSuccessTotal(ctx, clientset, caseNS, modelName)
					if err != nil {
						return 0
					}
					diff := utils.DiffSnapshots(before, after)
					active := 0
					for _, d := range diff {
						if d > 0 {
							active++
						}
					}
					return active
				}, 60*time.Second, 3*time.Second).Should(BeNumerically(">=", 1),
					"EPP must pick a real pod (at least one shadow pod's success counter grew)")
			})
		})

		// ─────────────────────────────────────────────────────────────────
		// P0 — Static proof of filter chain order via Envoy admin
		// /config_dump on the Gateway pod.
		// ─────────────────────────────────────────────────────────────────

		Context("P0: Envoy filter chain order (config_dump)", func() {

			// F1 — Read the HCM's `http_filters` list directly from the
			// Gateway pod's Envoy admin port and assert the relative order
			// of the four filters we care about. This is the strongest
			// possible assertion because it does not depend on any business
			// request behaviour: it checks the rendered xDS config itself.
			It("F1: HCM filter order is ext_authz → bbr → ext_proc → router", func() {
				gwPod, err := firstRunningPod(ctx, caseNS, gatewayLabel)
				Expect(err).NotTo(HaveOccurred(),
					"per-namespace Gateway pod should be Running")

				dump, err := kubectlExec(caseNS, gwPod,
					"curl", "-s", "http://127.0.0.1:15000/config_dump")
				Expect(err).NotTo(HaveOccurred(),
					"failed to read Envoy admin /config_dump from %s/%s", caseNS, gwPod)

				filters := extractGatewayHTTPFilterNames(dump)
				Expect(filters).NotTo(BeEmpty(),
					"could not parse any HCM http_filters out of /config_dump (first 2k bytes: %s)",
					truncate(dump, 2000))

				idx := func(prefix string) int {
					for i, f := range filters {
						if strings.HasPrefix(f, prefix) {
							return i
						}
					}
					return -1
				}
				authIdx := idx("envoy.filters.http.ext_authz")
				bbrIdx := idx("envoy.filters.http.ext_proc.bbr")
				eppIdx := -1
				// EPP ext_proc is named generically; pick the first
				// `envoy.filters.http.ext_proc*` that is NOT the BBR one.
				for i, f := range filters {
					if strings.HasPrefix(f, "envoy.filters.http.ext_proc") &&
						!strings.HasPrefix(f, "envoy.filters.http.ext_proc.bbr") {
						eppIdx = i
						break
					}
				}
				routerIdx := idx("envoy.filters.http.router")

				Expect(authIdx).To(BeNumerically(">=", 0),
					"ext_authz must be present on the auth-enabled Gateway; got filters=%v", filters)
				Expect(bbrIdx).To(BeNumerically(">=", 0),
					"bbr ext_proc must be present; got filters=%v", filters)
				Expect(eppIdx).To(BeNumerically(">=", 0),
					"InferencePool ext_proc must be present; got filters=%v", filters)
				Expect(routerIdx).To(BeNumerically(">=", 0),
					"router must be present; got filters=%v", filters)

				Expect(authIdx).To(BeNumerically("<", bbrIdx),
					"ext_authz must precede BBR (got %v)", filters)
				Expect(bbrIdx).To(BeNumerically("<", eppIdx),
					"BBR must precede InferencePool ext_proc (got %v)", filters)
				Expect(eppIdx).To(BeNumerically("<", routerIdx),
					"InferencePool ext_proc must precede router (got %v)", filters)
			})
		})

		// ─────────────────────────────────────────────────────────────────
		// P1 — bbr ↔ router interaction, invalid-key + unknown-model
		// ─────────────────────────────────────────────────────────────────

		Context("P1: catch-all preserves auth, BBR controls header injection", func() {

			// D2 — Invalid bearer token on an unknown model name must
			// still return 401, not 404. This is the dual of A1: an attacker
			// supplying an invalid key combined with an unknown model
			// previously slipped through to the unauthenticated catch-all.
			It("D2: invalid API key + unknown model returns 401, not 404", func() {
				Eventually(func() int {
					resp, err := sendAuth("does-not-exist-model", "invalid-key-12345")
					if err != nil {
						return 0
					}
					defer resp.Body.Close()
					return resp.StatusCode
				}, 60*time.Second, 5*time.Second).Should(Equal(http.StatusUnauthorized),
					"invalid token + unknown model must be rejected by ext_authz (401), not by catch-all (404)")
			})

			// E1 — Request body with no `model` field. BBR cannot inject
			// X-Gateway-Model-Name, the HTTPRoute header match fails, and
			// the router falls through to the catch-all 404. Because the
			// request *is* authenticated, the 404 (with the OpenAI-style
			// `model_not_found` body) proves the chain's BBR-decided header
			// drove the route choice (the router did NOT run before BBR).
			It("E1: authed request with missing model field returns 404 model_not_found", func() {
				body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
				resp, err := sendRaw(body, "application/json", apiKey)
				Expect(err).NotTo(HaveOccurred())
				defer resp.Body.Close()

				Expect(resp.StatusCode).To(Equal(http.StatusNotFound),
					"authed request without `model` should fall through to catch-all 404")
				errResp, err := utils.ParseErrorResponse(resp)
				Expect(err).NotTo(HaveOccurred())
				Expect(errResp.ErrorCode()).To(Equal("model_not_found"))
			})
		})

		// ─────────────────────────────────────────────────────────────────
		// P2 — Coverage of secondary properties
		// ─────────────────────────────────────────────────────────────────

		Context("P2: EPP isolation, catch-all transits BBR, content-type handling", func() {

			// A3 — An unauth'd request must not reach EPP. EPP runs at
			// --v=1 (see charts/modeldeployment/templates/epp-deployment.yaml)
			// and logs the inference pool name plus the request model. We
			// snapshot EPP logs around the unauth'd request and look for
			// the request-specific needle.
			It("A3: unauth'd request never reaches the EPP pod", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				eppPods, err := utils.GetEPPPods(ctx, clientset, modelName, caseNS)
				Expect(err).NotTo(HaveOccurred())
				Expect(eppPods).NotTo(BeEmpty())
				eppPod := eppPods[0].Name

				before, err := utils.GetPodLogs(clientset, caseNS, eppPod, "epp")
				Expect(err).NotTo(HaveOccurred())
				beforeLen := len(before)

				needle := fmt.Sprintf("a3-no-epp-%d", time.Now().UnixNano())
				resp, err := sendAuth(needle, "")
				Expect(err).NotTo(HaveOccurred())
				defer resp.Body.Close()
				Expect(resp.StatusCode).To(Equal(http.StatusUnauthorized))

				time.Sleep(3 * time.Second)

				after, err := utils.GetPodLogs(clientset, caseNS, eppPod, "epp")
				Expect(err).NotTo(HaveOccurred())
				delta := after
				if len(after) >= beforeLen {
					delta = after[beforeLen:]
				}
				Expect(delta).NotTo(ContainSubstring(needle),
					"EPP should not have observed the unauth'd request; needle %q surfaced in new log slice", needle)
			})

			// D3 — Catch-all path is still preceded by BBR (otherwise we
			// could not maintain the ordering invariant for malformed
			// requests). Snapshot BBR logs around an authed request for an
			// unknown model and assert BBR's log grew.
			It("D3: authed + unknown model still transits BBR (catch-all path)", func() {
				clientset, err := utils.GetK8sClientset()
				Expect(err).NotTo(HaveOccurred())

				bbrNS := "istio-system"
				bbrPod, err := firstRunningPod(ctx, bbrNS,
					"app.kubernetes.io/name=body-based-routing")
				Expect(err).NotTo(HaveOccurred())

				before, err := utils.GetPodLogs(clientset, bbrNS, bbrPod, "bbr")
				Expect(err).NotTo(HaveOccurred())
				beforeLen := len(before)

				resp, err := sendAuth("d3-unknown-model", apiKey)
				Expect(err).NotTo(HaveOccurred())
				defer resp.Body.Close()
				Expect(resp.StatusCode).To(Equal(http.StatusNotFound),
					"unknown model with valid auth should hit catch-all 404")

				time.Sleep(3 * time.Second)

				after, err := utils.GetPodLogs(clientset, bbrNS, bbrPod, "bbr")
				Expect(err).NotTo(HaveOccurred())
				Expect(len(after)).To(BeNumerically(">", beforeLen),
					"BBR log must grow even for catch-all paths (proves BBR runs before router on every request)")
			})

			// E2 — Non-JSON Content-Type. BBR's body-field-to-header plugin
			// (gateway-api-inference-extension) attempts JSON-unmarshal on
			// every request body regardless of the Content-Type header, so
			// a syntactically-valid JSON payload with Content-Type=text/plain
			// still has its `model` field extracted into X-Gateway-Model-Name
			// and the request routes successfully (HTTP 200). The contract
			// we care about here is that mismatched Content-Type does not
			// crash any filter or produce a 5xx — the exact 2xx/4xx outcome
			// depends on whether BBR can parse the body and is intentionally
			// not asserted to avoid flaking on filter internals.
			It("E2: non-JSON Content-Type does not cause 5xx", func() {
				body := []byte(`{"model":"` + modelName + `","messages":[{"role":"user","content":"hi"}]}`)
				resp, err := sendRaw(body, "text/plain", apiKey)
				Expect(err).NotTo(HaveOccurred())
				defer resp.Body.Close()
				Expect(resp.StatusCode).To(BeNumerically("<", 500),
					"non-JSON content-type must not crash any filter (no 5xx)")
			})
		})
	})

// firstRunningPod returns the name of the first Running pod that matches
// labelSelector in the given namespace. Uses the shared GetK8sClientset
// so callers don't have to plumb the clientset through.
func firstRunningPod(ctx context.Context, namespace, labelSelector string) (string, error) {
	clientset, err := utils.GetK8sClientset()
	if err != nil {
		return "", err
	}
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return "", fmt.Errorf("list pods in %s with %q: %w", namespace, labelSelector, err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no Running pods in %s match %q", namespace, labelSelector)
	}
	return pods.Items[0].Name, nil
}

// kubectlExec runs `kubectl exec` and returns the combined stdout/stderr.
// We shell out (rather than using the K8s REST exec subresource) to stay
// consistent with the rest of this suite which already shells out for
// port-forward / helm / kubectl operations.
func kubectlExec(namespace, pod string, command ...string) (string, error) {
	args := append([]string{"exec", "-n", namespace, pod, "--"}, command...)
	cmd := exec.Command("kubectl", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("kubectl %s: %w (output: %s)",
			strings.Join(args, " "), err, truncate(out.String(), 1000))
	}
	return out.String(), nil
}

// extractGatewayHTTPFilterNames parses an Envoy /config_dump JSON blob and
// returns the names of HTTP filters configured on the Gateway-context
// listener (i.e. inbound listener serving Gateway traffic).
//
// /config_dump structure (simplified):
//
//	{ "configs": [
//	    { "@type": ".../ListenersConfigDump",
//	      "dynamic_listeners": [
//	        { "active_state": {
//	            "listener": {
//	              "filter_chains": [
//	                { "filters": [
//	                    { "typed_config": {
//	                        "@type": ".../HttpConnectionManager",
//	                        "http_filters": [{"name":"..."}, ...] }} ] } ] } } ] } ] }
//
// We walk every dynamic_listener and return the first listener that
// contains at least one ext_authz + ext_proc + router filter (which is
// the inference-traffic HCM on the Gateway). This avoids hard-coding the
// listener name (Istio generates a number-suffixed name per Gateway pod).
func extractGatewayHTTPFilterNames(configDump string) []string {
	var root struct {
		Configs []map[string]json.RawMessage `json:"configs"`
	}
	if err := json.Unmarshal([]byte(configDump), &root); err != nil {
		return nil
	}

	for _, cfg := range root.Configs {
		raw, ok := cfg["dynamic_listeners"]
		if !ok {
			continue
		}
		var listeners []struct {
			ActiveState struct {
				Listener struct {
					FilterChains []struct {
						Filters []struct {
							TypedConfig struct {
								Type        string `json:"@type"`
								HTTPFilters []struct {
									Name string `json:"name"`
								} `json:"http_filters"`
							} `json:"typed_config"`
						} `json:"filters"`
					} `json:"filter_chains"`
				} `json:"listener"`
			} `json:"active_state"`
		}
		if err := json.Unmarshal(raw, &listeners); err != nil {
			continue
		}
		for _, l := range listeners {
			for _, fc := range l.ActiveState.Listener.FilterChains {
				for _, f := range fc.Filters {
					if !strings.Contains(f.TypedConfig.Type, "HttpConnectionManager") {
						continue
					}
					names := make([]string, 0, len(f.TypedConfig.HTTPFilters))
					for _, hf := range f.TypedConfig.HTTPFilters {
						names = append(names, hf.Name)
					}
					if hasInferenceFilters(names) {
						return names
					}
				}
			}
		}
	}
	return nil
}

// hasInferenceFilters returns true when the HCM's http_filters list is
// the one serving inference traffic (i.e. carries both an ext_authz and a
// router). On Istio Gateway pods there is usually a single dynamic HCM,
// but we keep the check defensive in case sidecar / metrics listeners
// also appear in /config_dump.
func hasInferenceFilters(names []string) bool {
	var hasAuthz, hasRouter bool
	for _, n := range names {
		if strings.HasPrefix(n, "envoy.filters.http.ext_authz") {
			hasAuthz = true
		}
		if strings.HasPrefix(n, "envoy.filters.http.router") {
			hasRouter = true
		}
	}
	return hasAuthz && hasRouter
}

// truncate returns s capped at n bytes, with an ellipsis suffix when
// truncated. Used to keep error messages bounded.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
