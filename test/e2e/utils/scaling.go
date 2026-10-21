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
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
)

// KEDA default parameters (applied when the ScaledObject leaves them unset).
// See github.com/kedacore/keda and the keda-kaito-scaler project:
//   - PollingInterval default: 30s
//   - CooldownPeriod default: 300s
//   - ScaleUp stabilizationWindowSeconds default (set by keda-kaito-scaler):   60s
//   - ScaleDown stabilizationWindowSeconds default (set by keda-kaito-scaler): 1800s
const (
	DefaultKEDAPollingInterval    = 30 * time.Second
	DefaultKEDACooldownPeriod     = 300 * time.Second
	DefaultKEDAScaleUpStabilize   = 60 * time.Second
	DefaultKEDAScaleDownStabilize = 1800 * time.Second
)

// KEDAParams captures the dynamic KEDA knobs that drive the scaling test
// timeouts. Values are read from the ScaledObject (or fall back to the KEDA
// defaults).
type KEDAParams struct {
	Threshold              int
	PollingInterval        time.Duration
	CooldownPeriod         time.Duration
	InitialCooldownPeriod  time.Duration
	ScaleUpRatePeriod      time.Duration
	ScaleDownRatePeriod    time.Duration
	ScaleUpStabilization   time.Duration
	ScaleDownStabilization time.Duration
	ScaleUpTotalWait       time.Duration // polling + max(stabilization, rate period) + margin
	ScaleDownTotalWait     time.Duration // polling + max(stabilization, rate period) + margin
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

	p := scalingTimingParams(so.Object)

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
		is, err := dynClient.Resource(InferenceSetGVR).Namespace(namespace).Get(ctx, model, metav1.GetOptions{})
		if err == nil {
			if v, ok := is.GetAnnotations()["scaledobject.kaito.sh/metrics"]; ok {
				var metrics []struct {
					UpThreshold *float64 `json:"upthreshold"`
				}
				if err := yaml.Unmarshal([]byte(v), &metrics); err == nil &&
					len(metrics) > 0 && metrics[0].UpThreshold != nil {
					p.Threshold = int(*metrics[0].UpThreshold)
				}
			}
		}
	}
	return p, nil
}

