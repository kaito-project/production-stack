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

package controllers

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ShadowPodReconciler implements Phase 2 of the Shadow Pod lifecycle.
//
// It watches Pods in all namespaces and acts when a pod is:
//   - Assigned to a fake node (spec.nodeName starts with "fake-")
//   - Still in Pending phase (no kubelet will ever run it)
//
// For each such pod the reconciler:
//  1. Creates a "shadow pod" in the same namespace as the original pod on a real AKS node.
//     The shadow pod runs the LLM Mocker container and gets a real CNI IP.
//  2. Waits until the shadow pod is Running and has a podIP.
//  3. Patches the original pending pod's STATUS (not spec) with:
//     - phase = Running
//     - podIP / podIPs = shadow pod's real IP
//     - conditions[Ready] = True
//     - containerStatuses[*].ready = true, state.running
//
// From KAITO's perspective the original pod is Running/Ready → InferenceReady
// flips to True. Traffic routed by the Gateway/EPP to the pod IP hits the
// real shadow pod and is served by the LLM Mocker.
type ShadowPodReconciler struct {
	client.Client
	Config Config
}

// SetupWithManager registers the controller with two watches:
//
//  1. Primary watch on Pods (all namespaces) filtered to Pending pods on fake
//     nodes — these are the "original" pods we need to mirror.
//  2. Secondary watch on shadow pods — when a shadow pod transitions to Running
//     we re-queue the original pod so we can apply the status patch immediately.
func (r *ShadowPodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Predicate: only enqueue pods that are Pending AND assigned to a fake node.
	pendingOnFakeNode := predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return isPendingOnFakeNode(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return isPendingOnFakeNode(e.ObjectNew) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return false },
	}

	// Predicate: shadow pods — only care about Running transitions.
	shadowPodRunning := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			pod, ok := e.ObjectNew.(*corev1.Pod)
			return ok && isShadowPod(pod) && pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != ""
		},
		DeleteFunc:  func(e event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return false },
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}, builder.WithPredicates(pendingOnFakeNode)).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.shadowPodToOriginalPod),
			builder.WithPredicates(shadowPodRunning),
		).
		Complete(r)
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=get;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create

func (r *ShadowPodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithValues("pod", req.NamespacedName)

	original := &corev1.Pod{}
	if err := r.Get(ctx, req.NamespacedName, original); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get pod: %w", err)
	}

	if !isPendingOnFakeNode(original) {
		return ctrl.Result{}, nil
	}
	if original.Status.Phase == corev1.PodRunning {
		return ctrl.Result{}, nil
	}

	log.Info("processing pending pod on fake node", "node", original.Spec.NodeName)

	shadowName := shadowPodName(original)
	shadowPod, err := r.ensureShadowPod(ctx, original, shadowName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure shadow pod: %w", err)
	}

	// Annotate the original pod with the shadow pod reference so future
	// reconciles can correlate them without re-computing the name.
	if original.Annotations[AnnotationShadowPodRef] == "" {
		patch := client.MergeFrom(original.DeepCopy())
		if original.Annotations == nil {
			original.Annotations = map[string]string{}
		}
		original.Annotations[AnnotationShadowPodRef] = original.Namespace + "/" + shadowName
		if pErr := r.Patch(ctx, original, patch); pErr != nil {
			log.Error(pErr, "failed to annotate original pod with shadow ref")
		}
	}

	if shadowPod.Status.Phase != corev1.PodRunning || shadowPod.Status.PodIP == "" {
		log.Info("shadow pod not yet Running — will retry", "shadowPod", shadowName, "phase", shadowPod.Status.Phase)
		// The secondary watch will re-trigger us; RequeueAfter is a safety net.
		return ctrl.Result{RequeueAfter: 5_000_000_000}, nil // 5 s
	}

	if err := r.patchOriginalPodStatus(ctx, original, shadowPod); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch original pod status: %w", err)
	}

	log.Info("original pod patched to Running", "podIP", shadowPod.Status.PodIP)
	return ctrl.Result{}, nil
}

