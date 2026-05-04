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
	// ScalingThreshold is the queue depth threshold. Only used when
	// EnableScaling is true.
	ScalingThreshold int64
	// AuthAPIKeyEnabled signals that this deployment runs behind the
	// apikey-ext-authz CUSTOM provider. The per-namespace
	// AuthorizationPolicy and APIKey CR are provisioned by
	// EnsureNamespace; the warmup loop in SetupInferenceSetsWithRouting
	// reads the resulting Secret and sends Bearer + Host headers.
	AuthAPIKeyEnabled bool
	// NetworkPolicyEnabled signals that this deployment's namespace
	// should be locked down with the default-deny + allow-inference
	// NetworkPolicy pair (provisioned by EnsureNamespace). All
	// deployments in a case share a namespace, so the value on the first
	// deployment is what takes effect.
	NetworkPolicyEnabled bool
	// NetworkPolicyAllowedNamespaces lists namespaces (matched by the
	// standard `kubernetes.io/metadata.name` label) that are granted
	// cross-namespace ingress to non-gateway pods when
	// NetworkPolicyEnabled is true. Use this to permit control-plane
	// scrapers — e.g. `keda-kaito-scaler` in `keda` needs to reach
	// vLLM metrics on shadow pods to drive autoscaling decisions.
	// Leave nil/empty to keep the namespace strictly isolated (the
	// default for the network-policy e2e cases).
	NetworkPolicyAllowedNamespaces []string
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
		if v.ScalingThreshold > 0 {
			args = append(args, "--set", "scalingThreshold="+strconv.FormatInt(v.ScalingThreshold, 10))
		}
	}
	return args
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
// "<namespace>-gw" by chart default), the catch-all model-not-found
// HTTPRoute + ReferenceGrant, and — when authEnabled is true — the
// per-namespace AuthorizationPolicy + APIKey CR. When
// networkPolicyEnabled is true, the chart additionally renders the
// default-deny-ingress / allow-inference-traffic NetworkPolicies that
// lock down East-West ingress while keeping the gateway pod reachable;
// `allowedIngressNamespaces` (if non-empty) extends
// `allow-inference-traffic` with cross-namespace ingress for the named
// namespaces (matched by the `kubernetes.io/metadata.name` label).
//
// Idempotent: re-running on an existing release reconciles the values.
func InstallModelHarness(namespace string, authEnabled, networkPolicyEnabled bool, allowedIngressNamespaces []string) error {
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
		"--set", "networkPolicy.enabled=" + strconv.FormatBool(networkPolicyEnabled),
	}
	for i, allowed := range allowedIngressNamespaces {
		args = append(args, "--set",
			fmt.Sprintf("networkPolicy.allowedIngressNamespaces[%d]=%s", i, allowed))
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
