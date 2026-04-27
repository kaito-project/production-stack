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

package utils

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// KEDA default parameters (applied when the ScaledObject leaves them unset).
// See github.com/kedacore/keda and the keda-kaito-scaler project:
//   - PollingInterval default: 30s
//   - CooldownPeriod default: 300s
//   - ScaleUp stabilizationWindowSeconds default (set by keda-kaito-scaler):   60s
//   - ScaleDown stabilizationWindowSeconds default (set by keda-kaito-scaler): 300s
const (
	DefaultKEDAPollingInterval    = 30 * time.Second
	DefaultKEDACooldownPeriod     = 300 * time.Second
	DefaultKEDAScaleUpStabilize   = 60 * time.Second
	DefaultKEDAScaleDownStabilize = 300 * time.Second
)

// KEDAParams captures the dynamic KEDA knobs that drive the scaling test
// timeouts. Values are read from the ScaledObject (or fall back to the KEDA
// defaults).
type KEDAParams struct {
	Threshold              int
	PollingInterval        time.Duration
	CooldownPeriod         time.Duration
	ScaleUpStabilization   time.Duration
	ScaleDownStabilization time.Duration
	ScaleUpTotalWait       time.Duration // polling + upStabilization + margin
	ScaleDownTotalWait     time.Duration // polling + downStabilization + margin
}

// GetKEDAParams fetches the ScaledObject managed for the given InferenceSet
// and returns the effective KEDA parameters.
func GetKEDAParams(ctx context.Context, model, namespace string) (KEDAParams, error) {
	dynClient, err := GetDynamicClient()
	if err != nil {
		return KEDAParams{}, err
	}
	// The keda-kaito-scaler creates the ScaledObject with the same name as the
	// InferenceSet, in the same namespace.
	so, err := dynClient.Resource(ScaledObjectGVR).Namespace(namespace).Get(ctx, model, metav1.GetOptions{})
	if err != nil {
		return KEDAParams{}, fmt.Errorf("get ScaledObject %s/%s: %w", namespace, model, err)
	}

	p := KEDAParams{
		PollingInterval:        DefaultKEDAPollingInterval,
		CooldownPeriod:         DefaultKEDACooldownPeriod,
		ScaleUpStabilization:   DefaultKEDAScaleUpStabilize,
		ScaleDownStabilization: DefaultKEDAScaleDownStabilize,
	}

	if v, found, _ := unstructured.NestedInt64(so.Object, "spec", "pollingInterval"); found {
		p.PollingInterval = time.Duration(v) * time.Second
	}
	if v, found, _ := unstructured.NestedInt64(so.Object, "spec", "cooldownPeriod"); found {
		p.CooldownPeriod = time.Duration(v) * time.Second
	}
	if v, found, _ := unstructured.NestedInt64(so.Object, "spec", "advanced",
		"horizontalPodAutoscalerConfig", "behavior", "scaleUp", "stabilizationWindowSeconds"); found {
		p.ScaleUpStabilization = time.Duration(v) * time.Second
	}
	if v, found, _ := unstructured.NestedInt64(so.Object, "spec", "advanced",
		"horizontalPodAutoscalerConfig", "behavior", "scaleDown", "stabilizationWindowSeconds"); found {
		p.ScaleDownStabilization = time.Duration(v) * time.Second
	}

	// Threshold is read from the first trigger's metadata.
	triggers, found, _ := unstructured.NestedSlice(so.Object, "spec", "triggers")
	if found {
		for _, t := range triggers {
			tm, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			metadata, _, _ := unstructured.NestedStringMap(tm, "metadata")
			if v, ok := metadata["threshold"]; ok {
				if n, err := strconv.Atoi(v); err == nil {
					p.Threshold = n
					break
				}
			}
		}
	}
	if p.Threshold == 0 {
		// Fallback to the annotation on the InferenceSet.
		is, err := dynClient.Resource(InferenceSetGVR).Namespace(namespace).Get(ctx, model, metav1.GetOptions{})
		if err == nil {
			if v, ok := is.GetAnnotations()["scaledobject.kaito.sh/threshold"]; ok {
				if n, err := strconv.Atoi(v); err == nil {
					p.Threshold = n
				}
			}
		}
	}

	const margin = 15 * time.Second
	p.ScaleUpTotalWait = p.PollingInterval + p.ScaleUpStabilization + margin
	p.ScaleDownTotalWait = p.PollingInterval + p.ScaleDownStabilization + margin
	return p, nil
}

// GetInferenceSetReplicas returns the current .spec.replicas value of the
// named InferenceSet.
func GetInferenceSetReplicas(ctx context.Context, model, namespace string) (int32, error) {
	dynClient, err := GetDynamicClient()
	if err != nil {
		return 0, err
	}
	is, err := dynClient.Resource(InferenceSetGVR).Namespace(namespace).Get(ctx, model, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("get InferenceSet %s/%s: %w", namespace, model, err)
	}
	v, found, err := unstructured.NestedInt64(is.Object, "spec", "replicas")
	if err != nil || !found {
		return 0, fmt.Errorf("InferenceSet %s/%s: missing .spec.replicas", namespace, model)
	}
	return int32(v), nil
}

// SetInferenceSetReplicas patches the InferenceSet's .spec.replicas. Used for
// test cleanup to restore the baseline after a scaling test run.
func SetInferenceSetReplicas(ctx context.Context, model, namespace string, replicas int32) error {
	dynClient, err := GetDynamicClient()
	if err != nil {
		return err
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas))
	_, err = dynClient.Resource(InferenceSetGVR).Namespace(namespace).
		Patch(ctx, model, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch InferenceSet replicas %s/%s=%d: %w", namespace, model, replicas, err)
	}
	return nil
}