// ensureShadowPod creates the shadow pod if it does not yet exist, or returns
// the existing one.
//
// The shadow pod runs the llm-d inference simulator (ghcr.io/llm-d/llm-d-inference-sim)
// with a UDS tokenizer sidecar, matching the manifest structure from the
// llm-d-inference-sim helm chart:
//   - Init sidecar: uds-tokenizer (native sidecar with restartPolicy=Always)
//   - Main container: llm-d-inference-sim with --config /config/config.yaml
//   - ConfigMap volume for config.yaml + emptyDir for UDS socket
//   - Node anti-affinity to avoid fake nodes
//   - Model name extracted from the original pod's args/command
//   - KV cache enabled, no threshold set
func (r *ShadowPodReconciler) ensureShadowPod(ctx context.Context, original *corev1.Pod, shadowName string) (*corev1.Pod, error) {
	// Create the shadow pod in the same namespace as the original pod so that
	// Kubernetes NetworkPolicies applied to the model namespace also govern
	// the shadow pod's ingress/egress traffic.
	shadowNS := original.Namespace

	existing := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Namespace: shadowNS, Name: shadowName}, existing)
	if err == nil {
		return existing, nil
	}
	if !errors.IsNotFound(err) {
		return nil, fmt.Errorf("get shadow pod: %w", err)
	}

	modelName := extractModelName(original)
	servedModelName := extractServedModelName(original, modelName)
	servingPort := extractServingPort(original)

	// Tie shadow-pod and its ConfigMap lifecycle to the original pod via
	// OwnerReferences. When KAITO/InferenceSet deletes the original pod,
	// the K8s garbage collector cascades the delete to the shadow pod and
	// the ConfigMap, eliminating the need for a custom cleanup loop.
	// Same-namespace requirement is satisfied: ensureShadowPod uses
	// shadowNS = original.Namespace.
	ownerRef := metav1.OwnerReference{
		APIVersion:         "v1",
		Kind:               "Pod",
		Name:               original.Name,
		UID:                original.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(false),
	}

	// Ensure the ConfigMap for the inference simulator exists.
	if err := r.ensureSimConfigMap(ctx, shadowNS, shadowName, modelName, servedModelName, servingPort, ownerRef); err != nil {
		return nil, fmt.Errorf("ensure sim configmap: %w", err)
	}

	labels := map[string]string{
		LabelManagedBy:    ControllerName,
		ShadowPodLabelKey: original.Namespace + "." + original.Name,
	}
	if v, ok := original.Labels[InferenceSetCreatedByLabelKey]; ok && v != "" {
		labels[ShadowPodInferenceSetLabelKey] = v
	}

	udsTokenizerImage := r.Config.UDSTokenizerImage
	if udsTokenizerImage == "" {
		udsTokenizerImage = DefaultUDSTokenizerImage
	}

	shadow := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            shadowName,
			Namespace:       shadowNS,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
			Annotations: map[string]string{
				"kaito.sh/original-pod": original.Namespace + "/" + original.Name,
			},
		},
		Spec: corev1.PodSpec{
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{{
							MatchExpressions: []corev1.NodeSelectorRequirement{{
								Key:      LabelFakeNode,
								Operator: corev1.NodeSelectorOpDoesNotExist,
							}},
						}},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyAlways,
			// UDS tokenizer runs as a native sidecar (init container with restartPolicy=Always).
			InitContainers: []corev1.Container{
				{
					Name:            "uds-tokenizer",
					Image:           udsTokenizerImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					RestartPolicy:   ptr.To(corev1.ContainerRestartPolicyAlways),
					Env: []corev1.EnvVar{
						{Name: "LOG_LEVEL", Value: "INFO"},
						{Name: "PROBE_PORT", Value: fmt.Sprintf("%d", UDSTokenizerProbePort)},
					},
					Ports: []corev1.ContainerPort{
						{Name: "health", ContainerPort: UDSTokenizerProbePort},
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/health",
								Port: intstr.FromInt32(UDSTokenizerProbePort),
							},
						},
						InitialDelaySeconds: 10,
						PeriodSeconds:       10,
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/health",
								Port: intstr.FromInt32(UDSTokenizerProbePort),
							},
						},
						InitialDelaySeconds: 5,
						PeriodSeconds:       5,
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "uds-socket", MountPath: "/tmp/tokenizer"},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:            "llm-d-inference-sim",
					Image:           r.Config.ShadowPodImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Args:            []string{"--config", "/config/config.yaml"},
					Env: []corev1.EnvVar{
						{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"},
						}},
						{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"},
						}},
						{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
						}},
					},
					Ports: []corev1.ContainerPort{
						{Name: "http", ContainerPort: servingPort, Protocol: corev1.ProtocolTCP},
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/health",
								Port: intstr.FromString("http"),
							},
						},
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/ready",
								Port: intstr.FromString("http"),
							},
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "config", MountPath: "/config"},
						{Name: "uds-socket", MountPath: "/tmp/tokenizer"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "config",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: shadowName + "-config",
							},
						},
					},
				},
				{
					Name: "uds-socket",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
		},
	}

	if err := r.Create(ctx, shadow); err != nil {
		return nil, fmt.Errorf("create shadow pod: %w", err)
	}
	return shadow, nil
}

