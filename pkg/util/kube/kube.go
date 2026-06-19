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

// Package kube holds small, domain-agnostic Kubernetes helpers shared by the
// status-reporter evaluators: Deployment readiness summarisation and
// unstructured object accessors.
package kube

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
)

// DeploymentPodUnavailable inspects the pods owned by a Deployment. missing is
// true when the Deployment itself is gone. unavailable is true when a pod is
// not serving — either its Pod Ready condition is False, or it has no Ready
// condition at all because it is still Pending/unschedulable — or when the
// Deployment has no pods at all (scaled to zero or every replica removed), which
// would otherwise read as 0/0 = healthy and never surface. The kubelet only adds
// the Ready condition after the pod is bound to a node, so a Ready-only probe
// silently misses an unschedulable pod (a real blind spot: all higher resources
// can look healthy while no pod ever runs). notReadySince carries a
// lastTransitionTime the caller can debounce on: the Ready condition's
// transition for a not-ready pod, or the PodScheduled transition (falling back
// to the pod creation time) for a pending one — so a quick rolling upgrade never
// trips the window while a genuinely stuck pod does; it is zero for the no-pods
// case, deferring to the caller's reason-level startup-grace timer. No container-reason allowlist is used — the reason
// strings are kubelet/runtime implementation details and not an API contract, so
// classification is deliberately time-based instead. cause is a short
// human-readable hint for the event message only.
func DeploymentPodUnavailable(ctx context.Context, cs kubernetes.Interface, namespace, name string) (missing, unavailable bool, cause string, notReadySince time.Time) {
	dep, err := cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, false, "", time.Time{}
		}
		return false, false, "", time.Time{} // transient API error — nothing to emit
	}
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(dep.Spec.Selector),
	})
	if err != nil {
		return false, false, "", time.Time{}
	}
	// A Deployment with no pods at all cannot serve.
	if len(pods.Items) == 0 {
		cause := "no pods are running for the Deployment"
		if dep.Spec.Replicas != nil && *dep.Spec.Replicas == 0 {
			cause = "Deployment is scaled to zero replicas"
		}
		return false, true, cause, time.Time{}
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue // being deleted during a rollout — not an anomaly
		}
		if ready := PodReadyCondition(p); ready != nil {
			if ready.Status == corev1.ConditionFalse {
				return false, true, PodNotReadyCause(p, *ready), ready.LastTransitionTime.Time
			}
			continue // Ready=True — this pod is serving
		}
		// No Ready condition: the kubelet has not taken over, so the pod is
		// still Pending (typically unschedulable). This is the case a
		// Ready-only probe would silently miss.
		if p.Status.Phase == corev1.PodPending {
			since, hint := pendingCause(p)
			return false, true, hint, since
		}
	}
	return false, false, "", time.Time{}
}

