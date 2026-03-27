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
//  1. Creates a "shadow pod" in Config.ShadowPodNamespace on a real AKS node.
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
		original.Annotations[AnnotationShadowPodRef] = r.Config.ShadowPodNamespace + "/" + shadowName
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
// The shadow pod:
//   - Runs in Config.ShadowPodNamespace on a real AKS worker node.
//   - Uses node anti-affinity to avoid fake nodes (LabelFakeNode).
//   - Runs Config.ShadowPodImage (the LLM Mocker).
//   - Is labelled with ShadowPodLabelKey=<namespace>.<name> so the secondary watch
//     can map it back to the original pod.
//   - Does NOT carry the fake-node taint toleration — it must land on a real node.
func (r *ShadowPodReconciler) ensureShadowPod(ctx context.Context, original *corev1.Pod, shadowName string) (*corev1.Pod, error) {
	existing := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Namespace: r.Config.ShadowPodNamespace, Name: shadowName}, existing)
	if err == nil {
		return existing, nil
	}
	if !errors.IsNotFound(err) {
		return nil, fmt.Errorf("get shadow pod: %w", err)
	}

	// Inherit container port definitions from the original pod so traffic
	// can reach the mocker on the correct port.
	var ports []corev1.ContainerPort
	for _, c := range original.Spec.Containers {
		ports = append(ports, c.Ports...)
	}

	// Shadow pods run the lightweight LLM mocker — don't inherit the
	// original pod's GPU resource requests which can't be satisfied on
	// real worker nodes.
	resources := corev1.ResourceRequirements{}

	labels := map[string]string{
		LabelManagedBy:    ControllerName,
		ShadowPodLabelKey: original.Namespace + "." + original.Name,
		// No workload labels — shadow pods must not be selected by KAITO Services.
	}

	// Readiness probe port: first declared port or 5000 (vLLM default).
	probePort := intstr.FromInt32(5000)
	if len(ports) > 0 {
		probePort = intstr.FromInt32(ports[0].ContainerPort)
	}

	shadow := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      shadowName,
			Namespace: r.Config.ShadowPodNamespace,
			Labels:    labels,
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
			// Don't inherit ServiceAccountName — the original pod's SA
			// likely doesn't exist in the shadow pod namespace.
			Containers: []corev1.Container{
				{
					Name:            "llm-mocker",
					Image:           r.Config.ShadowPodImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Ports:           ports,
					Resources:       resources,
					Env: []corev1.EnvVar{
						{Name: "ORIGINAL_POD_NAME", Value: original.Name},
						{Name: "ORIGINAL_POD_NAMESPACE", Value: original.Namespace},
						{Name: "ORIGINAL_NODE_NAME", Value: original.Spec.NodeName},
						{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
						}},
					},
					// The readiness probe gates the status patch: we only
					// mark the original pod Running once the mocker is
					// actually accepting connections on the shadow IP.
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/healthz",
								Port: probePort,
							},
						},
						InitialDelaySeconds: 2,
						PeriodSeconds:       5,
						FailureThreshold:    6,
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
