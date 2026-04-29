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
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// EPPMetricsPort is the port where the EPP exposes Prometheus metrics.
	EPPMetricsPort = 9090
)

// ScrapePodMetrics fetches the /metrics endpoint from a pod using the
// Kubernetes API server proxy. Returns the raw Prometheus text.
// Retries on transient 503 errors (pod container not yet serving).
func ScrapePodMetrics(ctx context.Context, clientset *kubernetes.Clientset, namespace, podName string, port int) (string, error) {
	const maxRetries = 5
	const retryDelay = 3 * time.Second

	for attempt := 0; attempt <= maxRetries; attempt++ {
		result, err := clientset.CoreV1().RESTClient().Get().
			Namespace(namespace).
			Resource("pods").
			SubResource("proxy").
			Name(fmt.Sprintf("%s:%d", podName, port)).
			Suffix("metrics").
			Do(ctx).
			Raw()
		if err == nil {
			return string(result), nil
		}
		if apierrors.IsServiceUnavailable(err) && attempt < maxRetries {
			time.Sleep(retryDelay)
			continue
		}
		return "", fmt.Errorf("failed to scrape metrics from %s/%s:%d: %w", namespace, podName, port, err)
	}
	return "", fmt.Errorf("failed to scrape metrics from %s/%s:%d after %d retries", namespace, podName, port, maxRetries)
}

// metricsLineRegex matches a Prometheus metric line with optional labels.
// Examples:
//
//	vllm:request_success_total{model_name="falcon"} 42
//	inference_extension_scheduler_attempts_total{status="success"} 10
var metricsLineRegex = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)\{([^}]*)\}\s+(\S+)`)

// metricsLineNoLabels matches a Prometheus metric line without labels.
var metricsLineNoLabels = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)\s+(\S+)$`)

// ParseMetricValue extracts the value of a metric with the given name and
// label subset from raw Prometheus text. If labels is nil, matches any label
// set. Returns 0 and false if not found.
func ParseMetricValue(metricsText, metricName string, labels map[string]string) (float64, bool) {
	for _, line := range strings.Split(metricsText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Try line with labels first.
		if m := metricsLineRegex.FindStringSubmatch(line); m != nil {
			if m[1] != metricName {
				continue
			}
			if labels != nil && !labelsMatch(m[2], labels) {
				continue
			}
			val, err := strconv.ParseFloat(m[3], 64)
			if err != nil {
				continue
			}
			return val, true
		}

		// Try line without labels.
		if labels == nil || len(labels) == 0 {
			if m := metricsLineNoLabels.FindStringSubmatch(line); m != nil {
				if m[1] != metricName {
					continue
				}
				val, err := strconv.ParseFloat(m[2], 64)
				if err != nil {
					continue
				}
				return val, true
			}
		}
	}
	return 0, false
}

// labelsMatch checks whether the raw label string (e.g. `model_name="falcon",status="ok"`)
// contains all the required key=value pairs.
func labelsMatch(rawLabels string, required map[string]string) bool {
	parsed := parseLabels(rawLabels)
	for k, v := range required {
		if parsed[k] != v {
			return false
		}
	}
	return true
}

// parseLabels splits a raw Prometheus label string into a map.
func parseLabels(raw string) map[string]string {
	result := make(map[string]string)
	// Split on commas that are not inside quotes.
	parts := splitLabels(raw)
	for _, part := range parts {
		part = strings.TrimSpace(part)
		eqIdx := strings.Index(part, "=")
		if eqIdx < 0 {
			continue
		}
		key := part[:eqIdx]
		val := strings.Trim(part[eqIdx+1:], "\"")
		result[key] = val
	}
	return result
}

