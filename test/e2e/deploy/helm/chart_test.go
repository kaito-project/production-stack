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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func TestModelDeploymentScalingAnnotations(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart rendering tests")
	}
	const prefix = "scaledobject.kaito.sh/"
	for _, tt := range []struct {
		name    string
		values  []string
		want    map[string]string
		omitted []string
	}{
		{
			name:   "defaults",
			values: []string{"enableScaling=true", "minReplicas=1", "maxReplicas=5"},
			want: map[string]string{
				"scaleupstabilizationwindow": "60", "scaleupcooldown": "900", "scaleuptolerance": "0.1",
				"scaledownstabilizationwindow": "1800", "scaledowncooldown": "1800", "scaledowntolerance": "0.1",
			},
			omitted: []string{"evaluationwindow", "initialcooldownperiod", "cooldownperiod"},
		},
		{
			name:   "scale-to-zero defaults",
			values: []string{"enableScaling=true", "minReplicas=0", "maxReplicas=5"},
			want:   map[string]string{"initialcooldownperiod": "1800", "cooldownperiod": "1800"},
		},
		{
			name: "independent HPA fields including zero",
			values: []string{
				"enableScaling=true", "minReplicas=0", "maxReplicas=5",
				"scaling.hpa.scaleUp.stabilizationWindowSeconds=0", "scaling.hpa.scaleUp.periodSeconds=1",
				"scaling.hpa.scaleUp.tolerance=0", "scaling.hpa.scaleDown.stabilizationWindowSeconds=3600",
				"scaling.hpa.scaleDown.periodSeconds=1800", "scaling.hpa.scaleDown.tolerance=0.2",
				"scaling.initialCooldownPeriod=120",
			},
			want: map[string]string{
				"scaleupstabilizationwindow": "0", "scaleupcooldown": "1", "scaleuptolerance": "0",
				"scaledownstabilizationwindow": "3600", "scaledowncooldown": "1800", "scaledowntolerance": "0.2",
				"initialcooldownperiod": "120", "cooldownperiod": "1800",
			},
		},
		{
			name: "boundary values are accepted",
			values: []string{
				"enableScaling=true", "minReplicas=0", "maxReplicas=5",
				"scaling.hpa.scaleUp.stabilizationWindowSeconds=3600", "scaling.hpa.scaleUp.periodSeconds=1800",
				"scaling.hpa.scaleUp.tolerance=0.99", "scaling.hpa.scaleDown.stabilizationWindowSeconds=0",
				"scaling.hpa.scaleDown.periodSeconds=1", "scaling.hpa.scaleDown.tolerance=0.49",
				"scaling.cooldownPeriod=0", "scaling.initialCooldownPeriod=2147483647",
			},
			want: map[string]string{
				"scaleupstabilizationwindow": "3600", "scaleupcooldown": "1800", "scaleuptolerance": "0.99",
				"scaledownstabilizationwindow": "0", "scaledowncooldown": "1", "scaledowntolerance": "0.49",
				"cooldownperiod": "0", "initialcooldownperiod": "2147483647",
			},
		},
		{
			name: "numeric strings are accepted and normalized",
			values: []string{
				"enableScaling=true", "minReplicas=0", "maxReplicas=5",
				"string:scaling.hpa.scaleUp.periodSeconds=0900", "string:scaling.hpa.scaleDown.stabilizationWindowSeconds=600",
				"string:scaling.hpa.scaleDown.tolerance=0.25", "string:scaling.cooldownPeriod=00",
			},
			want: map[string]string{
				"scaleupcooldown": "900", "scaledownstabilizationwindow": "600",
				"scaledowntolerance": "0.25", "cooldownperiod": "0",
			},
		},
		{
			name:   "explicit zero initial cooldown",
			values: []string{"enableScaling=true", "minReplicas=0", "maxReplicas=5", "scaling.initialCooldownPeriod=0"},
			want:   map[string]string{"initialcooldownperiod": "0"},
		},
		{
			name:   "maximum initial cooldown",
			values: []string{"enableScaling=true", "minReplicas=0", "maxReplicas=5", "scaling.initialCooldownPeriod=2147483647"},
			want:   map[string]string{"initialcooldownperiod": "2147483647"},
		},
		{
			name: "null overrides",
			values: []string{
				"enableScaling=true", "minReplicas=0", "maxReplicas=5",
				"scaling.hpa.scaleUp.stabilizationWindowSeconds=null", "scaling.hpa.scaleDown.tolerance=null",
				"scaling.initialCooldownPeriod=null",
			},
			omitted: []string{"scaleupstabilizationwindow", "scaledowntolerance", "initialcooldownperiod"},
		},
		{
			name:    "initial cooldown ignored above zero",
			values:  []string{"enableScaling=true", "minReplicas=1", "maxReplicas=5", "scaling.initialCooldownPeriod=120"},
			omitted: []string{"initialcooldownperiod", "cooldownperiod"},
		},
		{
			name:    "autoscaling disabled",
			values:  []string{"scaling.hpa.scaleUp.tolerance=0.2", "scaling.initialCooldownPeriod=120"},
			omitted: []string{"auto-provision", "scaleupstabilizationwindow", "scaleuptolerance", "initialcooldownperiod"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			output, err := renderModelDeployment(t, tt.values...)
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, output)
			}
			var is struct {
				Metadata metav1.ObjectMeta `json:"metadata"`
			}
			if err := yaml.Unmarshal(output, &is); err != nil {
				t.Fatalf("decode InferenceSet: %v", err)
			}
			for key, want := range tt.want {
				if got := is.Metadata.Annotations[prefix+key]; got != want {
					t.Errorf("annotation %s = %q, want %q", key, got, want)
				}
			}
			for _, key := range tt.omitted {
				if _, found := is.Metadata.Annotations[prefix+key]; found {
					t.Errorf("annotation %s should be omitted", key)
				}
			}
		})
	}
}

