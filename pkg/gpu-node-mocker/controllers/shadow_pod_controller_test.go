// Copyright (c) KAITO authors.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestIsPendingOnFakeNode(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"valid", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"inferenceset.kaito.sh/created-by": "falcon"}},
			Spec:       corev1.PodSpec{NodeName: "fake-ws1"}, Status: corev1.PodStatus{Phase: corev1.PodPending},
		}, true},
		{"running", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"inferenceset.kaito.sh/created-by": "falcon"}},
			Spec:       corev1.PodSpec{NodeName: "fake-ws1"}, Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}, false},
		{"real node", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"inferenceset.kaito.sh/created-by": "falcon"}},
			Spec:       corev1.PodSpec{NodeName: "aks-node1"}, Status: corev1.PodStatus{Phase: corev1.PodPending},
		}, false},
		{"no kaito label", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "nginx"}},
			Spec:       corev1.PodSpec{NodeName: "fake-ws1"}, Status: corev1.PodStatus{Phase: corev1.PodPending},
		}, false},
		{"no node", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"inferenceset.kaito.sh/created-by": "falcon"}},
			Status:     corev1.PodStatus{Phase: corev1.PodPending},
		}, false},
		{"nil labels", &corev1.Pod{
			Spec: corev1.PodSpec{NodeName: "fake-ws1"}, Status: corev1.PodStatus{Phase: corev1.PodPending},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPendingOnFakeNode(tt.pod); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsShadowPod(t *testing.T) {
	if !isShadowPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{ShadowPodLabelKey: "x"}}}) {
		t.Error("should detect shadow pod")
	}
	if isShadowPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "x"}}}) {
		t.Error("should not detect non-shadow pod")
	}
	if isShadowPod(&corev1.Pod{}) {
		t.Error("should not detect pod with no labels")
	}
}

func TestShadowPodName(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "falcon-0", Namespace: "default"}}
	if got := shadowPodName(pod); got != "shadow-default-falcon-0" {
		t.Errorf("got %q", got)
	}
}

func TestShadowPodNameTruncation(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: string(make([]byte, 250)), Namespace: "default"}}
	if len(shadowPodName(pod)) > 253 {
		t.Error("name too long")
	}
}

func TestMakePodCondition(t *testing.T) {
	now := metav1.Now()
	c := makePodCondition(corev1.PodReady, corev1.ConditionTrue, "R", "m", now)
	if c.Type != corev1.PodReady || c.Status != corev1.ConditionTrue {
		t.Errorf("unexpected: %+v", c)
	}
}

// ── Reconciler tests ────────────────────────────────────────────────────────