// ScalingInventory captures the set of infrastructure objects that participate
// in an InferenceSet scale event. Snapshotting it before and after lets tests
// reason about what was created or removed by the scaling pipeline.
type ScalingInventory struct {
	// FakeNodeNames lists Node names where kaito.sh/fake-node=true and the
	// node's kaito.sh/workspace label starts with the given model name.
	FakeNodeNames []string
	// ShadowPodNames lists shadow pod names serving this model.
	ShadowPodNames []string
	// NodeClaimNames lists NodeClaim names whose kaito.sh/workspace label
	// starts with the given model name.
	NodeClaimNames []string
	// LeaseNames lists Lease names in kube-node-lease matching FakeNodeNames.
	LeaseNames []string
}

// SnapshotScalingInventory captures the current state of fake nodes, shadow
// pods, NodeClaims, and node leases attributable to the given model.
//
// Filtering uses the kaito.sh/workspace label propagated from NodeClaim →
// fake Node, and matches any workspace name that starts with model+"-" (KAITO
// names workspaces <inferenceSet>-<index>).
func SnapshotScalingInventory(ctx context.Context, clientset *kubernetes.Clientset, dynClient dynamic.Interface, namespace, model string) (ScalingInventory, error) {
	inv := ScalingInventory{}
	wsPrefix := model + "-"
	workspaceMatches := func(ws string) bool {
		return ws == model || strings.HasPrefix(ws, wsPrefix)
	}

	// Fake nodes.
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "kaito.sh/fake-node=true",
	})
	if err != nil {
		return inv, fmt.Errorf("list fake nodes: %w", err)
	}
	for _, n := range nodes.Items {
		ws := n.Labels["kaito.sh/workspace"]
		if workspaceMatches(ws) {
			inv.FakeNodeNames = append(inv.FakeNodeNames, n.Name)
		}
	}

	// Shadow pods (already model-scoped via GetShadowPodsForModel).
	pods, err := GetShadowPodsForModel(ctx, clientset, namespace, model)
	if err != nil && !strings.Contains(err.Error(), "no running shadow pods found") {
		return inv, err
	}
	for _, p := range pods {
		inv.ShadowPodNames = append(inv.ShadowPodNames, p.Name)
	}

	// NodeClaims.
	ncList, err := dynClient.Resource(NodeClaimGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return inv, fmt.Errorf("list NodeClaims: %w", err)
	}
	for _, nc := range ncList.Items {
		ws := nc.GetLabels()["kaito.sh/workspace"]
		if workspaceMatches(ws) {
			inv.NodeClaimNames = append(inv.NodeClaimNames, nc.GetName())
		}
	}

	// Leases matching fake node names.
	for _, nodeName := range inv.FakeNodeNames {
		lease := &coordinationv1.Lease{}
		_, err := clientset.CoordinationV1().Leases("kube-node-lease").
			Get(ctx, nodeName, metav1.GetOptions{})
		if err == nil {
			inv.LeaseNames = append(inv.LeaseNames, nodeName)
			continue
		}
		if !apierrors.IsNotFound(err) {
			return inv, fmt.Errorf("get lease for %s: %w", nodeName, err)
		}
		_ = lease // silence unused
	}
	return inv, nil
}

// DiffInventory returns (added, removed) lists for each sub-collection.
type InventoryDiff struct {
	AddedFakeNodes, RemovedFakeNodes   []string
	AddedShadowPods, RemovedShadowPods []string
	AddedNodeClaims, RemovedNodeClaims []string
	AddedLeases, RemovedLeases         []string
}

func DiffInventory(before, after ScalingInventory) InventoryDiff {
	d := InventoryDiff{}
	d.AddedFakeNodes, d.RemovedFakeNodes = diffStringSlices(before.FakeNodeNames, after.FakeNodeNames)
	d.AddedShadowPods, d.RemovedShadowPods = diffStringSlices(before.ShadowPodNames, after.ShadowPodNames)
	d.AddedNodeClaims, d.RemovedNodeClaims = diffStringSlices(before.NodeClaimNames, after.NodeClaimNames)
	d.AddedLeases, d.RemovedLeases = diffStringSlices(before.LeaseNames, after.LeaseNames)
	return d
}

func diffStringSlices(before, after []string) (added, removed []string) {
	bm := make(map[string]struct{}, len(before))
	for _, s := range before {
		bm[s] = struct{}{}
	}
	am := make(map[string]struct{}, len(after))
	for _, s := range after {
		am[s] = struct{}{}
	}
	for s := range am {
		if _, ok := bm[s]; !ok {
			added = append(added, s)
		}
	}
	for s := range bm {
		if _, ok := am[s]; !ok {
			removed = append(removed, s)
		}
	}
	return added, removed
}

// IsFakeNodeReady returns true when the Node has Ready=True and no
// unreachable taint.
func IsFakeNodeReady(node *corev1.Node) bool {
	for _, t := range node.Spec.Taints {
		if t.Key == "node.kubernetes.io/unreachable" {
			return false
		}
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// CountInferencePoolReadyPods returns the number of Running shadow pods
// (which mirror the InferencePool's inference pods) that are Ready for
// the given modeldeployment in the given namespace.
func CountInferencePoolReadyPods(ctx context.Context, clientset *kubernetes.Clientset, model, namespace string) (int, error) {
	pods, err := GetShadowPodsForModel(ctx, clientset, namespace, model)
	if err != nil {
		if strings.Contains(err.Error(), "no running shadow pods found") {
			return 0, nil
		}
		return 0, err
	}
	ready := 0
	for _, p := range pods {
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready++
				break
			}
		}
	}
	return ready, nil
}
