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

package helm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
)

// newTestDeployer returns a Deployer whose charts resolve to real directories
// and whose helm invocations are captured instead of executed.
func newTestDeployer(t *testing.T, err error) (*Deployer, *[][]string) {
	t.Helper()

	root := t.TempDir()
	harnessChart := filepath.Join(root, "modelharness")
	deploymentChart := filepath.Join(root, "modeldeployment")
	for _, dir := range []string{harnessChart, deploymentChart} {
		if mkErr := os.Mkdir(dir, 0o755); mkErr != nil {
			t.Fatalf("create chart dir: %v", mkErr)
		}
	}

	var calls [][]string
	d, newErr := New(Options{
		ModelHarnessChart:    harnessChart,
		ModelDeploymentChart: deploymentChart,
		Runner: func(_ context.Context, args ...string) ([]byte, error) {
			calls = append(calls, args)
			return []byte("helm output"), err
		},
	})
	if newErr != nil {
		t.Fatalf("New: %v", newErr)
	}
	return d, &calls
}

// argValue returns the argument following flag whose value carries prefix.
func argValue(args []string, flag, prefix string) (string, bool) {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && strings.HasPrefix(args[i+1], prefix) {
			return strings.TrimPrefix(args[i+1], prefix), true
		}
	}
	return "", false
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestName(t *testing.T) {
	d, _ := newTestDeployer(t, nil)
	if got := d.Name(); got != BackendName {
		t.Fatalf("Name() = %q, want %q", got, BackendName)
	}
}

func TestInstallModelDeploymentMapsValues(t *testing.T) {
	d, calls := newTestDeployer(t, nil)

	queue, prefix := 5, 0
	values := deploy.ModelDeploymentValues{
		Name:          "phi",
		Namespace:     "e2e-ns",
		Model:         "phi-4-mini-instruct",
		Replicas:      3,
		InstanceType:  "Standard_NC24ads_A100_v4",
		EnableScaling: true,
		MaxReplicas:   7,
		ScalingMetrics: []deploy.ScalingMetric{
			{Name: "vllm:num_requests_waiting", Type: "gauge", UpThreshold: "10", DownThreshold: "1"},
			{Name: "vllm:request_queue_time_seconds", Type: "histogram", UpThreshold: "30", DownThreshold: "1", MetricCacheWindow: "300"},
		},
		AutoUpgrade: deploy.AutoUpgrade{
			Enabled:                   true,
			MaintenanceWindowSchedule: "0 2 * * 6",
			MaintenanceWindowDuration: "4h",
		},
		EPPScorerWeights: &deploy.EPPScorerWeights{Queue: &queue, PrefixCache: &prefix},
	}

	if err := d.InstallModelDeployment(context.Background(), values); err != nil {
		t.Fatalf("InstallModelDeployment: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 helm invocation, got %d", len(*calls))
	}
	args := (*calls)[0]

	if args[0] != "upgrade" || args[1] != "--install" || args[2] != "phi" {
		t.Fatalf("unexpected leading args: %v", args[:3])
	}
	if ns, ok := argValue(args, "--namespace", ""); !ok || ns != "e2e-ns" {
		t.Fatalf("--namespace = %q (found=%v), want e2e-ns", ns, ok)
	}

	for _, want := range []string{
		"name=phi",
		"namespace=e2e-ns",
		"model=phi-4-mini-instruct",
		"replicas=3",
		"instanceType=Standard_NC24ads_A100_v4",
		"enableScaling=true",
		"maxReplicas=7",
		"scaling.metrics[0].name=vllm:num_requests_waiting",
		"scaling.metrics[0].type=gauge",
		"scaling.metrics[0].upThreshold=10",
		"scaling.metrics[0].downThreshold=1",
		"scaling.metrics[1].name=vllm:request_queue_time_seconds",
		"scaling.metrics[1].metricCacheWindow=300",
		"autoUpgrade.enabled=true",
		"epp.scorerWeights.queue=5",
	} {
		if !hasArg(args, want) {
			t.Errorf("missing --set %q in %v", want, args)
		}
	}

	// A zero weight is meaningful and must still be rendered.
	if !hasArg(args, "epp.scorerWeights.prefixCache=0") {
		t.Errorf("zero-valued prefixCache weight was dropped: %v", args)
	}
	// kvCacheUtilization was nil and must fall through to the chart default.
	if _, found := argValue(args, "--set", "epp.scorerWeights.kvCacheUtilization="); found {
		t.Errorf("nil kvCacheUtilization must not be rendered: %v", args)
	}
	// The cron schedule contains spaces and asterisks, so it must be passed
	// verbatim via --set-string rather than --set.
	if v, ok := argValue(args, "--set-string", "autoUpgrade.maintenanceWindow.schedule="); !ok || v != "0 2 * * 6" {
		t.Errorf("schedule not passed via --set-string: %v", args)
	}
	if _, ok := argValue(args, "--set-string", "autoUpgrade.maintenanceWindow.duration="); !ok {
		t.Errorf("maintenance window duration missing: %v", args)
	}

	// The metrics-only field must not leak when scaling is disabled.
	values.EnableScaling = false
	values.ScalingMetrics = nil
	if err := d.InstallModelDeployment(context.Background(), values); err != nil {
		t.Fatalf("InstallModelDeployment (scaling off): %v", err)
	}
	if args := (*calls)[1]; hasArg(args, "enableScaling=true") || hasArg(args, "maxReplicas=7") {
		t.Errorf("scaling args rendered while scaling disabled: %v", args)
	}
}

