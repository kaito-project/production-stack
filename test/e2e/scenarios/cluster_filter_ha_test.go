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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func haDeployment(replicas int32, withAntiAffinity, withGRPCProbe bool) *appsv1.Deployment {
	d := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "bbr"}},
				},
			},
		},
	}
	if withAntiAffinity {
		d.Spec.Template.Spec.Affinity = &corev1.Affinity{
			PodAntiAffinity: &corev1.PodAntiAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{Weight: 100}},
			},
		}
	}
	if withGRPCProbe {
		d.Spec.Template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				GRPC: &corev1.GRPCAction{Port: 9005},
			},
		}
	}
	return d
}

func TestAssertBBRHighAvailabilityRendered_Pass(t *testing.T) {
	kube := newFakeKubeOps()
	kube.deployments[key("kaito-system", "body-based-router")] = haDeployment(2, true, false)

	state := ClusterFilterHAState{cfg: ClusterFilterHAConfig{Kube: kube}}
	if err := AssertBBRHighAvailabilityRendered(context.Background(), state); err != nil {
		t.Fatalf("expected pass, got %v", err)
	}
}

func TestAssertBBRHighAvailabilityRendered_FailsBelowTwoReplicas(t *testing.T) {
	kube := newFakeKubeOps()
	kube.deployments[key("kaito-system", "body-based-router")] = haDeployment(1, true, false)

	state := ClusterFilterHAState{cfg: ClusterFilterHAConfig{Kube: kube}}
	if err := AssertBBRHighAvailabilityRendered(context.Background(), state); err == nil {
		t.Fatal("expected failure for a single-replica BBR Deployment")
	}
}

func TestAssertBBRHighAvailabilityRendered_FailsWithoutAntiAffinity(t *testing.T) {
	kube := newFakeKubeOps()
	kube.deployments[key("kaito-system", "body-based-router")] = haDeployment(2, false, false)

	state := ClusterFilterHAState{cfg: ClusterFilterHAConfig{Kube: kube}}
	if err := AssertBBRHighAvailabilityRendered(context.Background(), state); err == nil {
		t.Fatal("expected failure when podAntiAffinity is not set")
	}
}

func TestAssertBBRReadinessProbeConfigured_Pass(t *testing.T) {
	kube := newFakeKubeOps()
	kube.deployments[key("kaito-system", "body-based-router")] = haDeployment(2, true, true)

	state := ClusterFilterHAState{cfg: ClusterFilterHAConfig{Kube: kube}}
	if err := AssertBBRReadinessProbeConfigured(context.Background(), state); err != nil {
		t.Fatalf("expected pass, got %v", err)
	}
}

func TestAssertBBRReadinessProbeConfigured_FailsWithoutGRPCProbe(t *testing.T) {
	kube := newFakeKubeOps()
	kube.deployments[key("kaito-system", "body-based-router")] = haDeployment(2, true, false)

	state := ClusterFilterHAState{cfg: ClusterFilterHAConfig{Kube: kube}}
	if err := AssertBBRReadinessProbeConfigured(context.Background(), state); err == nil {
		t.Fatal("expected failure when no gRPC readinessProbe is configured")
	}
}

func TestAssertServesAtSingleReplica_NoFailoverIssues(t *testing.T) {
	kube := newFakeKubeOps()
	kube.readyReplicas[key("kaito-system", "body-based-router")] = 1
	chat := &fakeChatClient{def: fakeChatRule{status: 200}}
	state := ClusterFilterHAState{
		cfg: ClusterFilterHAConfig{
			Kube:          kube,
			Chat:          chat,
			Logger:        &fakeLogger{},
			BurstInterval: time.Millisecond,
		},
		namespace: "e2e-cluster-filter-ha",
		modelName: "cluster-filter-ha-gemma",
		access:    NamespaceAccess{GatewayURL: "http://fake"},
	}

	if err := AssertServesAtSingleReplica(context.Background(), state); err != nil {
		t.Fatalf("expected pass when every request returns 200, got %v", err)
	}
}

func TestAssertServesAtSingleReplica_FailsOn404(t *testing.T) {
	kube := newFakeKubeOps()
	kube.readyReplicas[key("kaito-system", "body-based-router")] = 1
	chat := &fakeChatClient{def: fakeChatRule{status: 404}}
	state := ClusterFilterHAState{
		cfg: ClusterFilterHAConfig{
			Kube:          kube,
			Chat:          chat,
			Logger:        &fakeLogger{},
			BurstInterval: time.Millisecond,
		},
		namespace: "e2e-cluster-filter-ha",
		modelName: "cluster-filter-ha-gemma",
		access:    NamespaceAccess{GatewayURL: "http://fake"},
	}

	if err := AssertServesAtSingleReplica(context.Background(), state); err == nil {
		t.Fatal("expected failure when a single healthy replica 404s (fail-open regression)")
	}
}