func newPendingPodOnFakeNode(name, ns, nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				"inferenceset.kaito.sh/created-by": "falcon-7b-instruct",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{
					Name:  "model",
					Image: "kaito/falcon:latest",
					Args:  []string{"--model", "tiiuae/falcon-7b-instruct", "--port", "5000"},
					Ports: []corev1.ContainerPort{{ContainerPort: 5000}},
				},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

func TestEnsureShadowPod_Creates(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	cfg := testConfig()

	// Create the shadow namespace
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.ShadowPodNamespace}}
	original := newPendingPodOnFakeNode("falcon-0", "default", "fake-ws1")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, original).Build()
	r := &ShadowPodReconciler{Client: cl, Config: cfg}

	shadow, err := r.ensureShadowPod(ctx, original, "shadow-default-falcon-0")
	if err != nil {
		t.Fatalf("ensureShadowPod: %v", err)
	}
	if shadow.Name != "shadow-default-falcon-0" {
		t.Errorf("name = %q", shadow.Name)
	}
	if shadow.Namespace != cfg.ShadowPodNamespace {
		t.Errorf("namespace = %q", shadow.Namespace)
	}
	// Main container should be llm-d-inference-sim
	if len(shadow.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(shadow.Spec.Containers))
	}
	mainContainer := shadow.Spec.Containers[0]
	if mainContainer.Name != "llm-d-inference-sim" {
		t.Errorf("container name = %q", mainContainer.Name)
	}
	if mainContainer.Image != cfg.ShadowPodImage {
		t.Errorf("image = %q", mainContainer.Image)
	}
	// Should use --config /config/config.yaml args
	if len(mainContainer.Args) != 2 || mainContainer.Args[0] != "--config" || mainContainer.Args[1] != "/config/config.yaml" {
		t.Errorf("args = %v", mainContainer.Args)
	}
	// Main container should use port from original pod (5000)
	if len(mainContainer.Ports) != 1 || mainContainer.Ports[0].ContainerPort != 5000 {
		t.Errorf("ports = %v, want containerPort=5000 (from original pod)", mainContainer.Ports)
	}
	// Should have readiness probe at /ready
	if mainContainer.ReadinessProbe == nil || mainContainer.ReadinessProbe.HTTPGet.Path != "/ready" {
		t.Error("missing or wrong readiness probe")
	}
	// Should have liveness probe at /health
	if mainContainer.LivenessProbe == nil || mainContainer.LivenessProbe.HTTPGet.Path != "/health" {
		t.Error("missing or wrong liveness probe")
	}
	// Should have config and uds-socket volume mounts
	if len(mainContainer.VolumeMounts) != 2 {
		t.Fatalf("expected 2 volume mounts, got %d", len(mainContainer.VolumeMounts))
	}

	// Init container should be uds-tokenizer (native sidecar)
	if len(shadow.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(shadow.Spec.InitContainers))
	}
	udsContainer := shadow.Spec.InitContainers[0]
	if udsContainer.Name != "uds-tokenizer" {
		t.Errorf("init container name = %q", udsContainer.Name)
	}
	if udsContainer.Image != DefaultUDSTokenizerImage {
		t.Errorf("uds tokenizer image = %q", udsContainer.Image)
	}
	if udsContainer.RestartPolicy == nil || *udsContainer.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("uds-tokenizer should have restartPolicy=Always (native sidecar)")
	}

	// Should have 2 volumes: config (ConfigMap) + uds-socket (emptyDir)
	if len(shadow.Spec.Volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(shadow.Spec.Volumes))
	}

	if shadow.Labels[LabelManagedBy] != ControllerName {
		t.Error("missing managed-by label")
	}
	if shadow.Labels[ShadowPodLabelKey] != "default.falcon-0" {
		t.Errorf("shadow label = %q", shadow.Labels[ShadowPodLabelKey])
	}
	// Should have anti-affinity to exclude fake nodes
	affinity := shadow.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil ||
		affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatal("missing node anti-affinity")
	}
	terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchExpressions) != 1 {
		t.Fatalf("unexpected terms: %+v", terms)
	}
	expr := terms[0].MatchExpressions[0]
	if expr.Key != LabelFakeNode || expr.Operator != corev1.NodeSelectorOpDoesNotExist {
		t.Errorf("anti-affinity expr = %+v", expr)
	}
	// Should NOT have ServiceAccountName
	if shadow.Spec.ServiceAccountName != "" {
		t.Errorf("ServiceAccountName should be empty, got %q", shadow.Spec.ServiceAccountName)
	}

	// Verify ConfigMap was created
	cm := &corev1.ConfigMap{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "shadow-default-falcon-0-config", Namespace: cfg.ShadowPodNamespace}, cm); err != nil {
		t.Fatalf("configmap not created: %v", err)
	}
	configYAML := cm.Data["config.yaml"]
	if !strings.Contains(configYAML, "port: 5000") {
		t.Errorf("config.yaml should use port 5000 from original pod, got: %s", configYAML)
	}
	if !strings.Contains(configYAML, "tiiuae/falcon-7b-instruct") {
		t.Errorf("config.yaml should contain model name, got: %s", configYAML)
	}
	if !strings.Contains(configYAML, "enable-kvcache: true") {
		t.Errorf("config.yaml should enable kv cache, got: %s", configYAML)
	}
	// Should NOT contain threshold
	if strings.Contains(configYAML, "threshold") {
		t.Errorf("config.yaml should not contain threshold, got: %s", configYAML)
	}
}

func TestEnsureShadowPod_ReturnsExisting(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	cfg := testConfig()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.ShadowPodNamespace}}
	existing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shadow-default-falcon-0",
			Namespace: cfg.ShadowPodNamespace,
			Labels:    map[string]string{"existing": "true"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i"}}},
	}
	original := newPendingPodOnFakeNode("falcon-0", "default", "fake-ws1")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, existing, original).Build()
	r := &ShadowPodReconciler{Client: cl, Config: cfg}

	shadow, err := r.ensureShadowPod(ctx, original, "shadow-default-falcon-0")
	if err != nil {
		t.Fatalf("ensureShadowPod: %v", err)
	}
	if shadow.Labels["existing"] != "true" {
		t.Error("should return existing shadow pod")
	}
}