// splitLabels splits a label string by commas, respecting quoted values.
func splitLabels(s string) []string {
	var parts []string
	var current strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '"' {
			inQuote = !inQuote
			current.WriteByte(ch)
		} else if ch == ',' && !inQuote {
			parts = append(parts, current.String())
			current.Reset()
		} else {
			current.WriteByte(ch)
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

// PodMetricSnapshot holds per-pod metric values for a specific metric.
type PodMetricSnapshot map[string]float64

// ScrapeRequestSuccessTotal scrapes vllm:request_success_total from all shadow
// pods for the given model in the given namespace and returns a map of
// podName → counter value.
func ScrapeRequestSuccessTotal(ctx context.Context, clientset *kubernetes.Clientset, namespace, model string) (PodMetricSnapshot, error) {
	pods, err := GetShadowPodsForModel(ctx, clientset, namespace, model)
	if err != nil {
		return nil, err
	}

	snapshot := make(PodMetricSnapshot, len(pods))
	for _, pod := range pods {
		port := inferenceSimPort(pod)
		raw, err := ScrapePodMetrics(ctx, clientset, namespace, pod.Name, port)
		if err != nil {
			return nil, fmt.Errorf("scraping %s: %w", pod.Name, err)
		}
		val, found := ParseMetricValue(raw, "vllm:request_success_total", map[string]string{
			"model_name": model,
		})
		if !found {
			// Counter may not exist yet if no requests have been served.
			val = 0
		}
		snapshot[pod.Name] = val
	}
	return snapshot, nil
}

// GetShadowPodsForModel returns the Running shadow pods in the given
// namespace that serve the given model. Shadow pods are identified by label
// kaito.sh/managed-by=gpu-mocker and annotation or label containing the
// model name.
func GetShadowPodsForModel(ctx context.Context, clientset *kubernetes.Clientset, namespace, model string) ([]corev1.Pod, error) {
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "kaito.sh/managed-by=gpu-mocker",
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return nil, fmt.Errorf("listing shadow pods: %w", err)
	}

	var matched []corev1.Pod
	for _, pod := range pods.Items {
		// Match by checking the shadow-pod-for label or by scraping the
		// model name from the pod's served-model-name argument.
		if belongsToModel(pod, model) {
			matched = append(matched, pod)
		}
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("no running shadow pods found for model %q in %s", model, namespace)
	}
	return matched, nil
}

// belongsToModel checks whether a shadow pod serves the given model by
// inspecting its labels/annotations or container args.
func belongsToModel(pod corev1.Pod, model string) bool {
	// Check if the shadow-pod-for label references a pod whose name contains the model.
	if ref, ok := pod.Labels["kaito.sh/shadow-pod-for"]; ok {
		if strings.Contains(ref, model) {
			return true
		}
	}

	// Check container args for --served-model-name.
	for _, c := range pod.Spec.Containers {
		if c.Name == "llm-d-inference-sim" {
			for i, arg := range c.Args {
				if arg == "--served-model-name" && i+1 < len(c.Args) && c.Args[i+1] == model {
					return true
				}
				if strings.HasPrefix(arg, "--served-model-name=") && strings.TrimPrefix(arg, "--served-model-name=") == model {
					return true
				}
			}
		}
	}
	return false
}

// DiffSnapshots returns the per-pod delta between two snapshots (after - before).
func DiffSnapshots(before, after PodMetricSnapshot) PodMetricSnapshot {
	diff := make(PodMetricSnapshot, len(after))
	for pod, afterVal := range after {
		beforeVal := before[pod]
		diff[pod] = afterVal - beforeVal
	}
	return diff
}

// TotalDelta returns the sum of all deltas in a diff snapshot.
func TotalDelta(diff PodMetricSnapshot) float64 {
	var total float64
	for _, v := range diff {
		total += v
	}
	return total
}

// ScrapeModelMetric scrapes a named metric with a model_name label from all
// shadow pods in the given namespace for the given model and returns a
// per-pod snapshot. This is used for metrics like vllm:prefix_cache_hits,
// vllm:prefix_cache_queries, etc.
func ScrapeModelMetric(ctx context.Context, clientset *kubernetes.Clientset, namespace, model, metricName string) (PodMetricSnapshot, error) {
	pods, err := GetShadowPodsForModel(ctx, clientset, namespace, model)
	if err != nil {
		return nil, err
	}

	snapshot := make(PodMetricSnapshot, len(pods))
	for _, pod := range pods {
		port := inferenceSimPort(pod)
		raw, err := ScrapePodMetrics(ctx, clientset, namespace, pod.Name, port)
		if err != nil {
			return nil, fmt.Errorf("scraping %s: %w", pod.Name, err)
		}
		val, _ := ParseMetricValue(raw, metricName, map[string]string{
			"model_name": model,
		})
		snapshot[pod.Name] = val
	}
	return snapshot, nil
}

// inferenceSimPort returns the llm-d-inference-sim container's port from the
// pod spec. The simulator serves both the API and /metrics on the same port.
// Falls back to 8000 if no port is declared.
func inferenceSimPort(pod corev1.Pod) int {
	for _, c := range pod.Spec.Containers {
		if c.Name == "llm-d-inference-sim" {
			for _, p := range c.Ports {
				if p.ContainerPort > 0 {
					return int(p.ContainerPort)
				}
			}
		}
	}
	return 8000
}

// GetEPPPods returns the EPP pods for the given deployment.
func GetEPPPods(ctx context.Context, clientset *kubernetes.Clientset, deploymentName, namespace string) ([]corev1.Pod, error) {
	eppName := EPPServiceName(deploymentName)
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("inferencepool=%s", eppName),
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return nil, fmt.Errorf("listing EPP pods for %s: %w", deploymentName, err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no running EPP pods found for %s", deploymentName)
	}
	return pods.Items, nil
}

// ScrapeEPPMetric scrapes a single metric from the EPP pod(s) for a deployment
// and returns the value. If multiple EPP pods exist, sums across them.
func ScrapeEPPMetric(ctx context.Context, clientset *kubernetes.Clientset, deploymentName, namespace, metricName string, labels map[string]string) (float64, error) {
	pods, err := GetEPPPods(ctx, clientset, deploymentName, namespace)
	if err != nil {
		return 0, err
	}

	var total float64
	for _, pod := range pods {
		raw, err := ScrapePodMetrics(ctx, clientset, namespace, pod.Name, EPPMetricsPort)
		if err != nil {
			return 0, fmt.Errorf("scraping EPP pod %s: %w", pod.Name, err)
		}
		val, found := ParseMetricValue(raw, metricName, labels)
		if found {
			total += val
		}
	}
	return total, nil
}
