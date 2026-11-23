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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
)

// PodUnavailability is why a Deployment or DaemonSet is not fully serving:
// one of its pods that is not serving or, for a Deployment, the absence of any
// pod.
type PodUnavailability struct {
	// Cause is a short human-readable hint for the event message only.
	Cause string
	// NodeName is the node the pod is assigned to. It is empty for a pod that
	// has no node yet and for a Deployment without pods.
	NodeName string
	// Since is the lastTransitionTime the caller debounces on, so a quick
	// rolling upgrade never trips the window while a genuinely stuck pod does:
	// the Ready condition's transition for a pod the kubelet reports as not
	// ready, or the PodScheduled transition for a pod the kubelet has not
	// reported yet (see pendingCause). It is zero for a Deployment without
	// pods, deferring to the caller's reason-level startup-grace timer.
	Since time.Time
	// AwaitingNode is true for a pod that has no node yet, typically because no
	// node has room for it and one is being provisioned, and for a DaemonSet
	// pod on a node that has only just registered (see DaemonSetPodUnavailable).
	// Callers give such a pod a longer startup grace than one that is already
	// on a node.
	AwaitingNode bool
}

// DeploymentPodUnavailable inspects the pods owned by a Deployment and reports
// whether one is not serving, and why: its Pod Ready condition is False, or it
// has no Ready condition at all because it is still Pending, typically
// unschedulable while it waits for a node. Pods in a terminal phase are ignored
// (see livePods). A Deployment with no live pods (scaled to zero, every
// replica removed, or only evicted pods left), which would otherwise read as
// 0/0 = healthy and never surface, is unavailable too. A missing Deployment,
// or one that cannot be read, is reported as available: callers check only the
// readiness of a Deployment that exists. The kubelet only adds the Ready
// condition after the pod is bound to a node, so a Ready-only probe silently
// misses an unschedulable pod (a real blind spot: all higher resources can
// look healthy while no pod ever runs). When several pods are unavailable, a
// pod failing on its node is reported before one still waiting for a node
// (see pickUnavailable). No container-reason allowlist is used — the reason
// strings are kubelet/runtime implementation details and not an API contract,
// so classification is deliberately time-based instead.
func DeploymentPodUnavailable(ctx context.Context, cs kubernetes.Interface, namespace, name string) (PodUnavailability, bool) {
	dep, err := cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return PodUnavailability{}, false // missing, or a transient API error — nothing to emit
	}
	list, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(dep.Spec.Selector),
	})
	if err != nil {
		return PodUnavailability{}, false
	}
	pods := livePods(list.Items)
	// A Deployment with no live pods cannot serve.
	if len(pods) == 0 {
		cause := "no pods are running for the Deployment"
		if dep.Spec.Replicas != nil && *dep.Spec.Replicas == 0 {
			cause = "Deployment is scaled to zero replicas"
		}
		return PodUnavailability{Cause: cause}, true
	}
	return pickUnavailable(pods, nil, nil)
}

// DaemonSetPodUnavailable probes a DaemonSet the same way
// DeploymentPodUnavailable probes a Deployment, but per node: it reports a
// DaemonSet pod that is not Ready (or still Pending), together with the node
// it runs on. Pods on nodes for which skipNode returns true are ignored, so
// simulated nodes that can never run the DaemonSet do not flag it. A pod on a
// node for which newNode returns true is still waiting for its node: the
// DaemonSet controller binds a pod to a node as soon as the node registers,
// before the node can run it, so unlike a Deployment pod it never waits
// unscheduled for a new node. Pods in a terminal phase are ignored (see
// livePods). A DaemonSet with no live pods is not flagged: it may
// legitimately have no eligible node. A missing DaemonSet, or a transient API
// error, reports nothing.
func DaemonSetPodUnavailable(ctx context.Context, cs kubernetes.Interface, namespace, name string, skipNode, newNode func(string) bool) (PodUnavailability, bool) {
	ds, err := cs.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return PodUnavailability{}, false
	}
	list, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: metav1.FormatLabelSelector(ds.Spec.Selector),
	})
	if err != nil {
		return PodUnavailability{}, false
	}
	onNode := func(match func(string) bool) func(*corev1.Pod) bool {
		return func(p *corev1.Pod) bool {
			return match != nil && p.Spec.NodeName != "" && match(p.Spec.NodeName)
		}
	}
	return pickUnavailable(livePods(list.Items), onNode(skipNode), onNode(newNode))
}