func TestPatchOriginalPodStatus(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	original := newPendingPodOnFakeNode("falcon-0", "default", "fake-ws1")
	shadow := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "shadow-default-falcon-0", Namespace: "kaito-shadow"},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIP:  "10.244.1.100",
			HostIP: "10.0.0.5",
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "model", ImageID: "sha256:abc123"},
			},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(original).WithStatusSubresource(original).Build()
	r := &ShadowPodReconciler{Client: cl, Config: testConfig()}

	if err := r.patchOriginalPodStatus(ctx, original, shadow); err != nil {
		t.Fatalf("patchOriginalPodStatus: %v", err)
	}

	updated := &corev1.Pod{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "falcon-0", Namespace: "default"}, updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != corev1.PodRunning {
		t.Errorf("phase = %v", updated.Status.Phase)
	}
	if updated.Status.PodIP != "10.244.1.100" {
		t.Errorf("podIP = %q", updated.Status.PodIP)
	}
	if updated.Status.HostIP != "10.0.0.5" {
		t.Errorf("hostIP = %q", updated.Status.HostIP)
	}
	if len(updated.Status.PodIPs) != 1 || updated.Status.PodIPs[0].IP != "10.244.1.100" {
		t.Errorf("podIPs = %v", updated.Status.PodIPs)
	}
	// Check conditions
	readyFound := false
	for _, c := range updated.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			readyFound = true
		}
	}
	if !readyFound {
		t.Error("pod should have Ready=True condition")
	}
	// Check container statuses
	if len(updated.Status.ContainerStatuses) != 1 {
		t.Fatalf("containerStatuses len = %d", len(updated.Status.ContainerStatuses))
	}
	cs := updated.Status.ContainerStatuses[0]
	if !cs.Ready || cs.State.Running == nil {
		t.Errorf("container status: ready=%v, running=%v", cs.Ready, cs.State.Running)
	}
}

func TestShadowPodToOriginalPod(t *testing.T) {
	r := &ShadowPodReconciler{}

	t.Run("valid shadow pod", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{ShadowPodLabelKey: "default.falcon-0"},
			},
		}
		reqs := r.shadowPodToOriginalPod(context.Background(), pod)
		if len(reqs) != 1 {
			t.Fatalf("got %d requests, want 1", len(reqs))
		}
		if reqs[0].NamespacedName != (types.NamespacedName{Namespace: "default", Name: "falcon-0"}) {
			t.Errorf("got %v", reqs[0].NamespacedName)
		}
	})

	t.Run("no shadow label", func(t *testing.T) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "x"}}}
		if reqs := r.shadowPodToOriginalPod(context.Background(), pod); len(reqs) != 0 {
			t.Errorf("expected 0 requests, got %d", len(reqs))
		}
	})

	t.Run("invalid label format", func(t *testing.T) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{ShadowPodLabelKey: "noseparator"}}}
		if reqs := r.shadowPodToOriginalPod(context.Background(), pod); len(reqs) != 0 {
			t.Errorf("expected 0 requests for invalid format, got %d", len(reqs))
		}
	})
}

func TestShadowPodReconcile_NotFound(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ShadowPodReconciler{Client: cl, Config: testConfig()}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "gone", Namespace: "default"}})
	if err != nil {
		t.Fatalf("should not error: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue")
	}
}

func TestShadowPodReconcile_SkipsRunningPod(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	pod := newPendingPodOnFakeNode("falcon-0", "default", "fake-ws1")
	pod.Status.Phase = corev1.PodRunning

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := &ShadowPodReconciler{Client: cl, Config: testConfig()}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "falcon-0", Namespace: "default"}})
	if err != nil {
		t.Fatalf("should not error: %v", err)
	}
	if result.Requeue {
		t.Error("should not requeue for running pod")
	}
}

func TestShadowPodReconcile_SkipsRealNode(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	pod := newPendingPodOnFakeNode("falcon-0", "default", "aks-real-node")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := &ShadowPodReconciler{Client: cl, Config: testConfig()}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "falcon-0", Namespace: "default"}})
	if err != nil {
		t.Fatalf("should not error: %v", err)
	}
	_ = result
}

