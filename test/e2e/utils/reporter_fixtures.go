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
	"encoding/json"
	"fmt"
	"math/rand"
	"os/exec"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
)

// Fixtures for the zero-cost status-reporter specs. They build the objects the
// reporter evaluates directly — a managed namespace, an InferenceSet, a stub
// EPP Deployment, a mock ModelMirror, a stub model pod — instead of deploying
// the modelharness and modeldeployment charts, so no GPU node is provisioned.

const (
	// managedByLabel / managedByValue mark a namespace as modelharness-managed,
	// which is all the reporter needs to discover it.
	managedByLabel = "productionstack.kaito.sh/managed-by"
	managedByValue = "modelharness"

	// stubEPPImage is a tiny always-Ready container standing in for the EPP:
	// the reporter only checks the EPP Deployment's readiness.
	stubEPPImage = "mcr.microsoft.com/oss/kubernetes/pause:3.6"
	// stubMetricsImage serves a static /metrics file standing in for a model
	// pod that is downloading its weights.
	stubMetricsImage = "mcr.microsoft.com/azurelinux/base/python:3.12"
	// MissingImage is an image reference that can never be pulled.
	MissingImage = "mcr.microsoft.com/oss/kaito/e2e-image-that-does-not-exist:v0.0.0"

	// labelCreatedBy is the label KAITO stamps on an InferenceSet's pods.
	labelCreatedBy = "inferenceset.kaito.sh/created-by"
)

// ModelMirrorGVR is the KAITO ModelMirror resource.
var ModelMirrorGVR = schema.GroupVersionResource{Group: "kaito.sh", Version: "v1alpha1", Resource: "modelmirrors"}

// UniqueName appends a random suffix so a rerun never collides with the
// namespaces, objects or reporter Events of an earlier run.
func UniqueName(prefix string) string {
	return fmt.Sprintf("%s-%04x", prefix, rand.Intn(0x10000)) //nolint:gosec // not security sensitive
}

// CreateManagedNamespace creates a namespace carrying the modelharness
// discovery label, without installing the modelharness chart.
func CreateManagedNamespace(ctx context.Context, name string) error {
	cs, err := GetK8sClientset()
	if err != nil {
		return err
	}
	_, err = cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{managedByLabel: managedByValue},
	}}, metav1.CreateOptions{})
	return err
}

// DeleteManagedNamespace deletes a namespace created by CreateManagedNamespace
// and waits for it to disappear, so KAITO has cleaned up its children.
func DeleteManagedNamespace(ctx context.Context, name string, timeout time.Duration) error {
	cs, err := GetK8sClientset()
	if err != nil {
		return err
	}
	if err := cs.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := cs.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return nil
		}
		time.Sleep(PollInterval)
	}
	return fmt.Errorf("namespace %s not deleted within %s", name, timeout)
}

// InferenceSetSpec describes an InferenceSet built directly by the reporter
// specs. Exactly one of Preset or PodTemplate is set.
type InferenceSetSpec struct {
	Namespace    string
	Name         string
	Replicas     int32
	InstanceType string
	// NodeSelector is template.resource.labelSelector.matchLabels: the nodes
	// KAITO may use or provision for the workload.
	NodeSelector map[string]string
	// Annotations are spec.template.metadata.annotations, which KAITO copies
	// onto every child Workspace.
	Annotations map[string]string
	Preset      string
	PodTemplate *corev1.PodTemplateSpec
}

// CreateInferenceSet creates the InferenceSet described by spec.
func CreateInferenceSet(ctx context.Context, spec InferenceSetSpec) error {
	dyn, err := GetDynamicClient()
	if err != nil {
		return err
	}
	inference := map[string]interface{}{}
	if spec.Preset != "" {
		inference["preset"] = map[string]interface{}{"name": spec.Preset}
	}
	if spec.PodTemplate != nil {
		raw, err := json.Marshal(spec.PodTemplate)
		if err != nil {
			return err
		}
		var tpl map[string]interface{}
		if err := json.Unmarshal(raw, &tpl); err != nil {
			return err
		}
		inference["template"] = tpl
	}
	resource := map[string]interface{}{"instanceType": spec.InstanceType}
	if len(spec.NodeSelector) > 0 {
		resource["labelSelector"] = map[string]interface{}{"matchLabels": toInterfaceMap(spec.NodeSelector)}
	}
	metadata := map[string]interface{}{"labels": map[string]interface{}{"app": spec.Name}}
	if len(spec.Annotations) > 0 {
		metadata["annotations"] = toInterfaceMap(spec.Annotations)
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kaito.sh/v1beta1",
		"kind":       "InferenceSet",
		"metadata":   map[string]interface{}{"name": spec.Name, "namespace": spec.Namespace},
		"spec": map[string]interface{}{
			"replicas":      int64(spec.Replicas),
			"labelSelector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": spec.Name}},
			"template": map[string]interface{}{
				"metadata":  metadata,
				"resource":  resource,
				"inference": inference,
			},
		},
	}}
	_, err = dyn.Resource(InferenceSetGVR).Namespace(spec.Namespace).Create(ctx, obj, metav1.CreateOptions{})
	return err
}

