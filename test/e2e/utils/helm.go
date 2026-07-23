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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// ModelDeploymentChartPath is the relative path (from the repo root, where
// `go test` is invoked by `make test-e2e`) to the modeldeployment Helm chart.
// It can be overridden via the MODELDEPLOYMENT_CHART env var to support
// running tests from other working directories.
const defaultModelDeploymentChartPath = "charts/modeldeployment"

// ModelDeploymentValues holds the subset of `charts/modeldeployment/values.yaml`
// inputs that E2E test cases need to configure.
type ModelDeploymentValues struct {
	// Name is the helm release name. Used as the InferenceSet name and as
	// the X-Gateway-Model-Name header value matched by the HTTPRoute.
	Name string
	// Namespace is the target namespace for the helm release.
	Namespace string
	// Model is the inference preset name (spec.template.inference.preset.name).
	// Defaults to Name when empty.
	Model string
	// Replicas is the desired number of InferenceSet replicas.
	Replicas int64
	// InstanceType is the VM instance type. Defaults to chart default when empty.
	InstanceType string
	// EnableScaling toggles scaledobject.kaito.sh/* annotations.
	EnableScaling bool
	// MaxReplicas is the upper bound for autoscaling. Only used when
	// EnableScaling is true.
	MaxReplicas int64
	// ScalingMetrics is the ordered list of composite scaling signals wired
	// onto the modeldeployment chart's scaling.metrics[<i>] entries. Only
	// used when EnableScaling is true; at least one entry is required in
	// that case (the chart rejects an empty metrics list). Each entry's
	// UpThreshold MUST be strictly greater than its DownThreshold.
	ScalingMetrics []ScalingMetric
	// AuthAPIKeyEnabled signals that this deployment runs behind the
	// apikey-ext-authz CUSTOM provider. The per-namespace
	// AuthorizationPolicy and APIKey CR are provisioned by
	// EnsureNamespace; the warmup loop in SetupInferenceSetsWithRouting
	// reads the resulting Secret and sends Bearer + Host headers.
	AuthAPIKeyEnabled bool
	// AutoUpgrade opts the InferenceSet into KAITO automatic base image
	// upgrades, wired onto the modeldeployment chart's autoUpgrade.* values
	// (rendered as spec.autoUpgrade). Only rendered when Enabled is true.
	AutoUpgrade AutoUpgrade
}

// AutoUpgrade mirrors the modeldeployment chart's autoUpgrade values, wired
// onto the InferenceSet's spec.autoUpgrade. Consumed only when Enabled is true.
type AutoUpgrade struct {
	// Enabled toggles autoUpgrade.enabled (spec.autoUpgrade.enabled).
	Enabled bool
	// MaintenanceWindowSchedule is the 5-field cron (UTC) marking when
	// rollouts may begin (autoUpgrade.maintenanceWindow.schedule). Empty
	// omits the maintenanceWindow block entirely.
	MaintenanceWindowSchedule string
	// MaintenanceWindowDuration is how long the window stays open, e.g. "4h"
	// (autoUpgrade.maintenanceWindow.duration). Ignored when
	// MaintenanceWindowSchedule is empty.
	MaintenanceWindowDuration string
}

// ScalingMetric describes one composite scaling signal, mirroring a single
// entry of the modeldeployment chart's scaling.metrics list. Each field maps
// 1:1 to a field of an entry in the scaledobject.kaito.sh/metrics YAML list
// annotation the chart renders (keda-kaito-scaler v0.6.2+).
type ScalingMetric struct {
	// Name is the Prometheus metric family name (metrics entry `name`).
	// Required.
	Name string
	// Type is the aggregation applied to the metric: "gauge" (per-replica
	// average) or "histogram" (per-pod windowed average) (metrics entry
	// `type`). Empty defaults to gauge.
	Type string
	// UpThreshold is the per-replica scale-up threshold (metrics entry
	// `upthreshold`). Required; MUST be strictly greater than DownThreshold.
	UpThreshold string
	// DownThreshold is the per-replica scale-down threshold (metrics entry
	// `downthreshold`). Required; MUST be strictly less than UpThreshold.
	DownThreshold string
	// MetricCacheWindow is the rolling cache window in seconds for histogram
	// metrics (metrics entry `metriccachewindow`). Optional; ignored for gauge.
	MetricCacheWindow string
}