func TestShadowPodReconcile_CreatesShadowAndRequeues(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	cfg := testConfig()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.ShadowPodNamespace}}
	original := newPendingPodOnFakeNode("falcon-0", "default", "fake-ws1")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, original).WithStatusSubresource(original).Build()
	r := &ShadowPodReconciler{Client: cl, Config: cfg}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "falcon-0", Namespace: "default"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// Shadow pod is Pending (no kubelet in test), so should requeue
	if result.RequeueAfter == 0 {
		t.Error("should requeue while waiting for shadow pod to be Running")
	}

	// Verify shadow pod was created
	shadow := &corev1.Pod{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "shadow-default-falcon-0", Namespace: cfg.ShadowPodNamespace}, shadow); err != nil {
		t.Fatalf("shadow pod not created: %v", err)
	}

	// Verify annotation was set on original pod
	updated := &corev1.Pod{}
	_ = cl.Get(ctx, types.NamespacedName{Name: "falcon-0", Namespace: "default"}, updated)
	if updated.Annotations[AnnotationShadowPodRef] != cfg.ShadowPodNamespace+"/shadow-default-falcon-0" {
		t.Errorf("annotation = %q", updated.Annotations[AnnotationShadowPodRef])
	}
}

func TestShadowPodReconcile_PatchesWhenShadowRunning(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	cfg := testConfig()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.ShadowPodNamespace}}
	original := newPendingPodOnFakeNode("falcon-0", "default", "fake-ws1")

	// Pre-create a Running shadow pod
	shadow := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shadow-default-falcon-0",
			Namespace: cfg.ShadowPodNamespace,
			Labels:    map[string]string{ShadowPodLabelKey: "default.falcon-0"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "i"}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			PodIP:             "10.244.1.50",
			HostIP:            "10.0.0.5",
			ContainerStatuses: []corev1.ContainerStatus{{Name: "model", ImageID: "sha256:abc"}},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, original, shadow).WithStatusSubresource(original).Build()
	r := &ShadowPodReconciler{Client: cl, Config: cfg}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "falcon-0", Namespace: "default"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("should not requeue when shadow pod is Running")
	}

	// Verify original pod was patched to Running
	updated := &corev1.Pod{}
	_ = cl.Get(ctx, types.NamespacedName{Name: "falcon-0", Namespace: "default"}, updated)
	if updated.Status.Phase != corev1.PodRunning {
		t.Errorf("phase = %v, want Running", updated.Status.Phase)
	}
	if updated.Status.PodIP != "10.244.1.50" {
		t.Errorf("podIP = %q", updated.Status.PodIP)
	}
}

// Test that shadowPodToOriginalPod returns empty for non-pod objects
func TestShadowPodToOriginalPod_NonPod(t *testing.T) {
	r := &ShadowPodReconciler{}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "not-a-pod"}}
	reqs := r.shadowPodToOriginalPod(context.Background(), node)
	if len(reqs) != 0 {
		t.Errorf("expected 0, got %d", len(reqs))
	}
}

// Verify that the reconciler returns empty for a non-Pod object via the mapper
func TestShadowPodToOriginalPod_EmptyForNonPod(t *testing.T) {
	r := &ShadowPodReconciler{}
	reqs := r.shadowPodToOriginalPod(context.Background(), &corev1.Node{})
	expected := []reconcile.Request(nil)
	if len(reqs) != len(expected) {
		t.Errorf("len = %d", len(reqs))
	}
}

func TestPatchOriginalPodStatus_MultipleContainers(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	// Original pod has 2 containers; shadow pod only has 1.
	original := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "multi-0",
			Namespace: "default",
			Labels:    map[string]string{"inferenceset.kaito.sh/created-by": "falcon"},
		},
		Spec: corev1.PodSpec{
			NodeName: "fake-ws1",
			Containers: []corev1.Container{
				{Name: "model", Image: "kaito/falcon:latest", Ports: []corev1.ContainerPort{{ContainerPort: 5000}}},
				{Name: "sidecar", Image: "kaito/sidecar:latest"},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	shadow := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "shadow-default-multi-0", Namespace: "kaito-shadow"},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIP:  "10.244.1.200",
			HostIP: "10.0.0.5",
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "llm-mocker", ImageID: "sha256:mocker123"},
			},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(original).WithStatusSubresource(original).Build()
	r := &ShadowPodReconciler{Client: cl, Config: testConfig()}

	if err := r.patchOriginalPodStatus(ctx, original, shadow); err != nil {
		t.Fatalf("patchOriginalPodStatus: %v", err)
	}

	updated := &corev1.Pod{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "multi-0", Namespace: "default"}, updated); err != nil {
		t.Fatal(err)
	}
	// Should have 2 container statuses matching the 2 original containers.
	if len(updated.Status.ContainerStatuses) != 2 {
		t.Fatalf("containerStatuses len = %d, want 2", len(updated.Status.ContainerStatuses))
	}
	// First container should use the shadow's imageID.
	if updated.Status.ContainerStatuses[0].ImageID != "sha256:mocker123" {
		t.Errorf("first container imageID = %q", updated.Status.ContainerStatuses[0].ImageID)
	}
	// Second container should fall back to the original image name.
	if updated.Status.ContainerStatuses[1].ImageID != "kaito/sidecar:latest" {
		t.Errorf("second container imageID = %q, want fallback to original image", updated.Status.ContainerStatuses[1].ImageID)
	}
	// Both should be Ready.
	for i, cs := range updated.Status.ContainerStatuses {
		if !cs.Ready || cs.State.Running == nil {
			t.Errorf("container %d: ready=%v, running=%v", i, cs.Ready, cs.State.Running)
		}
	}
}

