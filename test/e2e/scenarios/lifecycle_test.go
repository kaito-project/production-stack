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
	"testing"
	"time"
)

func TestSuffix_EmptyIsNoop(t *testing.T) {
	var s Suffix // ""
	if got := s.Namespace("e2e-auth"); got != "e2e-auth" {
		t.Errorf("Namespace(%q) = %q, want unchanged", "e2e-auth", got)
	}
	if got := s.Deployment("auth-gemma"); got != "auth-gemma" {
		t.Errorf("Deployment(%q) = %q, want unchanged", "auth-gemma", got)
	}
}

func TestSuffix_AppliesConsistently(t *testing.T) {
	s := Suffix("w7")
	ns := s.Namespace("e2e-auth")
	dep := s.Deployment("auth-gemma")

	if want := "e2e-auth-w7"; ns != want {
		t.Errorf("Namespace = %q, want %q", ns, want)
	}
	if want := "auth-gemma-w7"; dep != want {
		t.Errorf("Deployment = %q, want %q", dep, want)
	}
	if want := ns + "-gw"; s.Gateway(ns) != want {
		t.Errorf("Gateway = %q, want %q", s.Gateway(ns), want)
	}
	if want := ns + ".gw.example.com"; s.HostHeader(ns) != want {
		t.Errorf("HostHeader = %q, want %q", s.HostHeader(ns), want)
	}
}

func TestGPUSmokeSetup_AppliesSuffixToNamespaceAndDeployment(t *testing.T) {
	lc := newFakeLifecycle()
	chat := &fakeChatClient{def: fakeChatRule{status: 200}}

	cfg := GPUSmokeConfig{
		Suffix:     Suffix("pr123"),
		Namespace:  "e2e-gpu-smoke",
		Deployment: DeploymentSpec{Name: "gpu-smoke-gemma", Model: "google/gemma-4-E2B-it", Replicas: 1},
		Lifecycle:  lc,
		Chat:       chat,
		Logger:     &fakeLogger{},
	}

	state, err := GPUSmokeSetup(t.Context(), cfg)
	if err != nil {
		t.Fatalf("GPUSmokeSetup: %v", err)
	}

	if want := "e2e-gpu-smoke-pr123"; state.namespace != want {
		t.Errorf("namespace = %q, want %q", state.namespace, want)
	}
	if want := "gpu-smoke-gemma-pr123"; state.modelName != want {
		t.Errorf("modelName = %q, want %q", state.modelName, want)
	}

	// The deployment passed to EnsureModelDeployment must carry the SAME
	// resolved (suffixed) namespace/name — resources must be consistent,
	// not suffixed twice or inconsistently between calls.
	calls := lc.callLog()
	if len(calls) < 2 {
		t.Fatalf("expected at least 2 lifecycle calls, got %v", calls)
	}
	if calls[0] != "EnsureNamespace(e2e-gpu-smoke-pr123,auth=false)" {
		t.Errorf("unexpected first call: %s", calls[0])
	}
	if calls[1] != "EnsureModelDeployment(e2e-gpu-smoke-pr123/gpu-smoke-gemma-pr123)" {
		t.Errorf("unexpected second call: %s", calls[1])
	}
}