func TestInstallValidatesBeforeInvokingHelm(t *testing.T) {
	tests := []struct {
		name    string
		values  deploy.ModelDeploymentValues
		wantErr string
	}{
		{
			name:    "missing name",
			values:  deploy.ModelDeploymentValues{Model: "phi"},
			wantErr: "Name (deployment name) is required",
		},
		{
			name:    "missing model",
			values:  deploy.ModelDeploymentValues{Name: "phi"},
			wantErr: "Model is required",
		},
		{
			name:    "scaling without metrics",
			values:  deploy.ModelDeploymentValues{Name: "phi", Model: "phi", EnableScaling: true},
			wantErr: "at least one ScalingMetric",
		},
		{
			name: "metric without thresholds",
			values: deploy.ModelDeploymentValues{
				Name: "phi", Model: "phi", EnableScaling: true,
				ScalingMetrics: []deploy.ScalingMetric{{Name: "vllm:num_requests_waiting"}},
			},
			wantErr: "UpThreshold is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, calls := newTestDeployer(t, nil)
			err := d.InstallModelDeployment(context.Background(), tc.values)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			if len(*calls) != 0 {
				t.Fatalf("helm was invoked despite invalid values: %v", *calls)
			}
		})
	}
}

func TestInstallModelHarnessMapsAuth(t *testing.T) {
	d, calls := newTestDeployer(t, nil)

	if err := d.InstallModelHarness(context.Background(), deploy.ModelHarnessValues{Namespace: "ns-a"}); err != nil {
		t.Fatalf("InstallModelHarness: %v", err)
	}
	if err := d.InstallModelHarness(context.Background(), deploy.ModelHarnessValues{Namespace: "ns-b", AuthEnabled: true}); err != nil {
		t.Fatalf("InstallModelHarness (auth): %v", err)
	}

	if got := (*calls)[0]; got[2] != ModelHarnessReleaseName || !hasArg(got, "auth.enabled=false") || !hasArg(got, "namespace=ns-a") {
		t.Errorf("unexpected args for auth-disabled harness: %v", got)
	}
	if got := (*calls)[1]; !hasArg(got, "auth.enabled=true") || !hasArg(got, "namespace=ns-b") {
		t.Errorf("unexpected args for auth-enabled harness: %v", got)
	}
	// --wait keeps the caller from racing the gateway rollout.
	if !hasArg((*calls)[0], "--wait") {
		t.Errorf("modelharness install must wait for readiness: %v", (*calls)[0])
	}

	err := d.InstallModelHarness(context.Background(), deploy.ModelHarnessValues{})
	if err == nil || !strings.Contains(err.Error(), "Namespace is required") {
		t.Fatalf("error = %v, want a missing-namespace validation error", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("helm was invoked for an invalid harness: %v", *calls)
	}
}

func TestUninstallIsIdempotentAndScoped(t *testing.T) {
	d, calls := newTestDeployer(t, nil)

	if err := d.UninstallModelDeployment(context.Background(), "phi", "e2e-ns"); err != nil {
		t.Fatalf("UninstallModelDeployment: %v", err)
	}
	if err := d.UninstallModelHarness(context.Background(), "e2e-ns"); err != nil {
		t.Fatalf("UninstallModelHarness: %v", err)
	}

	for i, args := range *calls {
		if args[0] != "uninstall" {
			t.Fatalf("call %d is not an uninstall: %v", i, args)
		}
		// --ignore-not-found is what makes cleanup idempotent.
		if !hasArg(args, "--ignore-not-found") || !hasArg(args, "--wait") {
			t.Errorf("call %d missing idempotent cleanup flags: %v", i, args)
		}
		if ns, ok := argValue(args, "--namespace", ""); !ok || ns != "e2e-ns" {
			t.Errorf("call %d not scoped to the namespace: %v", i, args)
		}
	}
	if (*calls)[1][1] != ModelHarnessReleaseName {
		t.Errorf("harness uninstall targeted %q, want %q", (*calls)[1][1], ModelHarnessReleaseName)
	}
}

func TestHelmFailurePropagatesOutput(t *testing.T) {
	sentinel := errors.New("exit status 1")
	d, _ := newTestDeployer(t, sentinel)

	err := d.InstallModelDeployment(context.Background(), deploy.ModelDeploymentValues{
		Name: "phi", Namespace: "e2e-ns", Model: "phi-4-mini-instruct",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap the helm failure", err)
	}
	if !strings.Contains(err.Error(), "phi") || !strings.Contains(err.Error(), "helm output") {
		t.Fatalf("error = %v, want it to name the release and include helm output", err)
	}
}

func TestMissingChartFailsWithOverrideHint(t *testing.T) {
	var calls [][]string
	d, err := New(Options{
		ModelHarnessChart:    filepath.Join(t.TempDir(), "absent"),
		ModelDeploymentChart: filepath.Join(t.TempDir(), "absent"),
		Runner: func(_ context.Context, args ...string) ([]byte, error) {
			calls = append(calls, args)
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	installErr := d.InstallModelDeployment(context.Background(), deploy.ModelDeploymentValues{
		Name: "phi", Namespace: "ns", Model: "phi",
	})
	if installErr == nil || !strings.Contains(installErr.Error(), EnvModelDeploymentChart) {
		t.Fatalf("error = %v, want it to mention %s", installErr, EnvModelDeploymentChart)
	}
	if len(calls) != 0 {
		t.Fatalf("helm was invoked with an unresolved chart: %v", calls)
	}
}

func TestRegisteredAsDefaultBackend(t *testing.T) {
	d, err := deploy.New(BackendName)
	if err != nil {
		t.Fatalf("deploy.New(%q): %v", BackendName, err)
	}
	if d.Name() != BackendName {
		t.Fatalf("backend name = %q, want %q", d.Name(), BackendName)
	}
	if deploy.DefaultBackend != BackendName {
		t.Fatalf("DefaultBackend = %q, want %q so local runs stay on Helm", deploy.DefaultBackend, BackendName)
	}
}