func TestModelDeploymentScalingValidation(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart rendering tests")
	}
	for _, tt := range []struct {
		value string
		field string
	}{
		{"scaling.hpa.scaleUp.stabilizationWindowSeconds=-1", "stabilizationWindowSeconds"},
		{"scaling.hpa.scaleDown.stabilizationWindowSeconds=3601", "stabilizationWindowSeconds"},
		{"scaling.hpa.scaleUp.periodSeconds=0", "periodSeconds"},
		{"scaling.hpa.scaleDown.periodSeconds=1801", "periodSeconds"},
		{"scaling.hpa.scaleUp.periodSeconds=1.5", "periodSeconds"},
		{"scaling.hpa.scaleUp.tolerance=-0.1", "tolerance"},
		{"scaling.hpa.scaleUp.tolerance=1", "tolerance"},
		{"scaling.hpa.scaleDown.tolerance=0.5", "tolerance"},
		{"string:scaling.hpa.scaleUp.tolerance=1.5", "scaling.hpa.scaleUp.tolerance"},
		{"string:scaling.hpa.scaleDown.tolerance=0.5", "scaling.hpa.scaleDown.tolerance"},
		{"string:scaling.hpa.scaleUp.stabilizationWindowSeconds=3601", "scaling.hpa.scaleUp.stabilizationWindowSeconds"},
		{"string:scaling.hpa.scaleUp.periodSeconds=0", "scaling.hpa.scaleUp.periodSeconds"},
		{"string:scaling.hpa.scaleDown.periodSeconds=1801", "scaling.hpa.scaleDown.periodSeconds"},
		{"string:scaling.hpa.scaleDown.stabilizationWindowSeconds=abc", "stabilizationWindowSeconds"},
		{"string:scaling.cooldownPeriod=2147483648", "scaling.cooldownPeriod"},
		{"scaling.hpa.scaleDown.tolerance=NaN", "tolerance"},
		{"scaling.hpa.scaleDown.tolerance=invalid", "tolerance"},
		{"scaling.initialCooldownPeriod=-1", "initialCooldownPeriod"},
		{"scaling.initialCooldownPeriod=2147483648", "initialCooldownPeriod"},
		{"scaling.initialCooldownPeriod=9999999999999999999999999", "initialCooldownPeriod"},
		{"scaling.initialCooldownPeriod=invalid", "initialCooldownPeriod"},
		{"scaling.hpa.scaleSideways.periodSeconds=10", "scaleSideways"},
	} {
		t.Run(tt.value, func(t *testing.T) {
			output, err := renderModelDeployment(t, "enableScaling=true", "minReplicas=0", "maxReplicas=5", tt.value)
			if err == nil || !strings.Contains(string(output), tt.field) {
				t.Fatalf("expected validation error for %s, got %v\n%s", tt.field, err, output)
			}
		})
	}
}

func renderModelDeployment(t *testing.T, values ...string) ([]byte, error) {
	t.Helper()
	chart := filepath.Join("..", "..", "..", "..", "charts", "modeldeployment")
	args := []string{"template", "model", chart, "--show-only", "templates/inferenceset.yaml", "--set", "model=test-model"}
	for _, value := range values {
		// A "string:" prefix passes the value via --set-string, exercising
		// the template guards that back the schema for numeric strings.
		if v, ok := strings.CutPrefix(value, "string:"); ok {
			args = append(args, "--set-string", v)
			continue
		}
		args = append(args, "--set", value)
	}
	return exec.Command("helm", args...).CombinedOutput()
}