// ReadyCondition is an InferenceSet's InferenceSetReady condition.
type ReadyCondition struct {
	Reason             string
	Message            string
	LastTransitionTime time.Time
}

// GetInferenceSetReadyCondition returns the InferenceSet's InferenceSetReady
// condition, or a zero ReadyCondition when it is not set.
func GetInferenceSetReadyCondition(ctx context.Context, namespace, name string) (ReadyCondition, error) {
	dyn, err := GetDynamicClient()
	if err != nil {
		return ReadyCondition{}, err
	}
	is, err := dyn.Resource(InferenceSetGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ReadyCondition{}, err
	}
	conds, _, _ := unstructured.NestedSlice(is.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]interface{})
		if m["type"] != "InferenceSetReady" {
			continue
		}
		out := ReadyCondition{}
		out.Reason, _ = m["reason"].(string)
		out.Message, _ = m["message"].(string)
		if ts, _ := m["lastTransitionTime"].(string); ts != "" {
			out.LastTransitionTime, _ = time.Parse(time.RFC3339, ts)
		}
		return out, nil
	}
	return ReadyCondition{}, nil
}

// CreateStubEPP creates the `<inferenceset>-inferencepool-epp` Deployment the
// reporter checks for every InferenceSet, with the modeldeployment chart's
// labels, backed by an always-Ready pause container, and waits until it is
// Ready.
func CreateStubEPP(ctx context.Context, namespace, inferenceSet string, timeout time.Duration) (string, error) {
	cs, err := GetK8sClientset()
	if err != nil {
		return "", err
	}
	name := EPPDeploymentName(inferenceSet)
	labels := map[string]string{
		"app":                   name,
		"kaito.sh/inferenceset": inferenceSet,
		"kaito.sh/owned-by":     "modeldeployment",
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "epp",
					Image: stubEPPImage,
				}}},
			},
		},
	}
	if _, err := cs.AppsV1().Deployments(namespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		return "", err
	}
	return name, WaitForDeploymentReplicas(ctx, namespace, name, 1, timeout)
}

// CreateModelMirror creates a static mock ModelMirror. KAITO's ModelMirror
// controller must be disabled (ModelMirror feature gate off) so it does not
// reconcile the mock.
func CreateModelMirror(ctx context.Context, namespace, name string) error {
	dyn, err := GetDynamicClient()
	if err != nil {
		return err
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kaito.sh/v1alpha1",
		"kind":       "ModelMirror",
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
		"spec":       map[string]interface{}{"mode": "Static"},
	}}
	_, err = dyn.Resource(ModelMirrorGVR).Namespace(namespace).Create(ctx, obj, metav1.CreateOptions{})
	return err
}

// SetModelMirrorDownloading sets the mock ModelMirror's status to an
// in-progress download at bytesPerSecond.
func SetModelMirrorDownloading(ctx context.Context, namespace, name string, bytesPerSecond int64) error {
	return patchModelMirrorStatus(ctx, namespace, name, map[string]interface{}{
		"phase":          "Pending",
		"failureMessage": nil,
		"download":       map[string]interface{}{"speedBytesPerSecond": bytesPerSecond, "remainingSeconds": int64(900)},
	})
}

// SetModelMirrorReady marks the mock ModelMirror's download as complete.
func SetModelMirrorReady(ctx context.Context, namespace, name string) error {
	return patchModelMirrorStatus(ctx, namespace, name, map[string]interface{}{
		"phase":    "Ready",
		"download": map[string]interface{}{"speedBytesPerSecond": int64(0), "remainingSeconds": int64(0)},
	})
}

func patchModelMirrorStatus(ctx context.Context, namespace, name string, status map[string]interface{}) error {
	dyn, err := GetDynamicClient()
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]interface{}{"status": status})
	if err != nil {
		return err
	}
	_, err = dyn.Resource(ModelMirrorGVR).Namespace(namespace).Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{}, "status")
	return err
}