// patchOriginalPodStatus patches the original (Pending) pod's status fields so
// that from the control-plane's perspective the pod is Running/Ready.
//
// The podIP is set to the shadow pod's real CNI IP so the
// Gateway/EPP routes inference traffic to the actual LLM Mocker process.
func (r *ShadowPodReconciler) patchOriginalPodStatus(ctx context.Context, original *corev1.Pod, shadow *corev1.Pod) error {
	patch := client.MergeFrom(original.DeepCopy())
	now := metav1.Now()
	shadowIP := shadow.Status.PodIP

	containerStatuses := make([]corev1.ContainerStatus, 0, len(original.Spec.Containers))
	for i, c := range original.Spec.Containers {
		imageID := c.Image
		if i < len(shadow.Status.ContainerStatuses) {
			imageID = shadow.Status.ContainerStatuses[i].ImageID
		}
		containerStatuses = append(containerStatuses, corev1.ContainerStatus{
			Name:    c.Name,
			Image:   c.Image,
			ImageID: imageID,
			Ready:   true,
			Started: ptr.To(true),
			State: corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{StartedAt: now},
			},
		})
	}

	original.Status = corev1.PodStatus{
		Phase: corev1.PodRunning,
		// This IP is what the Gateway/EPP uses to forward inference traffic.
		// It resolves to the shadow pod on a real AKS worker node.
		PodIP:  shadowIP,
		PodIPs: []corev1.PodIP{{IP: shadowIP}},
		HostIP: shadow.Status.HostIP,
		Conditions: []corev1.PodCondition{
			makePodCondition(corev1.PodScheduled, corev1.ConditionTrue, "PodScheduled", "accepted by fake node", now),
			makePodCondition(corev1.PodInitialized, corev1.ConditionTrue, "PodInitialized", "initialized", now),
			makePodCondition(corev1.ContainersReady, corev1.ConditionTrue, "ContainersReady", "shadow pod running", now),
			makePodCondition(corev1.PodReady, corev1.ConditionTrue, "PodReady", "shadow pod ready", now),
		},
		ContainerStatuses: containerStatuses,
		StartTime:         &now,
		Message:           "Running via shadow pod " + shadow.Namespace + "/" + shadow.Name,
	}

	if err := r.Status().Patch(ctx, original, patch); err != nil {
		return fmt.Errorf("status subresource patch: %w", err)
	}
	return nil
}

// shadowPodToOriginalPod maps a shadow pod back to a reconcile.
func (r *ShadowPodReconciler) shadowPodToOriginalPod(_ context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	ref, ok := pod.Labels[ShadowPodLabelKey]
	if !ok {
		return nil
	}
	// Label value uses "." as separator (not "/" which is invalid in labels).
	parts := strings.SplitN(ref, ".", 2)
	if len(parts) != 2 {
		return nil
	}
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: parts[0], Name: parts[1]}},
	}
}

func isPendingOnFakeNode(obj client.Object) bool {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return false
	}
	// Only process pods created by KAITO InferenceSets
	if _, hasLabel := pod.Labels["inferenceset.kaito.sh/created-by"]; !hasLabel {
		return false
	}
	return pod.Spec.NodeName != "" &&
		strings.HasPrefix(pod.Spec.NodeName, "fake-") &&
		pod.Status.Phase == corev1.PodPending
}

func isShadowPod(pod *corev1.Pod) bool {
	_, ok := pod.Labels[ShadowPodLabelKey]
	return ok
}

func shadowPodName(original *corev1.Pod) string {
	name := "shadow-" + original.Namespace + "-" + original.Name
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}

