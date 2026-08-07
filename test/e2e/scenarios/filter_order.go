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

package scenarios

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
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
//	`model-not-found` catch-all silently bypasses ext_authz, or BBR runs
//	after the InferencePool ext_proc and EPP routing falls through to
//	round-robin. Both have shipped at least once in main, so the order is
//	treated as a property worth its own regression suite. The case-level
//	deployment is auth-enabled so a single namespace exercises every
//	filter in the chain.

// FilterOrderConfig configures one run of the filter-execution-order
// group.
type FilterOrderConfig struct {
	Suffix     Suffix
	Namespace  string
	Deployment DeploymentSpec // AuthAPIKeyEnabled is forced true by Setup.

	Lifecycle Lifecycle
	Chat      ChatClient
	Kube      KubeOps
	Logger    Logger

	// BBRNamespace/BBRLabelSelector locate the cluster-wide BBR
	// Deployment's pods. Defaults: "kaito-system" /
	// "app.kubernetes.io/name=body-based-routing".
	BBRNamespace     string
	BBRLabelSelector string
}

// FilterOrderState is the resolved state produced by FilterOrderSetup,
// threaded into every filter-order assertion.
type FilterOrderState struct {
	cfg          FilterOrderConfig
	namespace    string
	modelName    string
	hostHeader   string
	gatewayLabel string
	access       NamespaceAccess
}

// Resources returns the physical resources this run owns, for Cleanup /
// Diagnostics.
func (s FilterOrderState) Resources() GroupResources {
	return GroupResources{
		Namespaces: []string{s.namespace},
		Deployments: []DeploymentSpec{
			{Namespace: s.namespace, Name: s.modelName},
		},
	}
}

// FilterOrderSetup provisions the group's namespace and API-key-enabled
// deployment (so the full ext_authz → bbr → ext_proc(EPP) → router chain
// is exercised) and resolves the API key + case Gateway URL.
//
// The returned FilterOrderState always carries cfg plus the resolved
// physical namespace/deployment identities — even when an error is
// returned — so a caller can still call state.Resources() (and thus
// Cleanup / Diagnostics) to locate whatever was partially created before
// the failure.
func FilterOrderSetup(ctx context.Context, cfg FilterOrderConfig) (FilterOrderState, error) {
	namespace := cfg.Suffix.Namespace(cfg.Namespace)
	deployment := cfg.Deployment
	deployment.Namespace = namespace
	deployment.Name = cfg.Suffix.Deployment(deployment.Name)
	deployment.AuthAPIKeyEnabled = true

	state := FilterOrderState{
		cfg:          cfg,
		namespace:    namespace,
		modelName:    deployment.Name,
		hostHeader:   cfg.Suffix.HostHeader(namespace),
		gatewayLabel: "gateway.networking.k8s.io/gateway-name=" + cfg.Suffix.Gateway(namespace),
	}

	if err := cfg.Lifecycle.EnsureNamespace(ctx, namespace, true); err != nil {
		return state, fmt.Errorf("ensure namespace %s: %w", namespace, err)
	}
	if err := cfg.Lifecycle.EnsureModelDeployment(ctx, deployment); err != nil {
		return state, fmt.Errorf("ensure deployment %s/%s: %w", namespace, deployment.Name, err)
	}
	access, err := cfg.Lifecycle.NamespaceAccess(ctx, namespace)
	if err != nil {
		return state, fmt.Errorf("resolve namespace access for %s: %w", namespace, err)
	}
	state.access = access
	if access.HostHeader != "" {
		state.hostHeader = access.HostHeader
	}

	return state, nil
}

func (s FilterOrderState) bbrNamespace() string {
	if s.cfg.BBRNamespace != "" {
		return s.cfg.BBRNamespace
	}
	return "kaito-system"
}

func (s FilterOrderState) bbrLabelSelector() string {
	if s.cfg.BBRLabelSelector != "" {
		return s.cfg.BBRLabelSelector
	}
	return "app.kubernetes.io/name=body-based-routing"
}

func (s FilterOrderState) sendAuth(ctx context.Context, model, token string) (*ChatResponse, error) {
	return s.cfg.Chat.SendChat(ctx, s.access.GatewayURL, ChatRequest{
		Model:       model,
		Prompt:      "hello",
		BearerToken: token,
		HostHeader:  s.hostHeader,
	})
}