func scalingTimingParams(object map[string]interface{}) KEDAParams {
	p := KEDAParams{
		PollingInterval:        DefaultKEDAPollingInterval,
		CooldownPeriod:         DefaultKEDACooldownPeriod,
		ScaleUpStabilization:   DefaultKEDAScaleUpStabilize,
		ScaleDownStabilization: DefaultKEDAScaleDownStabilize,
	}

	if v, found, _ := unstructured.NestedInt64(object, "spec", "pollingInterval"); found {
		p.PollingInterval = time.Duration(v) * time.Second
	}
	if v, found, _ := unstructured.NestedInt64(object, "spec", "cooldownPeriod"); found {
		p.CooldownPeriod = time.Duration(v) * time.Second
	}
	if v, found, _ := unstructured.NestedInt64(object, "spec", "initialCooldownPeriod"); found {
		p.InitialCooldownPeriod = time.Duration(v) * time.Second
	}
	if v, found, _ := unstructured.NestedInt64(object, "spec", "advanced",
		"horizontalPodAutoscalerConfig", "behavior", "scaleUp", "stabilizationWindowSeconds"); found {
		p.ScaleUpStabilization = time.Duration(v) * time.Second
	}
	if v, found, _ := unstructured.NestedInt64(object, "spec", "advanced",
		"horizontalPodAutoscalerConfig", "behavior", "scaleDown", "stabilizationWindowSeconds"); found {
		p.ScaleDownStabilization = time.Duration(v) * time.Second
	}

	for direction, period := range map[string]*time.Duration{
		"scaleUp": &p.ScaleUpRatePeriod, "scaleDown": &p.ScaleDownRatePeriod,
	} {
		policies, _, _ := unstructured.NestedSlice(object, "spec", "advanced",
			"horizontalPodAutoscalerConfig", "behavior", direction, "policies")
		for _, policy := range policies {
			if entry, ok := policy.(map[string]interface{}); ok {
				if seconds, ok := entry["periodSeconds"].(int64); ok {
					*period = max(*period, time.Duration(seconds)*time.Second)
				}
			}
		}
	}

	const margin = 15 * time.Second
	p.ScaleUpTotalWait = p.PollingInterval + max(p.ScaleUpStabilization, p.ScaleUpRatePeriod) + margin
	p.ScaleDownTotalWait = p.PollingInterval + max(p.ScaleDownStabilization, p.ScaleDownRatePeriod) + margin
	return p
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

// GetInferenceSetReadyReplicas returns .status.readyReplicas of the named
// InferenceSet; an absent field (omitempty) reads as 0.
func GetInferenceSetReadyReplicas(ctx context.Context, model, namespace string) (int32, error) {
	dynClient, err := GetDynamicClient()
	if err != nil {
		return 0, err
	}
	is, err := dynClient.Resource(InferenceSetGVR).Namespace(namespace).Get(ctx, model, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("get InferenceSet %s/%s: %w", namespace, model, err)
	}
	v, _, err := unstructured.NestedInt64(is.Object, "status", "readyReplicas")
	if err != nil {
		return 0, fmt.Errorf("InferenceSet %s/%s: read .status.readyReplicas: %w", namespace, model, err)
	}
	return int32(v), nil
}

// SetInferenceSetReplicas patches the InferenceSet's .spec.replicas directly.
//
// Reserved for KEDA-managed deployments (EnableScaling), where KEDA owns
// .spec.replicas through the scale subresource. The chart renders the
// `replicas` value, but Helm only patches .spec.replicas when that value
// changes between releases, so UpgradeModelDeployment cannot reliably restore
// the live count; it can only move the min-replicas floor.
// Everything else must change replicas through UpgradeModelDeployment so the
// declared configuration stays the source of truth.
//
// Callers MUST carry GinkgoLabelStandardK8sOnly: a managed cluster such as AKS
// Automatic does not allow the suite to reshape running workloads this way.
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

// The helpers below describe a scale event only through objects that exist
// identically on the GPU node mocker and on real Karpenter-provisioned GPUs:
// the InferenceSet's model pods, the nodes they run on, and the NodeClaims
// whose status.nodeName points at those nodes. On the mocker the model pod's
// IP and Ready condition are patched from its shadow pod, so scraping the
// model pod reaches the simulator.

// DefaultModelServerPort is the vLLM port KAITO serves inference and /metrics
// on; used when a model pod declares no container port.
const DefaultModelServerPort = 5000

// ListModelPods returns the InferenceSet's model pods that are not being
// deleted, sorted by name.
func ListModelPods(ctx context.Context, clientset kubernetes.Interface, values deploy.ModelDeploymentValues) ([]corev1.Pod, error) {
	list, err := clientset.CoreV1().Pods(values.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: values.InferencePodSelector(),
	})
	if err != nil {
		return nil, fmt.Errorf("list model pods for %s/%s: %w", values.Namespace, values.Name, err)
	}
	pods := make([]corev1.Pod, 0, len(list.Items))
	for _, pod := range list.Items {
		if pod.DeletionTimestamp == nil {
			pods = append(pods, pod)
		}
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	return pods, nil
}

// IsPodReady reports whether the pod is Running with Ready=True.
func IsPodReady(pod corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// ReadyPods returns the subset of pods that are Ready.
func ReadyPods(pods []corev1.Pod) []corev1.Pod {
	var ready []corev1.Pod
	for _, pod := range pods {
		if IsPodReady(pod) {
			ready = append(ready, pod)
		}
	}
	return ready
}

// PodPlacements maps pod name to the node it is scheduled on (empty when
// unscheduled).
func PodPlacements(pods []corev1.Pod) map[string]string {
	placements := make(map[string]string, len(pods))
	for _, pod := range pods {
		placements[pod.Name] = pod.Spec.NodeName
	}
	return placements
}

// ModelServerPort returns the first declared container port of the pod, or
// DefaultModelServerPort when none is declared.
func ModelServerPort(pod corev1.Pod) int {
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.ContainerPort > 0 {
				return int(p.ContainerPort)
			}
		}
	}
	return DefaultModelServerPort
}

