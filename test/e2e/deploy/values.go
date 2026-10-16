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

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

var corsOriginPattern = regexp.MustCompile(`^https?://(\[[0-9a-f:.]+\]|[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*)(:(0|[1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5]))?$`)

func isValidExactCORSOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || !corsOriginPattern.MatchString(origin) || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}

	port := parsed.Port()
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber > 65535 || strconv.Itoa(portNumber) != port {
			return false
		}
		if (parsed.Scheme == "http" && portNumber == 80) || (parsed.Scheme == "https" && portNumber == 443) {
			return false
		}
	}

	return true
}

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
	// CORS configures browser cross-origin access to the Gateway's /v1 API.
	CORS CORSValues
	// Gateway overrides the chart's gateway provider and public domain.
	Gateway GatewayValues
}

// CORSValues configures browser cross-origin access for a modelharness.
// Chart defaults supply methods, headers, and preflight max age. A nil
// AllowCredentials inherits the chart default; wildcard mode requires an
// explicit false value.
type CORSValues struct {
	Enabled          bool
	AllowedOrigins   []string
	AllowCredentials *bool
}

// GatewayValues configures charts for an externally managed GatewayClass.
type GatewayValues struct {
	CloudProvider    string
	GatewayClassName string
	DefaultDomain    string
}