func (s FilterOrderState) sendRaw(ctx context.Context, body []byte, contentType, token string) (*ChatResponse, error) {
	return s.cfg.Chat.SendChat(ctx, s.access.GatewayURL, ChatRequest{
		RawBody:     body,
		ContentType: contentType,
		BearerToken: token,
		HostHeader:  s.hostHeader,
	})
}

// FilterOrderAssertions returns the group's named, independently
// reportable checks in execution order, matching filter_order_test.go's
// existing P0/P1/P2 matrix (A1, A2, A2-sanity, B2, B1, F1, D2, E1, A3,
// D3, E2).
func FilterOrderAssertions(state FilterOrderState) []Assertion {
	return []Assertion{
		namedAssertion("A1: unauth'd + unknown model returns 401, not 404", func(ctx context.Context) error {
			return AssertUnauthUnknownModelReturns401(ctx, state)
		}),
		namedAssertion("A2: unauth'd request never reaches BBR", func(ctx context.Context) error {
			return AssertUnauthRequestNeverReachesBBR(ctx, state)
		}),
		namedAssertion("A2 sanity: authenticated request DOES exercise BBR", func(ctx context.Context) error {
			return AssertAuthedRequestExercisesBBR(ctx, state)
		}),
		namedAssertion("B2: N valid requests increase vLLM request_success_total by >=N", func(ctx context.Context) error {
			return AssertValidRequestsIncreaseSuccessTotal(ctx, state, 5)
		}),
		namedAssertion("B1: at least one shadow pod's per-pod counter increased", func(ctx context.Context) error {
			return AssertAtLeastOneShadowPodServed(ctx, state)
		}),
		namedAssertion("F1: HCM filter order is ext_authz -> bbr -> ext_proc -> router", func(ctx context.Context) error {
			return AssertEnvoyFilterOrder(ctx, state)
		}),
		namedAssertion("D2: invalid API key + unknown model returns 401, not 404", func(ctx context.Context) error {
			return AssertInvalidKeyUnknownModelReturns401(ctx, state)
		}),
		namedAssertion("E1: authed request with missing model field returns 400 invalid_request_body", func(ctx context.Context) error {
			return AssertMissingModelFieldReturns400(ctx, state)
		}),
		namedAssertion("A3: unauth'd request never reaches the EPP pod", func(ctx context.Context) error {
			return AssertUnauthRequestNeverReachesEPP(ctx, state)
		}),
		namedAssertion("D3: authed + unknown model still transits BBR (catch-all path)", func(ctx context.Context) error {
			return AssertAuthedUnknownModelTransitsBBR(ctx, state)
		}),
		namedAssertion("E2: non-JSON Content-Type does not cause 5xx", func(ctx context.Context) error {
			return AssertNonJSONContentTypeNo5xx(ctx, state)
		}),
	}
}

