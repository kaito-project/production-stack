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

// Package helm implements deploy.Deployer on top of the Helm CLI. It is the
// default E2E backend and the only one that knows about local chart bytes.
package helm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
)

// BackendName is the name this backend is registered under.
const BackendName = "helm"

// ModelHarnessReleaseName is the release name used for the modelharness.
// Each workload namespace owns exactly one release.
const ModelHarnessReleaseName = "modelharness"

const (
	// defaultModelDeploymentChartPath is the path (relative to the repo
	// root, where `go test` is invoked by `make test-e2e`) to the
	// modeldeployment chart. Override via EnvModelDeploymentChart.
	defaultModelDeploymentChartPath = "charts/modeldeployment"

	// defaultModelHarnessChartPath is the path to the modelharness chart.
	// Override via EnvModelHarnessChart.
	defaultModelHarnessChartPath = "charts/modelharness"

	// EnvModelDeploymentChart overrides the modeldeployment chart path to
	// support running tests from other working directories.
	EnvModelDeploymentChart = "MODELDEPLOYMENT_CHART"

	// EnvModelHarnessChart overrides the modelharness chart path.
	EnvModelHarnessChart = "MODELHARNESS_CHART"

	// namespaceDiscoveryLabel / namespaceDiscoveryValue mark a workload
	// namespace as production-stack managed. The productionstack-status-reporter
	// selects on it, and the models API refuses namespaces without it. The chart
	// cannot stamp it here: when the release namespace IS the workload namespace,
	// Helm owns that Namespace and templating it would collide with Helm's
	// ownership metadata (see charts/modelharness/templates/namespace.yaml).
	namespaceDiscoveryLabel = "productionstack.kaito.sh/managed-by"
	namespaceDiscoveryValue = "modelharness"
)

func init() {
	deploy.Register(BackendName, func() (deploy.Deployer, error) { return New(Options{}) })
}

// Runner executes a CLI invocation and returns its combined output. It is
// injectable so unit tests can assert argument mapping without the binaries.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// Options configures the Helm backend. The zero value resolves chart paths
// from the environment and shells out to the real helm binary.
type Options struct {
	// ModelHarnessChart overrides the modelharness chart path. Empty falls
	// back to EnvModelHarnessChart and then the repo-relative default.
	ModelHarnessChart string
	// ModelDeploymentChart overrides the modeldeployment chart path.
	ModelDeploymentChart string
	// Runner overrides helm execution. Empty uses the helm binary on PATH.
	Runner Runner
	// KubectlRunner overrides kubectl execution, used for the workload
	// namespace this backend owns alongside the harness release. Empty uses
	// the kubectl binary on PATH.
	KubectlRunner Runner
}

// Deployer manages modelharness and modeldeployment lifecycles with the Helm CLI.
type Deployer struct {
	modelHarnessChart    string
	modelDeploymentChart string
	run                  Runner
	kubectl              Runner
}

var _ deploy.Deployer = (*Deployer)(nil)

// New returns a Helm-backed Deployer.
func New(opts Options) (*Deployer, error) {
	d := &Deployer{
		modelHarnessChart:    firstNonEmpty(opts.ModelHarnessChart, os.Getenv(EnvModelHarnessChart), defaultModelHarnessChartPath),
		modelDeploymentChart: firstNonEmpty(opts.ModelDeploymentChart, os.Getenv(EnvModelDeploymentChart), defaultModelDeploymentChartPath),
		run:                  opts.Runner,
		kubectl:              opts.KubectlRunner,
	}
	if d.run == nil {
		d.run = execRunner("helm")
	}
	if d.kubectl == nil {
		d.kubectl = execRunner("kubectl")
	}
	return d, nil
}

// Name implements deploy.Deployer.
func (d *Deployer) Name() string { return BackendName }