// SumMetricValues sums every sample of metricName whose labels include the
// given subset. vLLM splits some counters across extra labels (e.g.
// vllm:request_success_total by finished_reason), so the first match alone
// would undercount.
func SumMetricValues(metricsText, metricName string, labels map[string]string) (float64, bool) {
	var (
		sum   float64
		found bool
	)
	for _, line := range strings.Split(metricsText, "\n") {
		line = strings.TrimSpace(line)
		m := metricsLineRegex.FindStringSubmatch(line)
		if m == nil || m[1] != metricName || !labelsMatch(m[2], labels) {
			continue
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		sum += v
		found = true
	}
	return sum, found
}

// ScrapeModelPodMetric scrapes metricName (labelled model_name=<values.Name>)
// from every Ready model pod via the API server pod proxy and returns
// pod name → value. It fails if there is no Ready pod, or if any Ready pod
// cannot be scraped or does not export the metric, so a broken replica cannot
// hide behind a healthy one.
func ScrapeModelPodMetric(ctx context.Context, clientset *kubernetes.Clientset, values deploy.ModelDeploymentValues, metricName string) (PodMetricSnapshot, error) {
	pods, err := ListModelPods(ctx, clientset, values)
	if err != nil {
		return nil, err
	}
	ready := ReadyPods(pods)
	if len(ready) == 0 {
		return nil, fmt.Errorf("no Ready model pods for %s/%s", values.Namespace, values.Name)
	}
	snapshot := make(PodMetricSnapshot, len(ready))
	var missing []string
	for _, pod := range ready {
		raw, err := ScrapePodMetrics(ctx, clientset, values.Namespace, pod.Name, ModelServerPort(pod))
		if err != nil {
			return nil, err
		}
		v, found := SumMetricValues(raw, metricName, map[string]string{"model_name": values.Name})
		if !found {
			missing = append(missing, pod.Name)
			continue
		}
		snapshot[pod.Name] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s{model_name=%q} missing on Ready model pods %v", metricName, values.Name, missing)
	}
	return snapshot, nil
}

// ListNodeClaims returns all Karpenter NodeClaims in the cluster.
func ListNodeClaims(ctx context.Context, dynClient dynamic.Interface) ([]unstructured.Unstructured, error) {
	list, err := dynClient.Resource(NodeClaimGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list NodeClaims: %w", err)
	}
	return list.Items, nil
}

// NodeClaimNames returns the sorted names of the NodeClaims.
func NodeClaimNames(nodeClaims []unstructured.Unstructured) []string {
	names := make([]string, 0, len(nodeClaims))
	for _, nc := range nodeClaims {
		names = append(names, nc.GetName())
	}
	sort.Strings(names)
	return names
}

// NodeClaimsByNode maps status.nodeName → NodeClaim name for the NodeClaims
// that have registered a node. Both the mocker and Karpenter set the field.
func NodeClaimsByNode(nodeClaims []unstructured.Unstructured) map[string]string {
	byNode := make(map[string]string, len(nodeClaims))
	for _, nc := range nodeClaims {
		node, _, _ := unstructured.NestedString(nc.Object, "status", "nodeName")
		if node != "" {
			byNode[node] = nc.GetName()
		}
	}
	return byNode
}

// NewNodeClaimNames returns the sorted names in current that are not in
// before. Callers snapshot before installing a case and use the result to
// find every NodeClaim the case caused, including ones that never registered
// a node.
func NewNodeClaimNames(before []string, current []unstructured.Unstructured) []string {
	seen := make(map[string]struct{}, len(before))
	for _, name := range before {
		seen[name] = struct{}{}
	}
	var added []string
	for _, name := range NodeClaimNames(current) {
		if _, ok := seen[name]; !ok {
			added = append(added, name)
		}
	}
	return added
}

// EnsureConfigMap creates the ConfigMap or replaces its data.
func EnsureConfigMap(ctx context.Context, clientset kubernetes.Interface, namespace, name string, data map[string]string) error {
	configMaps := clientset.CoreV1().ConfigMaps(namespace)
	existing, err := configMaps.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = configMaps.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Data:       data,
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create ConfigMap %s/%s: %w", namespace, name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get ConfigMap %s/%s: %w", namespace, name, err)
	}
	existing.Data = data
	if _, err := configMaps.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update ConfigMap %s/%s: %w", namespace, name, err)
	}
	return nil
}
