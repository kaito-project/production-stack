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
	"fmt"
	"net/http"
	"time"
)

// BBR cluster-filter high-availability tests (issue #89).
//
// What this verifies (and why):
//
//	BBR ext_proc is a CLUSTER-SCOPE singleton on the hot path of every
//	inference request. Running a single replica makes it a single point
//	of failure, and a fail-closed cluster (failure_mode_allow: false)
//	would turn any single-replica blip into a cluster-wide `502
//	bbr_unavailable` outage. This group asserts:
//
//	  1. the BBR Deployment renders with >= 2 replicas + pod
//	     anti-affinity, and an active gRPC readiness probe;
//	  2. the loss of ONE replica is a transparent failover (no 404, no
//	     502, virtually every request still served); and
//	  3. the fail-closed 5xx path fires only when ALL replicas are down,
//	     never as a silent 404.
//
//	This suite perturbs the shared cluster-wide BBR Deployment, so the
//	Ginkgo adapter MUST decorate its Describe Serial — no other spec may
//	run while BBR is degraded.

// ClusterFilterHAConfig configures one run of the BBR cluster-filter HA
// group.
type ClusterFilterHAConfig struct {
	Suffix     Suffix
	Namespace  string
	Deployment DeploymentSpec // non-auth; this case does not enable ext_authz.

	Lifecycle Lifecycle
	Chat      ChatClient
	Kube      KubeOps
	Logger    Logger

	// BBRNamespace/BBRDeploymentName/BBRHealthPort locate and describe
	// the cluster-wide BBR Deployment. Defaults: "kaito-system",
	// "body-based-router", 9005.
	BBRNamespace      string
	BBRDeploymentName string
	BBRHealthPort     int32
	// BurstInterval is the delay between requests in
	// AssertServesAtSingleReplica's burst probe. Defaults to 1 second
	// (production pacing); unit tests override it to keep runtime
	// bounded.
	BurstInterval time.Duration
}

// ClusterFilterHAState is the resolved state produced by
// ClusterFilterHASetup, threaded into every cluster-filter-HA assertion.
type ClusterFilterHAState struct {
	cfg       ClusterFilterHAConfig
	namespace string
	modelName string
	access    NamespaceAccess
	// bbrTargetReplicas is the BBR Deployment's original desired replica
	// count (spec.replicas), captured during Setup and required to be
	// >= 2. Every restore/recovery path (AssertRecoversToFullReplicaCount,
	// AssertFailsClosedWhenAllReplicasDown, TeardownBBR) scales back to
	// this captured value rather than a hardcoded 2, so a BBR Deployment
	// rendered with more replicas (e.g. 3) is restored to its own actual
	// desired count, not silently narrowed. Zero when never discovered
	// (e.g. Setup failed before reaching that step) — bbrTarget() falls
	// back to the schema minimum (2) in that case.
	bbrTargetReplicas int32
}

// bbrTarget returns the replica count every restore/recovery path should
// scale BBR back to: the captured original desired count, or the schema
// minimum (2) if Setup never got far enough to discover it.
func (s ClusterFilterHAState) bbrTarget() int32 {
	if s.bbrTargetReplicas > 0 {
		return s.bbrTargetReplicas
	}
	return 2
}

// Resources returns the physical resources this run owns, for Cleanup /
// Diagnostics. The cluster-wide BBR Deployment is NOT included — this
// group only perturbs its replica count and always restores it; it is
// never created or deleted by this group.
func (s ClusterFilterHAState) Resources() GroupResources {
	return GroupResources{
		Namespaces: []string{s.namespace},
		Deployments: []DeploymentSpec{
			{Namespace: s.namespace, Name: s.modelName},
		},
	}
}

func (c ClusterFilterHAConfig) bbrNamespace() string {
	if c.BBRNamespace != "" {
		return c.BBRNamespace
	}
	return "kaito-system"
}

func (c ClusterFilterHAConfig) bbrDeploymentName() string {
	if c.BBRDeploymentName != "" {
		return c.BBRDeploymentName
	}
	return "body-based-router"
}

func (c ClusterFilterHAConfig) bbrHealthPort() int32 {
	if c.BBRHealthPort != 0 {
		return c.BBRHealthPort
	}
	return 9005
}

func (c ClusterFilterHAConfig) burstInterval() time.Duration {
	if c.BurstInterval != 0 {
		return c.BurstInterval
	}
	return time.Second
}