// TestSetup_LifecycleOrdering asserts EnsureNamespace runs strictly before
// EnsureModelDeployment, which runs strictly before NamespaceAccess is
// resolved — regardless of which scenario group is doing the Setup. Uses
// the API-key-auth group (which also forces AuthAPIKeyEnabled=true) as
// the representative case.
func TestSetup_LifecycleOrdering(t *testing.T) {
	lc := newFakeLifecycle()
	cfg := APIKeyAuthConfig{
		Namespace:  "e2e-auth",
		Deployment: DeploymentSpec{Name: "auth-gemma", Model: "google/gemma-4-E2B-it", Replicas: 2},
		Lifecycle:  lc,
		Chat:       &fakeChatClient{},
		Logger:     &fakeLogger{},
	}

	if _, err := APIKeyAuthSetup(t.Context(), cfg); err != nil {
		t.Fatalf("APIKeyAuthSetup: %v", err)
	}

	calls := lc.callLog()
	if len(calls) != 3 {
		t.Fatalf("expected exactly 3 lifecycle calls, got %v", calls)
	}
	if calls[0] != "EnsureNamespace(e2e-auth,auth=true)" {
		t.Errorf("call[0] = %q, want EnsureNamespace first (with auth forced true)", calls[0])
	}
	if calls[1] != "EnsureModelDeployment(e2e-auth/auth-gemma)" {
		t.Errorf("call[1] = %q, want EnsureModelDeployment second", calls[1])
	}
	if calls[2] != "NamespaceAccess(e2e-auth)" {
		t.Errorf("call[2] = %q, want NamespaceAccess last", calls[2])
	}
}

func TestSetup_PropagatesEnsureNamespaceError(t *testing.T) {
	lc := newFakeLifecycle()
	lc.ensureNamespaceErr = errString("boom")
	cfg := GPUSmokeConfig{
		Suffix:     Suffix("pr1"),
		Namespace:  "e2e-gpu-smoke",
		Deployment: DeploymentSpec{Name: "gpu-smoke-gemma", Model: "m"},
		Lifecycle:  lc,
		Chat:       &fakeChatClient{},
		Logger:     &fakeLogger{},
	}
	state, err := GPUSmokeSetup(t.Context(), cfg)
	if err == nil {
		t.Fatal("expected error when EnsureNamespace fails, got nil")
	}
	// Must not have gone on to try installing the deployment.
	calls := lc.callLog()
	if len(calls) != 1 {
		t.Fatalf("expected Setup to stop after the failed EnsureNamespace call, got %v", calls)
	}
	// Even on failure, the returned state must carry the resolved
	// physical namespace/deployment identity so diagnostics/cleanup can
	// still locate whatever was partially created.
	res := state.Resources()
	if len(res.Namespaces) != 1 || res.Namespaces[0] != "e2e-gpu-smoke-pr1" {
		t.Errorf("Resources().Namespaces = %v, want physical namespace populated even on failure", res.Namespaces)
	}
	if len(res.Deployments) != 1 || res.Deployments[0].Namespace != "e2e-gpu-smoke-pr1" || res.Deployments[0].Name != "gpu-smoke-gemma-pr1" {
		t.Errorf("Resources().Deployments = %v, want physical deployment identity populated even on failure", res.Deployments)
	}
}