func makePodCondition(t corev1.PodConditionType, s corev1.ConditionStatus, reason, msg string, now metav1.Time) corev1.PodCondition {
	return corev1.PodCondition{
		Type:               t,
		Status:             s,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            msg,
	}
}

// ensureSimConfigMap creates the inference simulator ConfigMap if it does not exist.
// The config enables KV cache but does not set any threshold so cache_threshold
// is never triggered. The port is set to match the original pod's serving port.
//
// ownerRef is set on the ConfigMap so it is garbage-collected together with
// the original pod (same namespace, K8s GC cascades the delete).
func (r *ShadowPodReconciler) ensureSimConfigMap(ctx context.Context, namespace, shadowName, modelName, servedModelName string, port int32, ownerRef metav1.OwnerReference) error {
	cmName := shadowName + "-config"
	existing := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cmName}, existing); err == nil {
		return nil
	}

	configYAML := fmt.Sprintf(`port: %d
model: "%s"
served-model-name:
- "%s"
mode: "random"
max-num-seqs: 5
max-model-len: 32768
enable-kvcache: true
kv-cache-size: 4096
block-size: 16
time-to-first-token: 100ms
inter-token-latency: 30ms
`, port, modelName, servedModelName)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: namespace,
			Labels: map[string]string{
				LabelManagedBy: ControllerName,
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Data: map[string]string{
			"config.yaml": configYAML,
		},
	}

	if err := r.Create(ctx, cm); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create configmap %s: %w", cmName, err)
	}
	return nil
}

// extractModelName attempts to find the model name from the original pod's
// container args or command. It looks for "--model" first, then falls back
// to "--served-model-name" if not found.
func extractModelName(pod *corev1.Pod) string {
	// First pass: look for --model
	for _, c := range pod.Spec.Containers {
		if name := findArgValue(c.Command, "--model"); name != "" {
			return name
		}
		if name := findArgValue(c.Args, "--model"); name != "" {
			return name
		}
	}
	// Second pass: look for --served-model-name
	for _, c := range pod.Spec.Containers {
		if name := findArgValue(c.Command, "--served-model-name"); name != "" {
			return name
		}
		if name := findArgValue(c.Args, "--served-model-name"); name != "" {
			return name
		}
	}
	return DefaultModelName
}

// extractServedModelName returns the served model name from the original pod.
// If the pod has an explicit --served-model-name, that is used (it is the
// user-facing alias that EPP matches requests against). Otherwise, modelName
// (typically the HuggingFace model ID from --model) is used as the fallback.
func extractServedModelName(pod *corev1.Pod, modelName string) string {
	for _, c := range pod.Spec.Containers {
		if name := findArgValue(c.Command, "--served-model-name"); name != "" {
			return name
		}
		if name := findArgValue(c.Args, "--served-model-name"); name != "" {
			return name
		}
	}
	return modelName
}

// extractServingPort returns the first declared containerPort from the original
// pod. If no port is declared, it falls back to InferenceSimPort (8001).
// This ensures the inference simulator listens on the same port that
// KAITO Services and EPP expect traffic to reach.
func extractServingPort(pod *corev1.Pod) int32 {
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.ContainerPort > 0 {
				return p.ContainerPort
			}
		}
	}
	return InferenceSimPort
}

// findArgValue scans a slice of arguments for a flag (e.g. "--model") and
// returns its value. Supports:
//   - "--flag value" as separate array elements
//   - "--flag=value" as a single array element
//   - Shell-wrapped: "/bin/sh", "-c", "cmd --flag=value ..." where the flag
//     is embedded inside a single string (used by KAITO InferenceSet pods)
func findArgValue(args []string, flag string) string {
	// Pass 1: check for standalone elements.
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, flag+"=") {
			return strings.TrimPrefix(arg, flag+"=")
		}
	}
	// Pass 2: search inside shell-wrapped command strings.
	for _, arg := range args {
		if idx := strings.Index(arg, flag+"="); idx >= 0 {
			rest := arg[idx+len(flag)+1:]
			if sp := strings.IndexByte(rest, ' '); sp >= 0 {
				return rest[:sp]
			}
			return rest
		}
		if idx := strings.Index(arg, flag+" "); idx >= 0 {
			rest := strings.TrimLeft(arg[idx+len(flag)+1:], " ")
			if sp := strings.IndexByte(rest, ' '); sp >= 0 {
				return rest[:sp]
			}
			return rest
		}
	}
	return ""
}