// livePods drops the pods in a terminal phase, Failed or Succeeded. A pod the
// kubelet evicted, or stopped when its node shut down, never runs again but is
// kept until it is deleted, while its controller runs a replacement. Counting
// it would flag its component for as long as it is kept.
func livePods(pods []corev1.Pod) []corev1.Pod {
	live := make([]corev1.Pod, 0, len(pods))
	for i := range pods {
		if phase := pods[i].Status.Phase; phase != corev1.PodFailed && phase != corev1.PodSucceeded {
			live = append(live, pods[i])
		}
	}
	return live
}

// pickUnavailable returns the unavailable pod to report among pods, ignoring
// the pods for which skip returns true. A pod for which awaitingNode returns
// true counts as waiting for a node, like a pod that has no node yet. The
// first pod that is failing on its node is reported; only when every
// unavailable pod is still waiting for a node is the first of those reported,
// so the longer startup grace of the wait never hides a pod that is failing.
func pickUnavailable(pods []corev1.Pod, skip, awaitingNode func(*corev1.Pod) bool) (PodUnavailability, bool) {
	var waiting *PodUnavailability
	for i := range pods {
		p := &pods[i]
		if skip != nil && skip(p) {
			continue
		}
		u, ok := podUnavailability(p)
		if !ok {
			continue
		}
		if awaitingNode != nil && awaitingNode(p) {
			u.AwaitingNode = true
		}
		if !u.AwaitingNode {
			return u, true
		}
		if waiting == nil {
			waiting = &u
		}
	}
	if waiting == nil {
		return PodUnavailability{}, false
	}
	return *waiting, true
}

// podUnavailability reports whether a pod is not serving and, if so, why: its
// Ready condition is False, or it is still Pending without one. A pod being
// deleted during a rollout is not an anomaly and is not reported.
func podUnavailability(p *corev1.Pod) (PodUnavailability, bool) {
	if p.DeletionTimestamp != nil {
		return PodUnavailability{}, false
	}
	if ready := PodReadyCondition(p); ready != nil {
		if ready.Status != corev1.ConditionFalse {
			return PodUnavailability{}, false // this pod is serving
		}
		return PodUnavailability{Cause: PodNotReadyCause(p, *ready), NodeName: p.Spec.NodeName, Since: ready.LastTransitionTime.Time}, true
	}
	// No Ready condition: the kubelet has not taken over, so the pod is still
	// Pending, typically unschedulable. This is the case a Ready-only probe
	// would silently miss.
	if p.Status.Phase != corev1.PodPending {
		return PodUnavailability{}, false
	}
	since, cause := pendingCause(p)
	return PodUnavailability{Cause: cause, NodeName: p.Spec.NodeName, Since: since, AwaitingNode: p.Spec.NodeName == ""}, true
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
// that has no Ready condition yet, from its PodScheduled condition. A pod with
// no node is timed from when the scheduler first found it unschedulable (the
// condition is False and carries the "Unschedulable" reason and message). A pod
// already on a node, which the kubelet has not reported yet, is timed from when
// it got the node rather than from its creation, which may predate a long wait
// for a node. Both fall back to the pod creation time.
func pendingCause(p *corev1.Pod) (time.Time, string) {
	since, cause := p.CreationTimestamp.Time, "pod pending"
	for i := range p.Status.Conditions {
		c := &p.Status.Conditions[i]
		if c.Type != corev1.PodScheduled {
			continue
		}
		if !c.LastTransitionTime.IsZero() {
			since = c.LastTransitionTime.Time
		}
		if c.Status == corev1.ConditionFalse {
			switch {
			case c.Reason != "" && c.Message != "":
				cause = fmt.Sprintf("%s: %s", c.Reason, c.Message)
			case c.Message != "":
				cause = c.Message
			case c.Reason != "":
				cause = c.Reason
			default:
				cause = "pod unschedulable"
			}
		}
		break
	}
	return since, cause
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