func TestEnsureShadowPod_MultiplePorts(t *testing.T) {
	// Multiple ports test is no longer relevant since the inference sim always
	// uses a fixed port (InferenceSimPort). Verify it uses the correct port.
	ctx := context.Background()
	scheme := testScheme()
	cfg := testConfig()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.ShadowPodNamespace}}
	original := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "multi-port-0",
			Namespace: "default",
			Labels:    map[string]string{"inferenceset.kaito.sh/created-by": "falcon"},
		},
		Spec: corev1.PodSpec{
			NodeName: "fake-ws1",
			Containers: []corev1.Container{
				{Name: "model", Image: "kaito/falcon:latest", Args: []string{"--model", "falcon-7b"}, Ports: []corev1.ContainerPort{
					{ContainerPort: 8080, Name: "http"},
					{ContainerPort: 9090, Name: "metrics"},
				}},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, original).Build()
	r := &ShadowPodReconciler{Client: cl, Config: cfg}

	shadow, err := r.ensureShadowPod(ctx, original, "shadow-default-multi-port-0")
	if err != nil {
		t.Fatalf("ensureShadowPod: %v", err)
	}
	// Shadow pod should use the original pod's first port (8080)
	if len(shadow.Spec.Containers[0].Ports) != 1 || shadow.Spec.Containers[0].Ports[0].ContainerPort != 8080 {
		t.Errorf("expected port 8080 from original pod, got %v", shadow.Spec.Containers[0].Ports)
	}
}

func TestEnsureShadowPod_DefaultProbePort(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	cfg := testConfig()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.ShadowPodNamespace}}
	original := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-port-0",
			Namespace: "default",
			Labels:    map[string]string{"inferenceset.kaito.sh/created-by": "falcon"},
		},
		Spec: corev1.PodSpec{
			NodeName:   "fake-ws1",
			Containers: []corev1.Container{{Name: "model", Image: "kaito/falcon:latest", Args: []string{"--model=falcon-7b"}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, original).Build()
	r := &ShadowPodReconciler{Client: cl, Config: cfg}

	shadow, err := r.ensureShadowPod(ctx, original, "shadow-default-no-port-0")
	if err != nil {
		t.Fatalf("ensureShadowPod: %v", err)
	}
	// Readiness probe should use named port "http" which maps to InferenceSimPort
	probe := shadow.Spec.Containers[0].ReadinessProbe
	if probe == nil || probe.HTTPGet.Port.StrVal != "http" {
		t.Errorf("readiness probe port = %v, want named port 'http'", probe)
	}
}

func TestShadowPodReconcile_SkipsNonKaitoPod(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	// Pod on a fake node but without the KAITO label — should be skipped.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "non-kaito-0",
			Namespace: "default",
			Labels:    map[string]string{"app": "nginx"},
		},
		Spec:   corev1.PodSpec{NodeName: "fake-ws1"},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	r := &ShadowPodReconciler{Client: cl, Config: testConfig()}

	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "non-kaito-0", Namespace: "default"}})
	if err != nil {
		t.Fatalf("should not error: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Error("should not requeue for non-KAITO pod")
	}
}