// Validate reports whether the values describe a well-formed modelharness.
func (v ModelHarnessValues) Validate() error {
	if v.Namespace == "" {
		return fmt.Errorf("modelharness: Namespace is required")
	}
	if !v.CORS.Enabled {
		return nil
	}
	if len(v.CORS.AllowedOrigins) == 0 {
		return fmt.Errorf("modelharness: CORS.AllowedOrigins must contain at least one origin when CORS.Enabled is true")
	}

	for _, origin := range v.CORS.AllowedOrigins {
		if origin != "*" {
			continue
		}
		if len(v.CORS.AllowedOrigins) != 1 {
			return fmt.Errorf("modelharness: CORS.AllowedOrigins wildcard must be the sole configured origin")
		}
		if v.CORS.AllowCredentials == nil || *v.CORS.AllowCredentials {
			return fmt.Errorf("modelharness: CORS.AllowCredentials must be explicitly false when CORS.AllowedOrigins contains wildcard")
		}
		return nil
	}

	seen := make(map[string]struct{}, len(v.CORS.AllowedOrigins))
	for i, origin := range v.CORS.AllowedOrigins {
		if !isValidExactCORSOrigin(origin) {
			return fmt.Errorf("modelharness: CORS.AllowedOrigins[%d] must be an exact http(s) origin in canonical browser form, with a lowercase host and non-default port in the range 0-65535, without wildcard, credentials, path, query, fragment, or trailing slash: %q", i, origin)
		}
		if _, exists := seen[origin]; exists {
			return fmt.Errorf("modelharness: CORS.AllowedOrigins contains duplicate origin %q", origin)
		}
		seen[origin] = struct{}{}
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
	// Config is the name of a ConfigMap containing custom inference arguments
	// (spec.template.inference.config).
	Config string
	// Adapters are vLLM LoRA adapters rendered under
	// spec.template.inference.adapters. Their names are additional model IDs.
	Adapters []LoRAAdapter
	// Replicas is the desired number of InferenceSet replicas in manual mode.
	// Rendered only when EnableScaling is false, where 0 is an explicit
	// scale-to-zero rather than "unset" — UpgradeModelDeployment relies on
	// that to empty an inference pool. Ignored under autoscaling, so KEDA
	// owns the live count.
	Replicas int64
	// InstanceType is the VM instance type. Defaults to the backend default
	// when empty.
	InstanceType string
	// EnableScaling toggles scaledobject.kaito.sh/* annotations.
	EnableScaling bool
	// MinReplicas is the lower bound for autoscaling (chart minReplicas).
	// Always rendered when EnableScaling is true, so 0 is an explicit
	// scale-to-zero rather than "unset".
	MinReplicas int64
	// MaxReplicas is the upper bound for autoscaling (chart maxReplicas).
	// Required (>= 1 and >= MinReplicas) when EnableScaling is true.
	MaxReplicas int64
	// ScalingMetrics is the ordered list of composite scaling signals wired
	// onto the modeldeployment chart's scaling.metrics[<i>] entries. Only
	// used when EnableScaling is true. Empty inherits the chart's default
	// scale-to-zero and positive-replica metric policy. Each custom entry's
	// UpThreshold MUST be strictly greater than its DownThreshold.
	ScalingMetrics []ScalingMetric
	// CooldownPeriod is KEDA's idle cooldown in seconds before parking at
	// zero. Only valid with MinReplicas=0; zero keeps the chart default.
	CooldownPeriod int64
	// Nil inherits scaler defaults; pointers allow an explicit zero.
	InitialCooldownPeriod        *int64
	ScaleDownStabilizationWindow *int64
	ScaleUpPeriodSeconds         *int64
	ScaleDownPeriodSeconds       *int64
	// DisableBenchmark sets enableBenchmark=false so KAITO skips its
	// post-load benchmark. Zero value keeps the chart default (enabled).
	DisableBenchmark bool
	// AutoUpgrade opts the InferenceSet into KAITO automatic base image
	// upgrades, wired onto the modeldeployment chart's autoUpgrade.* values
	// (rendered as spec.autoUpgrade). Only rendered when Enabled is true.
	AutoUpgrade AutoUpgrade
	// EPPScorerWeights overrides the EPP EndpointPickerConfig plugin weights
	// for this deployment. Nil fields fall through to chart defaults
	// (queue=3, kvCacheUtilization=2, prefixCache=1).
	EPPScorerWeights *EPPScorerWeights
	// Gateway overrides the chart's gateway provider and public domain.
	Gateway GatewayValues
}

// LoRAAdapter describes one vLLM adapter source. Exactly one of Image and
// PersistentVolumeClaim must be set.
type LoRAAdapter struct {
	// Name is the client-visible adapter model ID.
	Name string
	// Image is an adapter image whose PEFT files are stored under /data.
	Image string
	// ImagePullSecrets contains Secret names used to pull Image.
	ImagePullSecrets []string
	// PersistentVolumeClaim names a PVC containing the adapter's PEFT files.
	PersistentVolumeClaim string
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
	// Source selects the metric endpoint: "modelpod" or "epp". Empty
	// preserves the scaler default.
	Source string
	// UpThreshold is the per-replica scale-up threshold (metrics entry
	// `upthreshold`). Required; MUST be strictly greater than DownThreshold.
	UpThreshold string
	// DownThreshold is the per-replica scale-down threshold (metrics entry
	// `downthreshold`). Required; MUST be strictly less than UpThreshold.
	DownThreshold string
	// ActivationThreshold is the EPP threshold that wakes a zero-replica
	// deployment. It requires DeactivationThreshold and Source="epp".
	ActivationThreshold string
	// DeactivationThreshold is the zero-edge inactive threshold. Model-pod
	// metrics may use it without an activation threshold.
	DeactivationThreshold string
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
	if v.Replicas < 0 {
		return fmt.Errorf("modeldeployment %q: Replicas must not be negative (got %d)", v.Name, v.Replicas)
	}
	adapterNames := make(map[string]struct{}, len(v.Adapters))
	for i, adapter := range v.Adapters {
		if errs := validation.IsDNS1123Subdomain(adapter.Name); len(errs) > 0 {
			return fmt.Errorf("modeldeployment %q: Adapters[%d].Name must be a DNS-1123 subdomain: %s",
				v.Name, i, strings.Join(errs, ", "))
		}
		if adapter.Name == v.Name {
			return fmt.Errorf("modeldeployment %q: Adapters[%d].Name must differ from the deployment name", v.Name, i)
		}
		if _, exists := adapterNames[adapter.Name]; exists {
			return fmt.Errorf("modeldeployment %q: Adapters[%d].Name %q is duplicated", v.Name, i, adapter.Name)
		}
		adapterNames[adapter.Name] = struct{}{}
		if (adapter.Image == "") == (adapter.PersistentVolumeClaim == "") {
			return fmt.Errorf("modeldeployment %q: Adapters[%d] must set exactly one of Image or PersistentVolumeClaim", v.Name, i)
		}
		if adapter.Image == "" && len(adapter.ImagePullSecrets) > 0 {
			return fmt.Errorf("modeldeployment %q: Adapters[%d].ImagePullSecrets requires Image", v.Name, i)
		}
		for j, secret := range adapter.ImagePullSecrets {
			if secret == "" {
				return fmt.Errorf("modeldeployment %q: Adapters[%d].ImagePullSecrets[%d] must not be empty", v.Name, i, j)
			}
		}
	}
	if !v.EnableScaling {
		return nil
	}
	if v.MinReplicas < 0 {
		return fmt.Errorf("modeldeployment %q: MinReplicas must not be negative (got %d)", v.Name, v.MinReplicas)
	}
	if v.MaxReplicas < 1 {
		return fmt.Errorf("modeldeployment %q: EnableScaling requires MaxReplicas >= 1 (got %d)", v.Name, v.MaxReplicas)
	}
	if v.MaxReplicas < v.MinReplicas {
		return fmt.Errorf("modeldeployment %q: MaxReplicas (%d) must be >= MinReplicas (%d)", v.Name, v.MaxReplicas, v.MinReplicas)
	}
	// The chart silently ignores cooldownPeriod unless minReplicas is 0.
	if v.MinReplicas > 0 && v.CooldownPeriod > 0 {
		return fmt.Errorf("modeldeployment %q: CooldownPeriod requires MinReplicas=0", v.Name)
	}
	if v.InitialCooldownPeriod != nil {
		if v.MinReplicas != 0 {
			return fmt.Errorf("modeldeployment %q: InitialCooldownPeriod requires MinReplicas=0", v.Name)
		}
		if *v.InitialCooldownPeriod < 0 || *v.InitialCooldownPeriod > 2147483647 {
			return fmt.Errorf("modeldeployment %q: InitialCooldownPeriod must be between 0 and 2147483647", v.Name)
		}
	}
	if v.ScaleDownStabilizationWindow != nil && (*v.ScaleDownStabilizationWindow < 0 || *v.ScaleDownStabilizationWindow > 3600) {
		return fmt.Errorf("modeldeployment %q: ScaleDownStabilizationWindow must be between 0 and 3600", v.Name)
	}
	for name, period := range map[string]*int64{"ScaleUpPeriodSeconds": v.ScaleUpPeriodSeconds, "ScaleDownPeriodSeconds": v.ScaleDownPeriodSeconds} {
		if period != nil && (*period < 1 || *period > 1800) {
			return fmt.Errorf("modeldeployment %q: %s must be between 1 and 1800", v.Name, name)
		}
	}
	if len(v.ScalingMetrics) == 0 {
		return nil
	}
	hasActivationMetric := false
	hasModelPodDeactivationMetric := false
	hasScalingMetric := false
	for i, m := range v.ScalingMetrics {
		if m.Name == "" {
			return fmt.Errorf("modeldeployment %q: ScalingMetrics[%d].Name is required", v.Name, i)
		}
		hasUp := m.UpThreshold != ""
		hasDown := m.DownThreshold != ""
		if hasUp != hasDown {
			return fmt.Errorf("modeldeployment %q: ScalingMetrics[%d] (%s) UpThreshold and DownThreshold must be supplied together", v.Name, i, m.Name)
		}
		if m.ActivationThreshold != "" {
			if m.DeactivationThreshold == "" {
				return fmt.Errorf("modeldeployment %q: ScalingMetrics[%d] (%s) ActivationThreshold requires DeactivationThreshold", v.Name, i, m.Name)
			}
			if m.Source != "epp" {
				return fmt.Errorf("modeldeployment %q: ScalingMetrics[%d] (%s) ActivationThreshold requires Source=epp", v.Name, i, m.Name)
			}
			hasActivationMetric = true
		}
		if !hasUp && m.ActivationThreshold == "" && m.DeactivationThreshold == "" {
			return fmt.Errorf("modeldeployment %q: ScalingMetrics[%d] (%s) requires UpThreshold/DownThreshold, ActivationThreshold or DeactivationThreshold", v.Name, i, m.Name)
		}
		if hasUp {
			hasScalingMetric = true
		}
		if m.Source == "modelpod" && m.DeactivationThreshold != "" {
			hasModelPodDeactivationMetric = true
		}
	}
	// The chart drops zero-edge-only metrics when MinReplicas > 0.
	if v.MinReplicas > 0 && !hasScalingMetric {
		return fmt.Errorf("modeldeployment %q: MinReplicas > 0 requires at least one metric with UpThreshold/DownThreshold", v.Name)
	}
	if v.MinReplicas == 0 && !hasActivationMetric {
		return fmt.Errorf("modeldeployment %q: scale-to-zero requires at least one EPP activation metric", v.Name)
	}
	if v.MinReplicas == 0 && !hasModelPodDeactivationMetric {
		return fmt.Errorf("modeldeployment %q: scale-to-zero requires at least one modelpod deactivation metric", v.Name)
	}
	return nil
}

// InferencePodSelector returns the label selector that finds the model-serving
// pods for this deployment.
func (v ModelDeploymentValues) InferencePodSelector() string {
	return "inferenceset.kaito.sh/created-by=" + v.Name
}
