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

package deploy

import "fmt"

// ModelHarnessValues holds the inputs describing one workload namespace's
// modelharness. Each workload namespace owns exactly one modelharness.
type ModelHarnessValues struct {
	// Namespace is the workload namespace the modelharness is installed
	// into. Required.
	Namespace string
	// AuthEnabled provisions the per-namespace AuthorizationPolicy and
	// APIKey CR that wire the Gateway into the cluster-wide
	// apikey-ext-authz CUSTOM provider.
	AuthEnabled bool
}

// Validate reports whether the values describe a well-formed modelharness.
func (v ModelHarnessValues) Validate() error {
	if v.Namespace == "" {
		return fmt.Errorf("modelharness: Namespace is required")
	}
	return nil
}

// ModelDeploymentValues holds the subset of `charts/modeldeployment/values.yaml`
// inputs that E2E test cases need to configure.
type ModelDeploymentValues struct {
	// Name is the deployment name. Used as the InferenceSet name and as
	// the X-Gateway-Model-Name header value matched by the HTTPRoute.
	Name string
	// Namespace is the target namespace for the deployment.
	Namespace string
	// Model is the inference preset name (spec.template.inference.preset.name).
	Model string
	// Replicas is the desired number of InferenceSet replicas.
	Replicas int64
	// InstanceType is the VM instance type. Defaults to the backend default
	// when empty.
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
	// EPPScorerWeights overrides the EPP EndpointPickerConfig plugin weights
	// for this deployment. Nil fields fall through to chart defaults
	// (queue=3, kvCacheUtilization=2, prefixCache=1).
	EPPScorerWeights *EPPScorerWeights
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

// EPPScorerWeights allows per-deployment override of the EPP
// EndpointPickerConfig scorer weights. A nil pointer means "use chart
// defaults"; individual zero-valued fields ARE rendered (weight 0 is valid).
type EPPScorerWeights struct {
	Queue              *int
	KVCacheUtilization *int
	PrefixCache        *int
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

// Validate reports whether the values describe a well-formed model deployment.
func (v ModelDeploymentValues) Validate() error {
	if v.Name == "" {
		return fmt.Errorf("modeldeployment: Name (deployment name) is required")
	}
	if v.Model == "" {
		return fmt.Errorf("modeldeployment %q: Model is required (must be set explicitly, not derived from Name)", v.Name)
	}
	if !v.EnableScaling {
		return nil
	}
	if len(v.ScalingMetrics) == 0 {
		return fmt.Errorf("modeldeployment %q: EnableScaling requires at least one ScalingMetric", v.Name)
	}
	for i, m := range v.ScalingMetrics {
		if m.Name == "" {
			return fmt.Errorf("modeldeployment %q: ScalingMetrics[%d].Name is required", v.Name, i)
		}
		if m.UpThreshold == "" {
			return fmt.Errorf("modeldeployment %q: ScalingMetrics[%d] (%s) UpThreshold is required", v.Name, i, m.Name)
		}
		if m.DownThreshold == "" {
			return fmt.Errorf("modeldeployment %q: ScalingMetrics[%d] (%s) DownThreshold is required", v.Name, i, m.Name)
		}
	}
	return nil
}

// InferencePodSelector returns the label selector that finds the model-serving
// pods for this deployment.
func (v ModelDeploymentValues) InferencePodSelector() string {
	return "inferenceset.kaito.sh/created-by=" + v.Name
}