func TestExtractModelName(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{"from args --model value", &corev1.Pod{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Args: []string{"--model", "meta-llama/Llama-3.1-8B-Instruct"}},
			}},
		}, "meta-llama/Llama-3.1-8B-Instruct"},
		{"from args --model=value", &corev1.Pod{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Args: []string{"--model=tiiuae/falcon-7b-instruct"}},
			}},
		}, "tiiuae/falcon-7b-instruct"},
		{"from command", &corev1.Pod{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Command: []string{"vllm", "serve", "--model", "Qwen/Qwen2-0.5B"}},
			}},
		}, "Qwen/Qwen2-0.5B"},
		{"from --served-model-name", &corev1.Pod{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Args: []string{"--served-model-name", "my-custom-model"}},
			}},
		}, "my-custom-model"},
		{"from --served-model-name=value", &corev1.Pod{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Args: []string{"--served-model-name=gpt-4o-mini"}},
			}},
		}, "gpt-4o-mini"},
		{"--model takes precedence over --served-model-name", &corev1.Pod{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Args: []string{"--model", "real-model", "--served-model-name", "alias"}},
			}},
		}, "real-model"},
		{"fallback default", &corev1.Pod{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Args: []string{"--port", "8000"}},
			}},
		}, DefaultModelName},
		{"no containers", &corev1.Pod{}, DefaultModelName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractModelName(tt.pod); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractServingPort(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want int32
	}{
		{"from first container port", &corev1.Pod{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Ports: []corev1.ContainerPort{{ContainerPort: 5000}}},
			}},
		}, 5000},
		{"from vLLM default 8000", &corev1.Pod{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Ports: []corev1.ContainerPort{{ContainerPort: 8000}}},
			}},
		}, 8000},
		{"no ports fallback", &corev1.Pod{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "model"},
			}},
		}, InferenceSimPort},
		{"empty pod", &corev1.Pod{}, InferenceSimPort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractServingPort(tt.pod); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEnsureSimConfigMap(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	cfg := testConfig()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.ShadowPodNamespace}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns).Build()
	r := &ShadowPodReconciler{Client: cl, Config: cfg}

	err := r.ensureSimConfigMap(ctx, "shadow-default-falcon-0", "tiiuae/falcon-7b", 5000)
	if err != nil {
		t.Fatalf("ensureSimConfigMap: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "shadow-default-falcon-0-config", Namespace: cfg.ShadowPodNamespace}, cm); err != nil {
		t.Fatalf("configmap not found: %v", err)
	}

	configYAML := cm.Data["config.yaml"]
	if !strings.Contains(configYAML, "port: 5000") {
		t.Errorf("config should use port 5000, got: %s", configYAML)
	}
	if !strings.Contains(configYAML, `model: "tiiuae/falcon-7b"`) {
		t.Errorf("missing model in config: %s", configYAML)
	}
	if !strings.Contains(configYAML, "enable-kvcache: true") {
		t.Errorf("missing enable-kvcache in config: %s", configYAML)
	}
	if strings.Contains(configYAML, "threshold") {
		t.Errorf("config should not contain threshold: %s", configYAML)
	}

	// Idempotent: calling again should not error
	if err := r.ensureSimConfigMap(ctx, "shadow-default-falcon-0", "tiiuae/falcon-7b", 5000); err != nil {
		t.Fatalf("second call should be idempotent: %v", err)
	}
}

func TestEnsureNamespace_CreatesWhenMissing(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ShadowPodReconciler{Client: cl, Config: testConfig()}

	if err := r.ensureNamespace(ctx, "kaito-shadow"); err != nil {
		t.Fatalf("ensureNamespace: %v", err)
	}

	// Namespace should now exist.
	ns := &corev1.Namespace{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "kaito-shadow"}, ns); err != nil {
		t.Fatalf("namespace should exist: %v", err)
	}
	if ns.Labels[LabelManagedBy] != ControllerName {
		t.Errorf("label = %q, want %q", ns.Labels[LabelManagedBy], ControllerName)
	}
}

func TestEnsureNamespace_SkipsWhenExists(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	existing := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "kaito-shadow",
			Labels: map[string]string{"existing": "true"},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	r := &ShadowPodReconciler{Client: cl, Config: testConfig()}

	if err := r.ensureNamespace(ctx, "kaito-shadow"); err != nil {
		t.Fatalf("ensureNamespace: %v", err)
	}

	// Should not overwrite existing namespace.
	ns := &corev1.Namespace{}
	_ = cl.Get(ctx, types.NamespacedName{Name: "kaito-shadow"}, ns)
	if ns.Labels["existing"] != "true" {
		t.Error("should not recreate existing namespace")
	}
}

func TestEnsureNamespace_Idempotent(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ShadowPodReconciler{Client: cl, Config: testConfig()}

	// Call twice — second call should not error.
	if err := r.ensureNamespace(ctx, "kaito-shadow"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := r.ensureNamespace(ctx, "kaito-shadow"); err != nil {
		t.Fatalf("second call should be idempotent: %v", err)
	}
}