// PodReadyCondition returns the pod's Ready condition, or nil when it is absent
// (an unscheduled/Pending pod has no Ready condition until the kubelet adds it).
func PodReadyCondition(p *corev1.Pod) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == corev1.PodReady {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

// pendingCause renders the debounce anchor and a short hint for a Pending pod
// that has no Ready condition yet. It prefers the PodScheduled condition (which
// carries the "Unschedulable" reason and its lastTransitionTime), falling back
// to the pod creation time.
func pendingCause(p *corev1.Pod) (time.Time, string) {
	for i := range p.Status.Conditions {
		c := &p.Status.Conditions[i]
		if c.Type != corev1.PodScheduled || c.Status != corev1.ConditionFalse {
			continue
		}
		switch {
		case c.Reason != "" && c.Message != "":
			return c.LastTransitionTime.Time, fmt.Sprintf("%s: %s", c.Reason, c.Message)
		case c.Message != "":
			return c.LastTransitionTime.Time, c.Message
		case c.Reason != "":
			return c.LastTransitionTime.Time, c.Reason
		default:
			return c.LastTransitionTime.Time, "pod unschedulable"
		}
	}
	return p.CreationTimestamp.Time, "pod pending"
}

// PodNotReadyCause renders a short, human-readable hint for a pod whose Ready
// condition is False. It prefers the most specific container-level reason, then
// falls back to the Pod Ready condition's reason/message. This string is used
// only for the event message, never for the emit decision.
func PodNotReadyCause(p *corev1.Pod, ready corev1.PodCondition) string {
	for _, cst := range p.Status.ContainerStatuses {
		if w := cst.State.Waiting; w != nil && w.Reason != "" {
			return fmt.Sprintf("%s on %s container", w.Reason, cst.Name)
		}
		if t := cst.State.Terminated; t != nil && t.Reason != "" {
			return fmt.Sprintf("%s on %s container", t.Reason, cst.Name)
		}
	}
	switch {
	case ready.Reason != "" && ready.Message != "":
		return fmt.Sprintf("%s: %s", ready.Reason, ready.Message)
	case ready.Reason != "":
		return ready.Reason
	case ready.Message != "":
		return ready.Message
	default:
		return "pod not ready"
	}
}

// terminalWaitingReasons / terminalTerminatedReasons are the container
// waiting/terminated reasons treated as a non-transient (terminal) failure, as
// opposed to a container that is merely still starting. A workload with a long,
// legitimate startup (e.g. KAITO's vLLM StartupProbe budgets up to 30 minutes
// for a healthy model to load) is Ready=False throughout, so only these
// unambiguous states classify a pod as failed regardless of the startup budget.
var (
	terminalWaitingReasons = map[string]bool{
		"ImagePullBackOff":           true,
		"ErrImagePull":               true,
		"ErrImageNeverPull":          true,
		"InvalidImageName":           true,
		"CreateContainerConfigError": true,
		"CreateContainerError":       true,
		"RunContainerError":          true,
		"CrashLoopBackOff":           true,
	}
	terminalTerminatedReasons = map[string]bool{
		"OOMKilled":          true,
		"ContainerCannotRun": true,
		"Error":              true,
		"DeadlineExceeded":   true,
		"StartError":         true,
	}
)

// PodTerminalContainerCause reports whether any of pod's init or main containers
// is in a terminal (non-transient) failure state — ImagePullBackOff,
// CrashLoopBackOff, OOMKilled, etc. — returning a short human-readable hint and
// true when so. A container that is merely still starting is not classified as
// terminal, so a pod that is simply slow to become Ready is not flagged.
func PodTerminalContainerCause(p *corev1.Pod) (string, bool) {
	statuses := make([]corev1.ContainerStatus, 0, len(p.Status.InitContainerStatuses)+len(p.Status.ContainerStatuses))
	statuses = append(statuses, p.Status.InitContainerStatuses...)
	statuses = append(statuses, p.Status.ContainerStatuses...)
	for _, cst := range statuses {
		if w := cst.State.Waiting; w != nil && terminalWaitingReasons[w.Reason] {
			return fmt.Sprintf("%s on %s container", w.Reason, cst.Name), true
		}
		if t := cst.State.Terminated; t != nil && terminalTerminatedReasons[t.Reason] {
			return fmt.Sprintf("%s on %s container", t.Reason, cst.Name), true
		}
	}
	return "", false
}

// ConditionStatus extracts status.conditions[type] from an unstructured object
// and returns (status, reason, message, found).
func ConditionStatus(obj *unstructured.Unstructured, condType string) (status, reason, message string, found bool) {
	conds, ok, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !ok {
		return "", "", "", false
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == condType {
			status, _ = m["status"].(string)
			reason, _ = m["reason"].(string)
			message, _ = m["message"].(string)
			return status, reason, message, true
		}
	}
	return "", "", "", false
}

// NestedString is a convenience wrapper over unstructured.NestedString that
// ignores the error and not-found flag.
func NestedString(obj map[string]interface{}, fields ...string) string {
	s, _, _ := unstructured.NestedString(obj, fields...)
	return s
}