// AssertUnauthUnknownModelReturns401 (A1). If the router executed before
// ext_authz, the unknown-model request would hit the catch-all
// `model-not-found-direct` EnvoyFilter and return 404; that is exactly
// the silent-bypass regression this guards against.
func AssertUnauthUnknownModelReturns401(ctx context.Context, state FilterOrderState) error {
	deadline := time.Now().Add(2 * time.Minute)
	var lastStatus int
	for {
		resp, err := state.sendAuth(ctx, "does-not-exist-model", "")
		if err == nil {
			lastStatus = resp.StatusCode
			if lastStatus == http.StatusUnauthorized {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("unauth'd request with an unknown model must be rejected by ext_authz (401), not by the catch-all (404); got %d", lastStatus)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// AssertUnauthRequestNeverReachesBBR (A2). Snapshots the BBR pod's log
// size before and after an unauth'd request and greps the new log lines
// for a unique correlation needle. BBR's body-field-to-header plugin
// logs the extracted model name, so any execution would surface here.
func AssertUnauthRequestNeverReachesBBR(ctx context.Context, state FilterOrderState) error {
	bbrNS := state.bbrNamespace()
	bbrPod, err := state.cfg.Kube.FirstRunningPod(ctx, bbrNS, state.bbrLabelSelector())
	if err != nil {
		return fmt.Errorf("BBR pod should be running in %s: %w", bbrNS, err)
	}

	before, err := state.cfg.Kube.PodLogs(ctx, bbrNS, bbrPod, "bbr")
	if err != nil {
		return err
	}
	beforeLen := len(before)

	needle := fmt.Sprintf("a2-no-bbr-%d", time.Now().UnixNano())
	resp, err := state.sendAuth(ctx, needle, "")
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("expected 401, got %d", resp.StatusCode)
	}

	// Give BBR's log writer time to flush — if it would have run.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(3 * time.Second):
	}

	after, err := state.cfg.Kube.PodLogs(ctx, bbrNS, bbrPod, "bbr")
	if err != nil {
		return err
	}
	delta := after
	if len(after) >= beforeLen {
		delta = after[beforeLen:]
	}
	if strings.Contains(delta, needle) {
		return fmt.Errorf("BBR should not have seen the unauth'd request body; found needle %q in new log slice", needle)
	}
	return nil
}

// AssertAuthedRequestExercisesBBR (A2 sanity). Counter-test for A2: a
// fully authenticated request for the same unique-needle model must
// increase BBR log output, proving A2's needle-absence is meaningful
// (BBR wasn't simply silent).
func AssertAuthedRequestExercisesBBR(ctx context.Context, state FilterOrderState) error {
	bbrNS := state.bbrNamespace()
	bbrPod, err := state.cfg.Kube.FirstRunningPod(ctx, bbrNS, state.bbrLabelSelector())
	if err != nil {
		return err
	}

	before, err := state.cfg.Kube.PodLogs(ctx, bbrNS, bbrPod, "bbr")
	if err != nil {
		return err
	}
	beforeLen := len(before)

	resp, err := state.sendAuth(ctx, state.modelName, state.access.APIKey)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("expected 200, got %d", resp.StatusCode)
	}

	// Poll for the log to grow rather than sleeping a fixed interval:
	// BBR logs via klog, which buffers and flushes on a periodic (~5s)
	// timer, so a single fixed wait races the flush.
	deadline := time.Now().Add(30 * time.Second)
	for {
		after, err := state.cfg.Kube.PodLogs(ctx, bbrNS, bbrPod, "bbr")
		if err == nil && len(after) > beforeLen {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("BBR log size should grow after a valid authenticated request (proves the A2 needle-absence is meaningful)")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// AssertValidRequestsIncreaseSuccessTotal (B2). Sends requestCount valid
// authenticated requests and asserts vLLM's
// vllm:request_success_total{model_name} grew by at least requestCount —
// proving ext_authz allowed the request → BBR injected the header → the
// HTTPRoute matched → EPP picked a backend → router forwarded to a real
// vLLM pod.
func AssertValidRequestsIncreaseSuccessTotal(ctx context.Context, state FilterOrderState, requestCount int) error {
	before, err := state.cfg.Kube.RequestSuccessTotalSnapshot(ctx, state.namespace, state.modelName)
	if err != nil {
		return err
	}

	for i := 0; i < requestCount; i++ {
		resp, err := state.sendAuth(ctx, state.modelName, state.access.APIKey)
		if err != nil {
			return fmt.Errorf("valid auth'd request %d: %w", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("valid auth'd request %d should succeed, got %d", i, resp.StatusCode)
		}
	}

	deadline := time.Now().Add(60 * time.Second)
	for {
		after, err := state.cfg.Kube.RequestSuccessTotalSnapshot(ctx, state.namespace, state.modelName)
		if err == nil {
			diff := DiffSnapshots(before, after)
			if TotalMetricDelta(diff) >= float64(requestCount) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("vLLM success counter must grow by >=%d (proves BBR->EPP->router->pod path ran)", requestCount)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// AssertAtLeastOneShadowPodServed (B1). Together with B2, rules out the
// failure mode where EPP is bypassed and the request lands on the
// catch-all (in which case the per-pod delta map would be entirely
// zero).
func AssertAtLeastOneShadowPodServed(ctx context.Context, state FilterOrderState) error {
	before, err := state.cfg.Kube.RequestSuccessTotalSnapshot(ctx, state.namespace, state.modelName)
	if err != nil {
		return err
	}

	resp, err := state.sendAuth(ctx, state.modelName, state.access.APIKey)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("expected 200, got %d", resp.StatusCode)
	}

	deadline := time.Now().Add(60 * time.Second)
	for {
		after, err := state.cfg.Kube.RequestSuccessTotalSnapshot(ctx, state.namespace, state.modelName)
		if err == nil {
			diff := DiffSnapshots(before, after)
			active := 0
			for _, d := range diff {
				if d > 0 {
					active++
				}
			}
			if active >= 1 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("EPP must pick a real pod (at least one shadow pod's success counter grew)")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// AssertEnvoyFilterOrder (F1). Reads the HCM's http_filters list directly
// from the Gateway pod's Envoy admin port and asserts the relative order
// of the four filters. Strongest possible assertion — it checks the
// rendered xDS config itself, not request-observed behaviour.
func AssertEnvoyFilterOrder(ctx context.Context, state FilterOrderState) error {
	gwPod, err := state.cfg.Kube.FirstRunningPod(ctx, state.namespace, state.gatewayLabel)
	if err != nil {
		return fmt.Errorf("per-namespace Gateway pod should be Running: %w", err)
	}

	dump, err := state.cfg.Kube.ExecInPod(ctx, state.namespace, gwPod,
		"curl", "-s", "http://127.0.0.1:15000/config_dump")
	if err != nil {
		return fmt.Errorf("failed to read Envoy admin /config_dump from %s/%s: %w", state.namespace, gwPod, err)
	}

	filters := extractGatewayHTTPFilterNames(dump)
	if len(filters) == 0 {
		return fmt.Errorf("could not parse any HCM http_filters out of /config_dump (first 2k bytes: %s)", truncate(dump, 2000))
	}

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
	for i, f := range filters {
		if strings.HasPrefix(f, "envoy.filters.http.ext_proc") &&
			!strings.HasPrefix(f, "envoy.filters.http.ext_proc.bbr") {
			eppIdx = i
			break
		}
	}
	routerIdx := idx("envoy.filters.http.router")

	if authIdx < 0 {
		return fmt.Errorf("ext_authz must be present on the auth-enabled Gateway; got filters=%v", filters)
	}
	if bbrIdx < 0 {
		return fmt.Errorf("bbr ext_proc must be present; got filters=%v", filters)
	}
	if eppIdx < 0 {
		return fmt.Errorf("InferencePool ext_proc must be present; got filters=%v", filters)
	}
	if routerIdx < 0 {
		return fmt.Errorf("router must be present; got filters=%v", filters)
	}
	if !(authIdx < bbrIdx) {
		return fmt.Errorf("ext_authz must precede BBR (got %v)", filters)
	}
	if !(bbrIdx < eppIdx) {
		return fmt.Errorf("BBR must precede InferencePool ext_proc (got %v)", filters)
	}
	if !(eppIdx < routerIdx) {
		return fmt.Errorf("InferencePool ext_proc must precede router (got %v)", filters)
	}
	return nil
}

// AssertInvalidKeyUnknownModelReturns401 (D2). Dual of A1: an invalid key
// combined with an unknown model must still return 401, not slip through
// to the unauthenticated catch-all.
func AssertInvalidKeyUnknownModelReturns401(ctx context.Context, state FilterOrderState) error {
	deadline := time.Now().Add(60 * time.Second)
	var lastStatus int
	for {
		resp, err := state.sendAuth(ctx, "does-not-exist-model", "invalid-key-12345")
		if err == nil {
			lastStatus = resp.StatusCode
			if lastStatus == http.StatusUnauthorized {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("invalid token + unknown model must be rejected by ext_authz (401), not by catch-all (404); got %d", lastStatus)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// AssertMissingModelFieldReturns400 (E1). BBR cannot inject
// X-Gateway-Model-Name without a `model` field; the HTTPRoute header
// match fails and the router falls through to the absent-header half of
// the split catch-all -> 400 invalid_request_body. Reaching the catch-all
// (rather than 401) proves auth still ran; the 400 (rather than 404)
// proves BBR's absent header — not a present-but-unknown model — drove
// the route choice.
func AssertMissingModelFieldReturns400(ctx context.Context, state FilterOrderState) error {
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	resp, err := state.sendRaw(ctx, body, "application/json", state.access.APIKey)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusBadRequest {
		return fmt.Errorf("authed request without model field should fall through to the absent-header catch-all (400 invalid_request_body); got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("x-kaito-error-source"); got != "bbr" {
		return fmt.Errorf("expected x-kaito-error-source=bbr, got %q", got)
	}
	errResp, err := parseErrorResponse(resp.Body)
	if err != nil {
		return err
	}
	if errResp.errorCode() != "invalid_request_body" {
		return fmt.Errorf("expected error code invalid_request_body, got %q", errResp.errorCode())
	}
	return nil
}

// AssertUnauthRequestNeverReachesEPP (A3). EPP logs the inference pool
// name plus the request model; snapshot EPP logs around an unauth'd
// request and look for the request-specific needle.
func AssertUnauthRequestNeverReachesEPP(ctx context.Context, state FilterOrderState) error {
	eppPods, err := state.cfg.Kube.EPPPodNames(ctx, state.namespace, state.modelName)
	if err != nil {
		return err
	}
	if len(eppPods) == 0 {
		return fmt.Errorf("no EPP pods found for %s/%s", state.namespace, state.modelName)
	}
	eppPod := eppPods[0]

	before, err := state.cfg.Kube.PodLogs(ctx, state.namespace, eppPod, "epp")
	if err != nil {
		return err
	}
	beforeLen := len(before)

	needle := fmt.Sprintf("a3-no-epp-%d", time.Now().UnixNano())
	resp, err := state.sendAuth(ctx, needle, "")
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("expected 401, got %d", resp.StatusCode)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(3 * time.Second):
	}

	after, err := state.cfg.Kube.PodLogs(ctx, state.namespace, eppPod, "epp")
	if err != nil {
		return err
	}
	delta := after
	if len(after) >= beforeLen {
		delta = after[beforeLen:]
	}
	if strings.Contains(delta, needle) {
		return fmt.Errorf("EPP should not have observed the unauth'd request; needle %q surfaced in new log slice", needle)
	}
	return nil
}

// AssertAuthedUnknownModelTransitsBBR (D3). The catch-all path is still
// preceded by BBR (otherwise the ordering invariant could not hold for
// malformed requests). BBR runs as an HA Deployment so a single request
// may land on any replica — aggregate log length across every running
// replica rather than snapshotting one pod.
func AssertAuthedUnknownModelTransitsBBR(ctx context.Context, state FilterOrderState) error {
	bbrNS := state.bbrNamespace()
	bbrSelector := state.bbrLabelSelector()

	beforeLen, err := state.cfg.Kube.AggregatePodSetLogLen(ctx, bbrNS, bbrSelector, "bbr")
	if err != nil {
		return err
	}

	resp, err := state.sendAuth(ctx, "d3-unknown-model", state.access.APIKey)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("unknown model with valid auth should hit catch-all 404; got %d", resp.StatusCode)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		afterLen, err := state.cfg.Kube.AggregatePodSetLogLen(ctx, bbrNS, bbrSelector, "bbr")
		if err == nil && afterLen > beforeLen {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("BBR log must grow even for catch-all paths (proves BBR runs before router on every request)")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// AssertNonJSONContentTypeNo5xx (E2). BBR's body-field-to-header plugin
// attempts JSON-unmarshal on every request body regardless of
// Content-Type, so a syntactically-valid JSON payload with a mismatched
// Content-Type still routes. The only contract asserted here is "no
// crash" (no 5xx) — the exact 2xx/4xx outcome is intentionally not
// pinned, to avoid flaking on filter internals.
func AssertNonJSONContentTypeNo5xx(ctx context.Context, state FilterOrderState) error {
	body := []byte(`{"model":"` + state.modelName + `","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := state.sendRaw(ctx, body, "text/plain", state.access.APIKey)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("non-JSON content-type must not crash any filter (no 5xx); got %d", resp.StatusCode)
	}
	return nil
}

// -- pure parsing helpers (no I/O, no ginkgo/gomega) --

// errorResponse mirrors the OpenAI-compatible error envelope, tolerating
// both string and numeric `code` values (vLLM returns numeric, the
// catch-all returns string).
type errorResponse struct {
	Error struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
	} `json:"error"`
}

func (e *errorResponse) errorCode() string {
	if e == nil || e.Error.Code == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(e.Error.Code, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(e.Error.Code))
}

func parseErrorResponse(body []byte) (*errorResponse, error) {
	var result errorResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse error response JSON: %w (body: %s)", err, string(body))
	}
	return &result, nil
}

// extractGatewayHTTPFilterNames parses an Envoy /config_dump JSON blob and
// returns the names of HTTP filters configured on the Gateway-context
// listener (i.e. the inbound listener serving Gateway traffic).
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
// contains at least one ext_authz + router filter (the inference-traffic
// HCM on the Gateway). This avoids hard-coding the listener name (Istio
// generates a number-suffixed name per Gateway pod).
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

// hasInferenceFilters reports whether the HCM's http_filters list is the
// one serving inference traffic (i.e. carries both an ext_authz and a
// router). On Istio Gateway pods there is usually a single dynamic HCM,
// but the check stays defensive in case sidecar / metrics listeners also
// appear in /config_dump.
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