func TestAssertFailsClosedWhenAllReplicasDown_RejectsSilent404(t *testing.T) {
	kube := newFakeKubeOps()
	// ScaleDeployment(...,0) will set readyReplicas to 0 for the fake.
	chat := &fakeChatClient{def: fakeChatRule{status: 404}} // silent-404 regression
	state := ClusterFilterHAState{
		cfg: ClusterFilterHAConfig{
			Kube:   kube,
			Chat:   chat,
			Logger: &fakeLogger{},
		},
		namespace: "e2e-cluster-filter-ha",
		modelName: "cluster-filter-ha-gemma",
		access:    NamespaceAccess{GatewayURL: "http://fake"},
	}

	// Bound the wait: the fake never reports >=2 ready after scale-up,
	// so use a context deadline to keep the unit test fast rather than
	// waiting out the assertion's internal 3-minute timeouts.
	ctx, cancel := context.WithTimeout(context.Background(), 50_000_000) // 50ms
	defer cancel()

	if err := AssertFailsClosedWhenAllReplicasDown(ctx, state); err == nil {
		t.Fatal("expected failure: a silent 404 must never be treated as fail-closed")
	}
}

// TestSendChatWithRetry_UsesLifecycleCredentialsWhenPresent guards
// against the (previously real) bug where an AIManager-mode Lifecycle
// that always returns an API key + HostHeader in NamespaceAccess would
// still see BBR-cluster-filter-HA traffic sent with no Authorization
// header — guaranteed 401s against an always-auth backend.
func TestSendChatWithRetry_UsesLifecycleCredentialsWhenPresent(t *testing.T) {
	chat := &fakeChatClient{def: fakeChatRule{status: 200}}
	state := ClusterFilterHAState{
		cfg:       ClusterFilterHAConfig{Chat: chat},
		namespace: "e2e-cluster-filter-ha",
		modelName: "cluster-filter-ha-gemma",
		access: NamespaceAccess{
			GatewayURL:  "http://fake",
			APIKey:      "sk-aimanager-key",
			AuthEnabled: true,
			HostHeader:  "e2e-cluster-filter-ha.gw.example.com",
		},
	}

	resp, err := state.sendChatWithRetry(context.Background())
	if err != nil {
		t.Fatalf("sendChatWithRetry() = %v, want nil", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}

	req := chat.lastRequest()
	if req.BearerToken != "sk-aimanager-key" {
		t.Errorf("request BearerToken = %q, want the lifecycle-provided API key", req.BearerToken)
	}
	if req.HostHeader != "e2e-cluster-filter-ha.gw.example.com" {
		t.Errorf("request HostHeader = %q, want the lifecycle-provided Host header", req.HostHeader)
	}
}

// TestSendChatWithRetry_OmitsCredentialsWhenAbsent asserts the converse:
// a Helm-mode (non-auth) NamespaceAccess with no APIKey/HostHeader must
// not have either field synthesized — the request stays exactly as
// unauthenticated as before this fix.
func TestSendChatWithRetry_OmitsCredentialsWhenAbsent(t *testing.T) {
	chat := &fakeChatClient{def: fakeChatRule{status: 200}}
	state := ClusterFilterHAState{
		cfg:       ClusterFilterHAConfig{Chat: chat},
		namespace: "e2e-cluster-filter-ha",
		modelName: "cluster-filter-ha-gemma",
		access:    NamespaceAccess{GatewayURL: "http://fake"}, // no APIKey/HostHeader
	}

	if _, err := state.sendChatWithRetry(context.Background()); err != nil {
		t.Fatalf("sendChatWithRetry() = %v, want nil", err)
	}

	req := chat.lastRequest()
	if req.BearerToken != "" {
		t.Errorf("request BearerToken = %q, want empty for a non-auth Helm namespace", req.BearerToken)
	}
	if req.HostHeader != "" {
		t.Errorf("request HostHeader = %q, want empty for a non-auth Helm namespace", req.HostHeader)
	}
}

// TestClusterFilterHASetup_CapturesOriginalReplicaCount proves Setup
// discovers the BBR Deployment's OWN desired replica count (here 3, not
// the schema-minimum 2) and stores it for later restore/recovery paths.
func TestClusterFilterHASetup_CapturesOriginalReplicaCount(t *testing.T) {
	kube := newFakeKubeOps()
	kube.deployments[key("kaito-system", "body-based-router")] = haDeployment(3, true, true)
	kube.readyReplicas[key("kaito-system", "body-based-router")] = 3

	cfg := ClusterFilterHAConfig{
		Namespace:  "e2e-cluster-filter-ha",
		Deployment: DeploymentSpec{Name: "cluster-filter-ha-gemma", Model: "m"},
		Lifecycle:  newFakeLifecycle(),
		Chat:       &fakeChatClient{def: fakeChatRule{status: 200}},
		Kube:       kube,
		Logger:     &fakeLogger{},
	}

	state, err := ClusterFilterHASetup(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ClusterFilterHASetup: %v", err)
	}
	if state.bbrTargetReplicas != 3 {
		t.Fatalf("bbrTargetReplicas = %d, want 3 (captured from the Deployment's own spec.replicas)", state.bbrTargetReplicas)
	}
	if got := state.bbrTarget(); got != 3 {
		t.Errorf("bbrTarget() = %d, want 3", got)
	}
}

// TestRestorePaths_UseCapturedReplicaCountOfThree proves every
// restore/recovery path (AssertRecoversToFullReplicaCount,
// AssertFailsClosedWhenAllReplicasDown, TeardownBBR) scales BBR back to
// a captured original count of 3 rather than a hardcoded 2.
func TestRestorePaths_UseCapturedReplicaCountOfThree(t *testing.T) {
	newState := func(kube *fakeKubeOps, chat *fakeChatClient) ClusterFilterHAState {
		return ClusterFilterHAState{
			cfg: ClusterFilterHAConfig{
				Kube:   kube,
				Chat:   chat,
				Logger: &fakeLogger{},
			},
			namespace:         "e2e-cluster-filter-ha",
			modelName:         "cluster-filter-ha-gemma",
			access:            NamespaceAccess{GatewayURL: "http://fake"},
			bbrTargetReplicas: 3,
		}
	}

	t.Run("AssertRecoversToFullReplicaCount", func(t *testing.T) {
		kube := newFakeKubeOps()
		state := newState(kube, &fakeChatClient{def: fakeChatRule{status: 200}})

		if err := AssertRecoversToFullReplicaCount(context.Background(), state); err != nil {
			t.Fatalf("AssertRecoversToFullReplicaCount: %v", err)
		}
		if got, want := kube.lastScaleCall(), "kaito-system/body-based-router=3"; got != want {
			t.Errorf("last scale call = %q, want %q", got, want)
		}
	})

	t.Run("AssertFailsClosedWhenAllReplicasDown", func(t *testing.T) {
		kube := newFakeKubeOps()
		// 502 first (detected as fail-closed), then 200 (final recovery check).
		chat := &fakeChatClient{rules: []fakeChatRule{{status: 502}, {status: 200}}}
		state := newState(kube, chat)

		if err := AssertFailsClosedWhenAllReplicasDown(context.Background(), state); err != nil {
			t.Fatalf("AssertFailsClosedWhenAllReplicasDown: %v", err)
		}
		if got, want := kube.lastScaleCall(), "kaito-system/body-based-router=3"; got != want {
			t.Errorf("last scale call = %q, want %q (restore must use the captured count, not hardcoded 2)", got, want)
		}
	})

	t.Run("TeardownBBR", func(t *testing.T) {
		kube := newFakeKubeOps()
		state := newState(kube, &fakeChatClient{})

		TeardownBBR(context.Background(), state)
		if got, want := kube.lastScaleCall(), "kaito-system/body-based-router=3"; got != want {
			t.Errorf("last scale call = %q, want %q", got, want)
		}
	})

	t.Run("TeardownBBR falls back to 2 when the count was never captured", func(t *testing.T) {
		kube := newFakeKubeOps()
		state := ClusterFilterHAState{
			cfg: ClusterFilterHAConfig{
				Kube:   kube,
				Chat:   &fakeChatClient{},
				Logger: &fakeLogger{},
			},
			namespace: "e2e-cluster-filter-ha",
			modelName: "cluster-filter-ha-gemma",
			// bbrTargetReplicas intentionally left zero (Setup never
			// reached that step, e.g. it failed earlier).
		}

		TeardownBBR(context.Background(), state)
		if got, want := kube.lastScaleCall(), "kaito-system/body-based-router=2"; got != want {
			t.Errorf("last scale call = %q, want %q (fallback to schema minimum)", got, want)
		}
	})
}