// CreateDownloadingModelPod creates a stub model pod of the InferenceSet that
// is never Ready and serves the vLLM self-download gauge at bytesPerSecond on
// port 5000, and waits until it is Running.
func CreateDownloadingModelPod(ctx context.Context, namespace, inferenceSet string, bytesPerSecond int64, timeout time.Duration) (string, error) {
	cs, err := GetK8sClientset()
	if err != nil {
		return "", err
	}
	name := inferenceSet + "-stub-0"
	script := fmt.Sprintf(
		"mkdir -p /tmp/www && printf 'kaito_model_download_speed_bytes_per_second %d\\n' > /tmp/www/metrics && exec python3 -m http.server 5000 --directory /tmp/www",
		bytesPerSecond)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{labelCreatedBy: inferenceSet}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:    "model",
			Image:   stubMetricsImage,
			Command: []string{"/bin/sh", "-c", script},
			Ports:   []corev1.ContainerPort{{ContainerPort: 5000}},
			// Never Ready: the reporter only samples model pods that are still
			// downloading.
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:  corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"false"}}},
				PeriodSeconds: 10,
			},
		}}},
	}
	if _, err := cs.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil && p.Status.Phase == corev1.PodRunning {
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodReady {
					return name, nil
				}
			}
		}
		time.Sleep(PollInterval)
	}
	return "", fmt.Errorf("stub model pod %s/%s not running within %s", namespace, name, timeout)
}

// ExistingNode returns a Ready node that KAITO would treat as a BYO node — not
// provisioned by Karpenter or gpu-node-mocker — with its hostname and
// instance-type labels.
func ExistingNode(ctx context.Context) (hostname, instanceType string, err error) {
	cs, err := GetK8sClientset()
	if err != nil {
		return "", "", err
	}
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", "", err
	}
	for _, n := range nodes.Items {
		if _, ok := n.Labels["karpenter.sh/nodepool"]; ok {
			continue
		}
		if _, ok := n.Labels["kaito.sh/fake-node"]; ok {
			continue
		}
		if n.Spec.Unschedulable || n.Labels[corev1.LabelInstanceTypeStable] == "" {
			continue
		}
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				return n.Labels[corev1.LabelHostname], n.Labels[corev1.LabelInstanceTypeStable], nil
			}
		}
	}
	return "", "", fmt.Errorf("no Ready BYO node found")
}

// GPUNodeMockerDeployed reports whether gpu-node-mocker is installed. It fakes
// node provisioning — a NodeClaim becomes a fake Node without any Azure call —
// so a cloud provisioning failure can never happen on such a cluster.
func GPUNodeMockerDeployed(ctx context.Context) (bool, error) {
	cs, err := GetK8sClientset()
	if err != nil {
		return false, err
	}
	deps, err := cs.AppsV1().Deployments("").List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=gpu-node-mocker",
	})
	if err != nil {
		return false, err
	}
	return len(deps.Items) > 0, nil
}

// ClusterRegion returns the Azure region of the cluster's nodes.
func ClusterRegion(ctx context.Context) (string, error) {
	cs, err := GetK8sClientset()
	if err != nil {
		return "", err
	}
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil || len(nodes.Items) == 0 {
		return "", fmt.Errorf("list nodes: %v", err)
	}
	return nodes.Items[0].Labels[corev1.LabelTopologyRegion], nil
}

// azArgs appends the subscription selector when AZURE_SUBSCRIPTION_ID is set.
func azArgs(args ...string) []string {
	if sub := GetEnv("AZURE_SUBSCRIPTION_ID"); sub != "" {
		args = append(args, "--subscription", sub)
	}
	return args
}

// AvailableQuota returns the free vCPU quota of a VM family in region, via the
// az CLI.
func AvailableQuota(ctx context.Context, region, family string) (int, error) {
	out, err := exec.CommandContext(ctx, "az", azArgs("vm", "list-usage", "--location", region,
		"--query", fmt.Sprintf("[?name.value=='%s'].[currentValue,limit]", family), "-o", "tsv")...).Output()
	if err != nil {
		return 0, fmt.Errorf("az vm list-usage: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0, fmt.Errorf("quota family %s not found in %s", family, region)
	}
	current, err1 := strconv.Atoi(fields[0])
	limit, err2 := strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("unexpected az output %q", out)
	}
	return limit - current, nil
}

// RestrictedGPUSKU returns an N-series VM size that is not available to the
// subscription in region, via the az CLI, or "" when there is none.
func RestrictedGPUSKU(ctx context.Context, region string) (string, error) {
	out, err := exec.CommandContext(ctx, "az", azArgs("vm", "list-skus", "--location", region,
		"--resource-type", "virtualMachines",
		"--query", "[?starts_with(name,'Standard_N') && length(restrictions[?reasonCode=='NotAvailableForSubscription']) > `0`].name",
		"-o", "tsv")...).Output()
	if err != nil {
		return "", fmt.Errorf("az vm list-skus: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}

func toInterfaceMap(in map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