// DefaultModelDeploymentValues returns a populated ModelDeploymentValues for a
// stateful, 2-replica steady-state deployment in the default namespace.
func DefaultModelDeploymentValues(name string) ModelDeploymentValues {
	return ModelDeploymentValues{
		Name:      name,
		Namespace: "default",
		Model:     name,
		Replicas:  2,
	}
}

// modelDeploymentChartPath resolves the chart path, honoring MODELDEPLOYMENT_CHART.
func modelDeploymentChartPath() string {
	if p := os.Getenv("MODELDEPLOYMENT_CHART"); p != "" {
		return p
	}
	return defaultModelDeploymentChartPath
}

// helmSetArgs builds the `--set key=value` arguments for the modeldeployment chart.
func (v ModelDeploymentValues) helmSetArgs() []string {
	args := []string{
		"--set", "name=" + v.Name,
		"--set", "namespace=" + v.Namespace,
		"--set", "model=" + v.Model,
	}
	if v.Replicas > 0 {
		args = append(args, "--set", "replicas="+strconv.FormatInt(v.Replicas, 10))
	}
	if v.InstanceType != "" {
		args = append(args, "--set", "instanceType="+v.InstanceType)
	}
	if v.EnableScaling {
		args = append(args, "--set", "enableScaling=true")
		if v.MaxReplicas > 0 {
			args = append(args, "--set", "maxReplicas="+strconv.FormatInt(v.MaxReplicas, 10))
		}
		for i, m := range v.ScalingMetrics {
			prefix := fmt.Sprintf("scaling.metrics[%d].", i)
			args = append(args, "--set", prefix+"name="+m.Name)
			if m.Type != "" {
				args = append(args, "--set", prefix+"type="+m.Type)
			}
			args = append(args,
				"--set", prefix+"upThreshold="+m.UpThreshold,
				"--set", prefix+"downThreshold="+m.DownThreshold,
			)
			if m.MetricCacheWindow != "" {
				args = append(args, "--set", prefix+"metricCacheWindow="+m.MetricCacheWindow)
			}
		}
	}
	if v.AutoUpgrade.Enabled {
		args = append(args, "--set", "autoUpgrade.enabled=true")
		if v.AutoUpgrade.MaintenanceWindowSchedule != "" {
			// Use --set-string: the cron schedule contains spaces and
			// asterisks that must be passed through verbatim.
			args = append(args, "--set-string",
				"autoUpgrade.maintenanceWindow.schedule="+v.AutoUpgrade.MaintenanceWindowSchedule)
			if v.AutoUpgrade.MaintenanceWindowDuration != "" {
				args = append(args, "--set-string",
					"autoUpgrade.maintenanceWindow.duration="+v.AutoUpgrade.MaintenanceWindowDuration)
			}
		}
	}
	return args
}

// InferencePodSelector returns the label selector that finds the model-serving
// pods for this deployment.
func (v ModelDeploymentValues) InferencePodSelector() string {
	return "inferenceset.kaito.sh/created-by=" + v.Name
}