// ClusterFilterHASetup provisions the group's namespace and single
// non-auth model deployment (mirrors the gpu-mocker case: one replica is
// enough, since this case exercises the cluster-wide BBR Deployment's HA,
// not the model pool's), captures the BBR Deployment's original (>=2)
// desired replica count, and waits for the baseline request path to be
// healthy through BBR.
//
// The returned ClusterFilterHAState always carries cfg plus the resolved
// physical namespace/deployment identities — even when an error is
// returned — so a caller can still call state.Resources() (and thus
// Cleanup / Diagnostics) to locate whatever was partially created before
// the failure, and TeardownBBR can still restore BBR using whatever
// target replica count (captured or the schema-minimum fallback) is
// available.
func ClusterFilterHASetup(ctx context.Context, cfg ClusterFilterHAConfig) (ClusterFilterHAState, error) {
	namespace := cfg.Suffix.Namespace(cfg.Namespace)
	deployment := cfg.Deployment
	deployment.Namespace = namespace
	deployment.Name = cfg.Suffix.Deployment(deployment.Name)
	deployment.AuthAPIKeyEnabled = false

	state := ClusterFilterHAState{
		cfg:       cfg,
		namespace: namespace,
		modelName: deployment.Name,
	}

	if err := cfg.Lifecycle.EnsureNamespace(ctx, namespace, false); err != nil {
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

	// Capture BBR's own original desired replica count (spec.replicas)
	// rather than assuming 2, so a Deployment rendered with more
	// replicas (e.g. 3) is restored to its own actual count everywhere
	// below, not silently narrowed to 2.
	bbrNS, bbrDeploy := cfg.bbrNamespace(), cfg.bbrDeploymentName()
	bbrDeployment, err := cfg.Kube.GetDeployment(ctx, bbrNS, bbrDeploy)
	if err != nil {
		return state, fmt.Errorf("read BBR Deployment %s/%s: %w", bbrNS, bbrDeploy, err)
	}
	if bbrDeployment.Spec.Replicas == nil || *bbrDeployment.Spec.Replicas < 2 {
		return state, fmt.Errorf("BBR Deployment %s/%s must declare spec.replicas >= 2 (schema minimum) to run this HA group; got %v",
			bbrNS, bbrDeploy, bbrDeployment.Spec.Replicas)
	}
	state.bbrTargetReplicas = *bbrDeployment.Spec.Replicas

	// BBR must be HA (at its own full desired count) before we start
	// removing replicas.
	if err := cfg.Kube.WaitForReadyReplicas(ctx, bbrNS, bbrDeploy, state.bbrTarget(), 3*time.Minute); err != nil {
		return state, fmt.Errorf("BBR Deployment must run >= %d ready replicas for HA (issue #89): %w", state.bbrTarget(), err)
	}

	// Confirm the baseline request path is healthy through BBR.
	if err := pollUntilOK(ctx, 5*time.Minute, 10*time.Second, func() (int, error) {
		resp, err := state.sendChatWithRetry(ctx)
		if err != nil {
			return 0, err
		}
		return resp.StatusCode, nil
	}); err != nil {
		return state, fmt.Errorf("baseline request path through BBR should return 200: %w", err)
	}

	return state, nil
}

// TeardownBBR is best-effort restoration of the BBR Deployment's
// original (captured during Setup, or the schema-minimum fallback)
// replica count, meant to run unconditionally in the Ginkgo adapter's
// AfterAll (independent of whether the group's own namespace/deployment
// cleanup runs) so a failed spec never leaves the shared cluster-wide
// dataplane degraded for the rest of the suite.
func TeardownBBR(ctx context.Context, state ClusterFilterHAState) {
	want := state.bbrTarget()
	_ = state.cfg.Kube.ScaleDeployment(ctx, state.cfg.bbrNamespace(), state.cfg.bbrDeploymentName(), want)
	_ = state.cfg.Kube.WaitForReadyReplicas(ctx, state.cfg.bbrNamespace(), state.cfg.bbrDeploymentName(), want, 3*time.Minute)
}

// sendChatWithRetry sends a chat-completion request, retrying on
// transport-level errors (a handful of attempts, short backoff) since
// this group perturbs the shared BBR Deployment and expects occasional
// connection blips, not HTTP-level failures. Requests automatically
// carry s.access.APIKey / HostHeader when the Lifecycle populated them
// (e.g. an AIManager-mode Lifecycle that always provisions an API key)
// and omit them when absent (Helm mode's default non-auth namespace for
// this case), so the group works unmodified against either mode.
func (s ClusterFilterHAState) sendChatWithRetry(ctx context.Context) (*ChatResponse, error) {
	const maxAttempts = 3
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		resp, err := s.cfg.Chat.SendChat(ctx, s.access.GatewayURL, ChatRequest{
			Model:       s.modelName,
			Prompt:      "hello",
			BearerToken: s.access.APIKey,
			HostHeader:  s.access.HostHeader,
		})
		if err == nil {
			return resp, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// pollUntilOK retries fn until it reports HTTP 200 or the timeout
// elapses.
func pollUntilOK(ctx context.Context, timeout, interval time.Duration, fn func() (int, error)) error {
	deadline := time.Now().Add(timeout)
	var lastStatus int
	var lastErr error
	for {
		status, err := fn()
		if err == nil && status == http.StatusOK {
			return nil
		}
		lastStatus, lastErr = status, err
		if time.Now().After(deadline) {
			if lastErr != nil {
				return lastErr
			}
			return fmt.Errorf("expected 200, got %d", lastStatus)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// ClusterFilterHAAssertions returns the group's named, independently
// reportable checks in execution order. Order matters: the
// single-replica and all-replicas-down checks intentionally perturb and
// restore the shared BBR Deployment in sequence.
func ClusterFilterHAAssertions(state ClusterFilterHAState) []Assertion {
	return []Assertion{
		namedAssertion("renders the BBR Deployment with >= 2 replicas and pod anti-affinity", func(ctx context.Context) error {
			return AssertBBRHighAvailabilityRendered(ctx, state)
		}),
		namedAssertion("configures an active gRPC readiness probe on the BBR health port", func(ctx context.Context) error {
			return AssertBBRReadinessProbeConfigured(ctx, state)
		}),
		namedAssertion("keeps serving prompts while running at a single replica", func(ctx context.Context) error {
			return AssertServesAtSingleReplica(ctx, state)
		}),
		namedAssertion("recovers to the full replica count after running degraded", func(ctx context.Context) error {
			return AssertRecoversToFullReplicaCount(ctx, state)
		}),
		namedAssertion("fails closed (no silent 404) when all BBR replicas are down", func(ctx context.Context) error {
			return AssertFailsClosedWhenAllReplicasDown(ctx, state)
		}),
	}
}

// AssertBBRHighAvailabilityRendered asserts the BBR Deployment's
// spec.replicas >= 2 and that pod anti-affinity is configured to spread
// replicas across nodes.
func AssertBBRHighAvailabilityRendered(ctx context.Context, state ClusterFilterHAState) error {
	d, err := state.cfg.Kube.GetDeployment(ctx, state.cfg.bbrNamespace(), state.cfg.bbrDeploymentName())
	if err != nil {
		return fmt.Errorf("BBR Deployment should exist in %s: %w", state.cfg.bbrNamespace(), err)
	}

	if d.Spec.Replicas == nil {
		return fmt.Errorf("BBR spec.replicas must be set")
	}
	if *d.Spec.Replicas < 2 {
		return fmt.Errorf("BBR spec.replicas must be >= 2 (schema minimum); got %d", *d.Spec.Replicas)
	}

	aff := d.Spec.Template.Spec.Affinity
	if aff == nil || aff.PodAntiAffinity == nil {
		return fmt.Errorf("BBR pod spec must set podAntiAffinity to spread replicas across nodes")
	}
	hasTerm := len(aff.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) > 0 ||
		len(aff.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0
	if !hasTerm {
		return fmt.Errorf("podAntiAffinity must declare at least one (preferred or required) spread term")
	}
	return nil
}

// AssertBBRReadinessProbeConfigured asserts the "bbr" container defines
// an active gRPC readiness probe targeting the health port (not the
// ext_proc port).
func AssertBBRReadinessProbeConfigured(ctx context.Context, state ClusterFilterHAState) error {
	d, err := state.cfg.Kube.GetDeployment(ctx, state.cfg.bbrNamespace(), state.cfg.bbrDeploymentName())
	if err != nil {
		return err
	}

	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name != "bbr" {
			continue
		}
		if c.ReadinessProbe == nil {
			return fmt.Errorf("bbr container must define a readinessProbe")
		}
		if c.ReadinessProbe.GRPC == nil {
			return fmt.Errorf("bbr readinessProbe must be a gRPC probe (active grpc.health.v1.Health check)")
		}
		if c.ReadinessProbe.GRPC.Port != state.cfg.bbrHealthPort() {
			return fmt.Errorf("bbr readinessProbe must target the health port %d, not the ext_proc port; got %d",
				state.cfg.bbrHealthPort(), c.ReadinessProbe.GRPC.Port)
		}
		return nil
	}
	return fmt.Errorf("bbr container not found on the BBR Deployment")
}

// AssertServesAtSingleReplica scales BBR down to one replica (rather than
// deleting a pod, which the Deployment controller would reconcile back
// almost immediately) and sends a burst of requests, asserting no 404
// (fail-open) and no 502 (premature fail-closed) is observed.
func AssertServesAtSingleReplica(ctx context.Context, state ClusterFilterHAState) error {
	bbrNS := state.cfg.bbrNamespace()
	bbrDeploy := state.cfg.bbrDeploymentName()

	if err := state.cfg.Kube.ScaleDeployment(ctx, bbrNS, bbrDeploy, 1); err != nil {
		return err
	}

	// Wait until the cluster is genuinely degraded: exactly one replica
	// Ready, so the burst below is not racing convergence.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		ready, err := state.cfg.Kube.ReadyReplicas(ctx, bbrNS, bbrDeploy)
		if err == nil && ready == 1 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("BBR should hold at exactly one ready replica")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	const burst = 15
	var ok, notFound, badGateway, other int
	for i := 0; i < burst; i++ {
		resp, err := state.sendChatWithRetry(ctx)
		if err != nil {
			other++
			time.Sleep(state.cfg.burstInterval())
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
		time.Sleep(state.cfg.burstInterval())
	}
	state.cfg.Logger.Logf("single-replica burst: ok=%d notFound=%d badGateway=%d other=%d", ok, notFound, badGateway, other)

	if notFound != 0 {
		return fmt.Errorf("a single healthy replica must not cause 404 fall-through (BBR must stay fail-closed, not fail-open); got %d", notFound)
	}
	if badGateway != 0 {
		return fmt.Errorf("a single healthy replica must not trip the 502 bbr_unavailable path; got %d", badGateway)
	}
	// Allow a single transient miss (port-forward blip, model timeout):
	// the load-bearing HA invariants are no 404 fall-through and no
	// premature 502, not a strict 100% pass rate.
	if ok < burst-1 {
		return fmt.Errorf("a single healthy BBR replica should serve virtually every request; ok=%d/%d", ok, burst)
	}
	return nil
}

// AssertRecoversToFullReplicaCount scales BBR back to its HA replica
// count and waits for it to converge.
func AssertRecoversToFullReplicaCount(ctx context.Context, state ClusterFilterHAState) error {
	bbrNS := state.cfg.bbrNamespace()
	bbrDeploy := state.cfg.bbrDeploymentName()
	want := state.bbrTarget()
	if err := state.cfg.Kube.ScaleDeployment(ctx, bbrNS, bbrDeploy, want); err != nil {
		return err
	}
	if err := state.cfg.Kube.WaitForReadyReplicas(ctx, bbrNS, bbrDeploy, want, 3*time.Minute); err != nil {
		return fmt.Errorf("BBR should return to >= %d ready replicas after scaling back up: %w", want, err)
	}
	return nil
}

// AssertFailsClosedWhenAllReplicasDown scales BBR to zero and asserts
// requests fail CLOSED (5xx, never a silent 404), then restores BBR and
// confirms the request path recovers to 200.
func AssertFailsClosedWhenAllReplicasDown(ctx context.Context, state ClusterFilterHAState) error {
	bbrNS := state.cfg.bbrNamespace()
	bbrDeploy := state.cfg.bbrDeploymentName()

	if err := state.cfg.Kube.ScaleDeployment(ctx, bbrNS, bbrDeploy, 0); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		ready, err := state.cfg.Kube.ReadyReplicas(ctx, bbrNS, bbrDeploy)
		if err == nil && ready == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("BBR should report zero ready replicas after scaling to zero")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	// With every replica gone and failure_mode_allow: false, the request
	// must fail CLOSED (5xx) — never silently fall through to a 404
	// model_not_found.
	failClosedDeadline := time.Now().Add(2 * time.Minute)
	for {
		resp, err := state.sendChatWithRetry(ctx)
		if err == nil && resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotFound {
			break
		}
		if time.Now().After(failClosedDeadline) {
			return fmt.Errorf("an all-replicas-down BBR must fail closed, not fall through to 404 model_not_found")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}

	if err := state.cfg.Kube.ScaleDeployment(ctx, bbrNS, bbrDeploy, state.bbrTarget()); err != nil {
		return err
	}
	if err := state.cfg.Kube.WaitForReadyReplicas(ctx, bbrNS, bbrDeploy, state.bbrTarget(), 3*time.Minute); err != nil {
		return fmt.Errorf("BBR should return to its HA replica count: %w", err)
	}

	if err := pollUntilOK(ctx, 3*time.Minute, 5*time.Second, func() (int, error) {
		resp, err := state.sendChatWithRetry(ctx)
		if err != nil {
			return 0, err
		}
		return resp.StatusCode, nil
	}); err != nil {
		return fmt.Errorf("request path should return 200 once BBR is healthy again: %w", err)
	}
	return nil
}