// TestSetup_ReturnsPartialStateAtEveryFailureBoundary is table-driven
// coverage (per group) proving that every Setup function returns a state
// whose Resources() already names the physical namespace/deployment it
// was trying to create, regardless of which lifecycle step failed
// (EnsureNamespace, EnsureModelDeployment, or NamespaceAccess). Without
// this, a failure partway through Setup would leave Cleanup/Diagnostics
// unable to identify (and therefore inspect or eventually reclaim)
// whatever was actually created on the cluster before the failure.
func TestSetup_ReturnsPartialStateAtEveryFailureBoundary(t *testing.T) {
	type failureCase struct {
		name      string
		inject    func(lc *fakeLifecycle)
		wantCalls int
	}
	failureCases := []failureCase{
		{"EnsureNamespace fails", func(lc *fakeLifecycle) { lc.ensureNamespaceErr = errString("ns boom") }, 1},
		{"EnsureModelDeployment fails", func(lc *fakeLifecycle) { lc.ensureDeploymentErr = errString("dep boom") }, 2},
		{"NamespaceAccess fails", func(lc *fakeLifecycle) { lc.namespaceAccessErr = errString("access boom") }, 3},
	}

	t.Run("GPUSmokeSetup", func(t *testing.T) {
		for _, fc := range failureCases {
			t.Run(fc.name, func(t *testing.T) {
				lc := newFakeLifecycle()
				fc.inject(lc)
				cfg := GPUSmokeConfig{
					Suffix:     Suffix("pr1"),
					Namespace:  "e2e-gpu-smoke",
					Deployment: DeploymentSpec{Name: "gpu-smoke-gemma", Model: "m"},
					Lifecycle:  lc,
					Chat:       &fakeChatClient{def: fakeChatRule{status: 200}},
					Logger:     &fakeLogger{},
				}
				state, err := GPUSmokeSetup(context.Background(), cfg)
				if err == nil {
					t.Fatalf("%s: expected error", fc.name)
				}
				res := state.Resources()
				if len(res.Namespaces) != 1 || res.Namespaces[0] != "e2e-gpu-smoke-pr1" {
					t.Errorf("%s: Resources().Namespaces = %v, want [e2e-gpu-smoke-pr1]", fc.name, res.Namespaces)
				}
				if len(res.Deployments) != 1 || res.Deployments[0].Namespace != "e2e-gpu-smoke-pr1" || res.Deployments[0].Name != "gpu-smoke-gemma-pr1" {
					t.Errorf("%s: Resources().Deployments = %v, want physical identity populated", fc.name, res.Deployments)
				}
				if calls := lc.callLog(); len(calls) != fc.wantCalls {
					t.Errorf("%s: lifecycle calls = %v, want %d call(s)", fc.name, calls, fc.wantCalls)
				}
			})
		}
	})

	t.Run("APIKeyAuthSetup", func(t *testing.T) {
		for _, fc := range failureCases {
			t.Run(fc.name, func(t *testing.T) {
				lc := newFakeLifecycle()
				fc.inject(lc)
				cfg := APIKeyAuthConfig{
					Suffix:     Suffix("pr1"),
					Namespace:  "e2e-auth",
					Deployment: DeploymentSpec{Name: "auth-gemma", Model: "m"},
					Lifecycle:  lc,
					Chat:       &fakeChatClient{def: fakeChatRule{status: 200}},
					Logger:     &fakeLogger{},
				}
				state, err := APIKeyAuthSetup(context.Background(), cfg)
				if err == nil {
					t.Fatalf("%s: expected error", fc.name)
				}
				res := state.Resources()
				if len(res.Namespaces) != 1 || res.Namespaces[0] != "e2e-auth-pr1" {
					t.Errorf("%s: Resources().Namespaces = %v, want [e2e-auth-pr1]", fc.name, res.Namespaces)
				}
				if len(res.Deployments) != 1 || res.Deployments[0].Namespace != "e2e-auth-pr1" || res.Deployments[0].Name != "auth-gemma-pr1" {
					t.Errorf("%s: Resources().Deployments = %v, want physical identity populated", fc.name, res.Deployments)
				}
			})
		}
	})

	t.Run("FilterOrderSetup", func(t *testing.T) {
		for _, fc := range failureCases {
			t.Run(fc.name, func(t *testing.T) {
				lc := newFakeLifecycle()
				fc.inject(lc)
				cfg := FilterOrderConfig{
					Suffix:     Suffix("pr1"),
					Namespace:  "e2e-filter-order",
					Deployment: DeploymentSpec{Name: "filter-order-gemma", Model: "m"},
					Lifecycle:  lc,
					Chat:       &fakeChatClient{def: fakeChatRule{status: 200}},
					Kube:       newFakeKubeOps(),
					Logger:     &fakeLogger{},
				}
				state, err := FilterOrderSetup(context.Background(), cfg)
				if err == nil {
					t.Fatalf("%s: expected error", fc.name)
				}
				res := state.Resources()
				if len(res.Namespaces) != 1 || res.Namespaces[0] != "e2e-filter-order-pr1" {
					t.Errorf("%s: Resources().Namespaces = %v, want [e2e-filter-order-pr1]", fc.name, res.Namespaces)
				}
				if len(res.Deployments) != 1 || res.Deployments[0].Namespace != "e2e-filter-order-pr1" || res.Deployments[0].Name != "filter-order-gemma-pr1" {
					t.Errorf("%s: Resources().Deployments = %v, want physical identity populated", fc.name, res.Deployments)
				}
			})
		}
	})

	t.Run("ClusterFilterHASetup", func(t *testing.T) {
		// ClusterFilterHASetup has extra failure boundaries beyond the
		// shared Lifecycle three (BBR Deployment lookup, replica-count
		// validation, wait-for-ready, and the baseline gateway check),
		// so it gets its own boundary list alongside the shared ones.
		type haCase struct {
			name       string
			injectLC   func(lc *fakeLifecycle)
			injectKube func(kube *fakeKubeOps)
			chat       fakeChatRule
			ctxTimeout time.Duration
		}
		haCases := []haCase{
			{name: "EnsureNamespace fails", injectLC: func(lc *fakeLifecycle) { lc.ensureNamespaceErr = errString("boom") }},
			{name: "EnsureModelDeployment fails", injectLC: func(lc *fakeLifecycle) { lc.ensureDeploymentErr = errString("boom") }},
			{name: "NamespaceAccess fails", injectLC: func(lc *fakeLifecycle) { lc.namespaceAccessErr = errString("boom") }},
			{
				name:       "BBR Deployment lookup fails",
				injectKube: func(kube *fakeKubeOps) {}, // no BBR Deployment populated -> GetDeployment errors "not found"
				chat:       fakeChatRule{status: 200},
			},
			{
				name: "BBR replica count below schema minimum",
				injectKube: func(kube *fakeKubeOps) {
					kube.deployments[key("kaito-system", "body-based-router")] = haDeployment(1, true, true)
				},
				chat: fakeChatRule{status: 200},
			},
			{
				name: "BBR never reaches enough ready replicas",
				injectKube: func(kube *fakeKubeOps) {
					kube.deployments[key("kaito-system", "body-based-router")] = haDeployment(2, true, true)
					// readyReplicas intentionally left at 0.
				},
				chat: fakeChatRule{status: 200},
			},
			{
				name: "baseline gateway check never returns 200",
				injectKube: func(kube *fakeKubeOps) {
					kube.deployments[key("kaito-system", "body-based-router")] = haDeployment(2, true, true)
					kube.readyReplicas[key("kaito-system", "body-based-router")] = 2
				},
				chat:       fakeChatRule{status: 500},
				ctxTimeout: 20 * time.Millisecond,
			},
		}

		for _, hc := range haCases {
			t.Run(hc.name, func(t *testing.T) {
				lc := newFakeLifecycle()
				if hc.injectLC != nil {
					hc.injectLC(lc)
				}
				kube := newFakeKubeOps()
				if hc.injectKube != nil {
					hc.injectKube(kube)
				}
				cfg := ClusterFilterHAConfig{
					Suffix:     Suffix("pr1"),
					Namespace:  "e2e-cluster-filter-ha",
					Deployment: DeploymentSpec{Name: "cluster-filter-ha-gemma", Model: "m"},
					Lifecycle:  lc,
					Chat:       &fakeChatClient{def: hc.chat},
					Kube:       kube,
					Logger:     &fakeLogger{},
				}

				ctx := context.Background()
				if hc.ctxTimeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, hc.ctxTimeout)
					defer cancel()
				}

				state, err := ClusterFilterHASetup(ctx, cfg)
				if err == nil {
					t.Fatalf("%s: expected error", hc.name)
				}
				res := state.Resources()
				if len(res.Namespaces) != 1 || res.Namespaces[0] != "e2e-cluster-filter-ha-pr1" {
					t.Errorf("%s: Resources().Namespaces = %v, want [e2e-cluster-filter-ha-pr1]", hc.name, res.Namespaces)
				}
				if len(res.Deployments) != 1 || res.Deployments[0].Namespace != "e2e-cluster-filter-ha-pr1" || res.Deployments[0].Name != "cluster-filter-ha-gemma-pr1" {
					t.Errorf("%s: Resources().Deployments = %v, want physical identity populated", hc.name, res.Deployments)
				}
			})
		}
	})
}

type errString string

func (e errString) Error() string { return string(e) }