// InstallModelDeployment runs `helm upgrade --install` for the modeldeployment
// chart with the supplied values. It is idempotent — re-running it on an
// existing release reconciles to the new values. The release name equals
// values.Name and the release namespace equals values.Namespace.
//
// Both Name (deployment name) and Model (preset / X-Gateway-Model-Name match
// value) are required.
func InstallModelDeployment(values ModelDeploymentValues) error {
	if values.Name == "" {
		return fmt.Errorf("modeldeployment: Name (deployment name) is required")
	}
	if values.Model == "" {
		return fmt.Errorf("modeldeployment %q: Model is required (must be set explicitly, not derived from Name)", values.Name)
	}
	chart := modelDeploymentChartPath()
	if _, err := os.Stat(chart); err != nil {
		// Try one path level up in case tests are invoked from test/e2e.
		alt := filepath.Join("..", "..", chart)
		if _, err2 := os.Stat(alt); err2 == nil {
			chart = alt
		} else {
			return fmt.Errorf("modeldeployment chart not found at %q (set MODELDEPLOYMENT_CHART): %w", chart, err)
		}
	}

	args := []string{
		"upgrade", "--install", values.Name, chart,
		"--namespace", values.Namespace,
		"--create-namespace",
	}
	args = append(args, values.helmSetArgs()...)

	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("helm upgrade --install %s failed: %w\n%s", values.Name, err, string(out))
	}
	return nil
}

// UninstallModelDeployment runs `helm uninstall` for the named release. Missing
// releases are treated as success (so cleanup is idempotent).
func UninstallModelDeployment(name, namespace string) error {
	args := []string{"uninstall", name, "--namespace", namespace, "--ignore-not-found", "--wait"}
	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("helm uninstall %s failed: %w\n%s", name, err, string(out))
	}
	return nil
}

// ModelHarnessReleaseName is the helm release name used by InstallModelHarness
// / UninstallModelHarness. Each workload namespace owns exactly one release.
const ModelHarnessReleaseName = "modelharness"

// defaultModelHarnessChartPath is the relative path (from the repo root,
// where `go test` is invoked by `make test-e2e`) to the modelharness Helm
// chart. Override via the MODELHARNESS_CHART env var.
const defaultModelHarnessChartPath = "charts/modelharness"

func modelHarnessChartPath() string {
	if p := os.Getenv("MODELHARNESS_CHART"); p != "" {
		return p
	}
	return defaultModelHarnessChartPath
}

// InstallModelHarness runs `helm upgrade --install` for the modelharness chart
// in `namespace`. It provisions the per-namespace Gateway (named
// "<namespace>-gw" by chart default), the catch-all
// `model-not-found-direct` EnvoyFilter (Envoy `direct_response` returning
// 404 + OpenAI-compatible JSON), and — when authEnabled is true — the
// per-namespace AuthorizationPolicy + APIKey CR. When
// networkPolicyEnabled is true, the chart additionally renders the
// default-deny-ingress / allow-inference-traffic NetworkPolicies that
// lock down East-West ingress while keeping the gateway pod reachable;
// the chart-default `allowedIngressNamespaces` (currently
// keda + kaito-system) covers the control-plane scrapers every workload
// in this repo needs.
//
// Idempotent: re-running on an existing release reconciles the values.
func InstallModelHarness(namespace string, authEnabled bool) error {
	if namespace == "" {
		return fmt.Errorf("modelharness: namespace is required")
	}
	chart := modelHarnessChartPath()
	if _, err := os.Stat(chart); err != nil {
		alt := filepath.Join("..", "..", chart)
		if _, err2 := os.Stat(alt); err2 == nil {
			chart = alt
		} else {
			return fmt.Errorf("modelharness chart not found at %q (set MODELHARNESS_CHART): %w", chart, err)
		}
	}

	args := []string{
		"upgrade", "--install", ModelHarnessReleaseName, chart,
		"--namespace", namespace,
		"--create-namespace",
		"--set", "namespace=" + namespace,
		"--set", "auth.enabled=" + strconv.FormatBool(authEnabled),
	}
	args = append(args, "--wait")

	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("helm upgrade --install %s in %s failed: %w\n%s",
			ModelHarnessReleaseName, namespace, err, string(out))
	}
	return nil
}

// UninstallModelHarness runs `helm uninstall` for the modelharness release in
// `namespace`. Missing releases are treated as success.
func UninstallModelHarness(namespace string) error {
	args := []string{"uninstall", ModelHarnessReleaseName,
		"--namespace", namespace, "--ignore-not-found", "--wait"}
	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("helm uninstall %s in %s failed: %w\n%s",
			ModelHarnessReleaseName, namespace, err, string(out))
	}
	return nil
}