// InstallModelHarness ensures the workload namespace exists and then runs
// `helm upgrade --install` for the modelharness chart in it. It provisions the
// per-namespace Gateway (named "<namespace>-gw" by chart default), the
// catch-all `model-not-found-direct` EnvoyFilter (Envoy `direct_response`
// returning 404 + OpenAI-compatible JSON, plus the model-discovery routes),
// and — when AuthEnabled is true — the per-namespace AuthorizationPolicy +
// APIKey CR. When the chart's networkPolicy values are enabled it additionally
// renders the CiliumNetworkPolicy that locks down East-West ingress while
// keeping the gateway pod reachable.
//
// Idempotent: re-running on an existing release reconciles the values.
func (d *Deployer) InstallModelHarness(ctx context.Context, values deploy.ModelHarnessValues) error {
	if err := values.Validate(); err != nil {
		return err
	}
	chart, err := resolveChart(d.modelHarnessChart, EnvModelHarnessChart, "modelharness")
	if err != nil {
		return err
	}
	if err := d.ensureNamespace(ctx, values.Namespace); err != nil {
		return err
	}

	args := []string{
		"upgrade", "--install", ModelHarnessReleaseName, chart,
		"--namespace", values.Namespace,
		"--set", "namespace=" + values.Namespace,
		"--set", "auth.enabled=" + strconv.FormatBool(values.AuthEnabled),
	}
	args = append(args, gatewaySetArgs(values.Gateway)...)
	if values.Gateway.GatewayClassName != "" {
		args = append(args, "--set", "gatewayClassName="+values.Gateway.GatewayClassName)
	}
	args = append(args, "--wait")

	if out, err := d.run(ctx, args...); err != nil {
		return fmt.Errorf("helm upgrade --install %s in %s failed: %w\n%s",
			ModelHarnessReleaseName, values.Namespace, err, string(out))
	}
	return nil
}

// ensureNamespace creates the workload namespace if absent and stamps the
// discovery label the control plane selects on. Both steps are idempotent.
func (d *Deployer) ensureNamespace(ctx context.Context, namespace string) error {
	if _, err := d.kubectl(ctx, "get", "namespace", namespace); err != nil {
		if out, createErr := d.kubectl(ctx, "create", "namespace", namespace); createErr != nil {
			return fmt.Errorf("create namespace %s: %w\n%s", namespace, createErr, string(out))
		}
	}
	if out, err := d.kubectl(ctx, "label", "namespace", namespace,
		namespaceDiscoveryLabel+"="+namespaceDiscoveryValue, "--overwrite"); err != nil {
		return fmt.Errorf("label namespace %s: %w\n%s", namespace, err, string(out))
	}
	return nil
}

// UninstallModelHarness runs `helm uninstall` for the modelharness release and
// then deletes the workload namespace, which cascades the per-case Gateway and
// anything else left in it. Missing releases and namespaces are treated as
// success. The namespace delete does not block on termination: finalizers can
// keep a namespace Terminating for minutes and no caller needs to observe it
// gone.
func (d *Deployer) UninstallModelHarness(ctx context.Context, namespace string) error {
	if namespace == "" {
		return fmt.Errorf("modelharness: namespace is required")
	}
	args := []string{"uninstall", ModelHarnessReleaseName,
		"--namespace", namespace, "--ignore-not-found", "--wait"}

	if out, err := d.run(ctx, args...); err != nil {
		return fmt.Errorf("helm uninstall %s in %s failed: %w\n%s",
			ModelHarnessReleaseName, namespace, err, string(out))
	}

	if out, err := d.kubectl(ctx, "delete", "namespace", namespace,
		"--ignore-not-found", "--wait=false"); err != nil {
		return fmt.Errorf("delete namespace %s: %w\n%s", namespace, err, string(out))
	}
	return nil
}

// InstallModelDeployment runs `helm upgrade --install` for the modeldeployment
// chart with the supplied values. It is idempotent — re-running it on an
// existing release reconciles to the new values. The release name equals
// values.Name and the release namespace equals values.Namespace.
func (d *Deployer) InstallModelDeployment(ctx context.Context, values deploy.ModelDeploymentValues) error {
	if err := values.Validate(); err != nil {
		return err
	}
	chart, err := resolveChart(d.modelDeploymentChart, EnvModelDeploymentChart, "modeldeployment")
	if err != nil {
		return err
	}

	args := []string{
		"upgrade", "--install", values.Name, chart,
		"--namespace", values.Namespace,
		"--create-namespace",
	}
	args = append(args, setArgs(values)...)
	args = append(args, gatewaySetArgs(values.Gateway)...)

	if out, err := d.run(ctx, args...); err != nil {
		return fmt.Errorf("helm upgrade --install %s failed: %w\n%s", values.Name, err, string(out))
	}
	return nil
}

// UpgradeModelDeployment runs `helm upgrade` (without --install) so a release
// that does not exist yet is a loud failure rather than a silent create. Every
// value is re-sent, so the release converges on values rather than merging
// onto whatever it previously held.
func (d *Deployer) UpgradeModelDeployment(ctx context.Context, values deploy.ModelDeploymentValues) error {
	if err := values.Validate(); err != nil {
		return err
	}
	chart, err := resolveChart(d.modelDeploymentChart, EnvModelDeploymentChart, "modeldeployment")
	if err != nil {
		return err
	}

	args := []string{"upgrade", values.Name, chart, "--namespace", values.Namespace}
	args = append(args, setArgs(values)...)
	args = append(args, gatewaySetArgs(values.Gateway)...)

	if out, err := d.run(ctx, args...); err != nil {
		return fmt.Errorf("helm upgrade %s in %s failed: %w\n%s",
			values.Name, values.Namespace, err, string(out))
	}
	return nil
}

func gatewaySetArgs(values deploy.GatewayValues) []string {
	args := []string{}
	if values.CloudProvider != "" {
		args = append(args, "--set", "cloudprovider="+values.CloudProvider)
	}
	if values.DefaultDomain != "" {
		args = append(args, "--set-string", "azure.defaultDomain.zoneName="+values.DefaultDomain)
	}
	return args
}

// UninstallModelDeployment runs `helm uninstall` for the named release.
// Missing releases are treated as success (so cleanup is idempotent).
func (d *Deployer) UninstallModelDeployment(ctx context.Context, name, namespace string) error {
	if name == "" {
		return fmt.Errorf("modeldeployment: name is required")
	}
	args := []string{"uninstall", name, "--namespace", namespace, "--ignore-not-found", "--wait"}

	if out, err := d.run(ctx, args...); err != nil {
		return fmt.Errorf("helm uninstall %s failed: %w\n%s", name, err, string(out))
	}
	return nil
}

// setArgs builds the `--set` arguments for the modeldeployment chart.
func setArgs(v deploy.ModelDeploymentValues) []string {
	args := []string{
		"--set", "name=" + v.Name,
		"--set", "namespace=" + v.Namespace,
		"--set", "model=" + v.Model,
		// Always rendered: 0 means scale-to-zero, which UpgradeModelDeployment
		// uses to empty an inference pool, so it cannot be treated as "unset".
		"--set", "replicas=" + strconv.FormatInt(v.Replicas, 10),
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
	if v.EPPScorerWeights != nil {
		if v.EPPScorerWeights.Queue != nil {
			args = append(args, "--set", "epp.scorerWeights.queue="+strconv.Itoa(*v.EPPScorerWeights.Queue))
		}
		if v.EPPScorerWeights.KVCacheUtilization != nil {
			args = append(args, "--set", "epp.scorerWeights.kvCacheUtilization="+strconv.Itoa(*v.EPPScorerWeights.KVCacheUtilization))
		}
		if v.EPPScorerWeights.PrefixCache != nil {
			args = append(args, "--set", "epp.scorerWeights.prefixCache="+strconv.Itoa(*v.EPPScorerWeights.PrefixCache))
		}
	}
	return args
}

// resolveChart returns an existing chart path, retrying one directory level up
// so tests invoked from test/e2e still find repo-root-relative charts.
func resolveChart(chart, envVar, name string) (string, error) {
	_, err := os.Stat(chart)
	if err == nil {
		return chart, nil
	}
	alt := filepath.Join("..", "..", chart)
	if _, altErr := os.Stat(alt); altErr == nil {
		return alt, nil
	}
	return "", fmt.Errorf("%s chart not found at %q (set %s): %w", name, chart, envVar, err)
}

func execRunner(bin string) Runner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, bin, args...).CombinedOutput()
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
