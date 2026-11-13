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
	"encoding/json"
	"maps"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

var maxLengthErrorRe = regexp.MustCompile(`name: String length must be less than or equal to 63|'/name': maxLength: got \d+, want 63`)

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

// The deployment name feeds the EPP Service name, the `inferencepool`
// selector label value and the InferencePool name (which Istio copies into a
// label on the pool's shadow Service), all capped at 63 characters. A
// 63-character name must render objects that satisfy those limits, and a
// 64-character name must be rejected by the chart.
func TestModelDeploymentNameLength(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart rendering tests")
	}
	chart := filepath.Join("..", "..", "..", "..", "charts", "modeldeployment")
	render := func(name string) ([]byte, error) {
		return exec.Command("helm", "template", "model", chart,
			"--set", "model=test-model", "--set", "namespace=ns", "--set", "name="+name).CombinedOutput()
	}

	// Helm's JSON schema validator words the error differently across releases:
	// "name: String length must be less than or equal to 63" before v3.18,
	// "at '/name': maxLength: got 64, want 63" from v3.18 on.
	output, err := render(strings.Repeat("a", 64))
	if err == nil || !maxLengthErrorRe.Match(output) {
		t.Errorf("expected a 64-character name to be rejected, got %v\n%s", err, output)
	}

	output, err = render(strings.Repeat("a", 63))
	if err != nil {
		t.Fatalf("render with a 63-character name: %v\n%s", err, output)
	}
	var kinds []string
	for _, doc := range strings.Split(string(output), "\n---") {
		var obj struct {
			Kind     string            `json:"kind"`
			Metadata metav1.ObjectMeta `json:"metadata"`
			Spec     struct {
				Selector json.RawMessage `json:"selector"`
				Template struct {
					Metadata metav1.ObjectMeta `json:"metadata"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("parse rendered object: %v\n%s", err, doc)
		}
		if obj.Kind == "" {
			continue
		}
		kinds = append(kinds, obj.Kind)
		labels := map[string]string{}
		maps.Copy(labels, obj.Metadata.Labels)
		maps.Copy(labels, obj.Spec.Template.Metadata.Labels)
		switch obj.Kind {
		case "Service":
			if errs := validation.IsDNS1035Label(obj.Metadata.Name); len(errs) > 0 {
				t.Errorf("Service name %q: %v", obj.Metadata.Name, errs)
			}
			var selector map[string]string
			if err := json.Unmarshal(obj.Spec.Selector, &selector); err != nil {
				t.Fatalf("parse Service selector: %v", err)
			}
			maps.Copy(labels, selector)
		case "Deployment":
			var selector metav1.LabelSelector
			if err := json.Unmarshal(obj.Spec.Selector, &selector); err != nil {
				t.Fatalf("parse Deployment selector: %v", err)
			}
			maps.Copy(labels, selector.MatchLabels)
		case "InferencePool":
			if len(obj.Metadata.Name) > 63 {
				t.Errorf("InferencePool name %q exceeds 63 characters", obj.Metadata.Name)
			}
		}
		for key, value := range labels {
			if len(value) > 63 {
				t.Errorf("%s %s: label %s=%q exceeds 63 characters", obj.Kind, obj.Metadata.Name, key, value)
			}
		}
	}
	for _, kind := range []string{"Service", "Deployment", "InferencePool"} {
		if !slices.Contains(kinds, kind) {
			t.Errorf("rendered chart has no %s; rendered kinds: %v", kind, kinds)
		}
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

// TestModelHarnessOutageReplyMappers pins the attribution contract of the
// consolidated outage local_reply: mapper order (first match wins), and that
// only Envoy-generated ext_proc failures (`ext_proc_error` code details) are
// blamed on the EPP or BBR, so a 5xx the EPP returns on purpose because it
// cannot place a request surfaces as a retryable model_unavailable whatever
// status the EPP picks.
func TestModelHarnessOutageReplyMappers(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart rendering tests")
	}
	chart := filepath.Join("..", "..", "..", "..", "charts", "modelharness")
	output, err := exec.Command("helm", "template", "harness", chart, "--set", "namespace=ns",
		"--show-only", "templates/envoyfilter-outage-reply.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, output)
	}

	type header struct {
		Name         string `json:"name"`
		PresentMatch bool   `json:"present_match"`
		InvertMatch  bool   `json:"invert_match"`
	}
	type filter struct {
		HeaderFilter *struct {
			Header header `json:"header"`
		} `json:"header_filter"`
		ResponseFlagFilter *struct {
			Flags []string `json:"flags"`
		} `json:"response_flag_filter"`
		ExtensionFilter *struct {
			Name        string `json:"name"`
			TypedConfig struct {
				Expression string `json:"expression"`
			} `json:"typed_config"`
		} `json:"extension_filter"`
	}
	var ef struct {
		Spec struct {
			ConfigPatches []struct {
				Patch struct {
					Value struct {
						TypedConfig struct {
							LocalReplyConfig struct {
								Mappers []struct {
									Filter struct {
										AndFilter struct {
											Filters []filter `json:"filters"`
										} `json:"and_filter"`
									} `json:"filter"`
									StatusCode   int `json:"status_code"`
									HeadersToAdd []struct {
										Header struct {
											Key   string `json:"key"`
											Value string `json:"value"`
										} `json:"header"`
									} `json:"headers_to_add"`
									Body struct {
										InlineString string `json:"inline_string"`
									} `json:"body"`
								} `json:"mappers"`
							} `json:"local_reply_config"`
						} `json:"typed_config"`
					} `json:"value"`
				} `json:"patch"`
			} `json:"configPatches"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(output, &ef); err != nil {
		t.Fatalf("decode EnvoyFilter: %v\n%s", err, output)
	}
	if len(ef.Spec.ConfigPatches) != 1 {
		t.Fatalf("expected 1 config patch, got %d", len(ef.Spec.ConfigPatches))
	}
	mappers := ef.Spec.ConfigPatches[0].Patch.Value.TypedConfig.LocalReplyConfig.Mappers

	const extProcError = "response.code_details.startsWith('ext_proc_error')"
	for _, tt := range []struct {
		code         string
		status       int
		source       string
		modelHeader  string // "present", "absent" or "" (not matched)
		routerFlag   bool
		extProcError bool
		retryAfter   bool
	}{
		{code: "ext_authz_unavailable", status: 502, source: "authz"},
		{code: "model_unavailable", status: 503, source: "inferenceset", modelHeader: "present", routerFlag: true, retryAfter: true},
		{code: "gateway_unavailable", status: 502, source: "gateway", modelHeader: "absent", routerFlag: true},
		{code: "epp_unavailable", status: 502, source: "epp", modelHeader: "present", extProcError: true},
		{code: "model_unavailable", status: 503, source: "inferenceset", modelHeader: "present", retryAfter: true},
		{code: "bbr_unavailable", status: 502, source: "bbr", modelHeader: "absent", extProcError: true},
		{code: "gateway_internal_error", source: "gateway"},
	} {
		if len(mappers) == 0 {
			t.Fatalf("missing mapper for %s", tt.code)
		}
		m := mappers[0]
		mappers = mappers[1:]
		if !strings.Contains(m.Body.InlineString, `"code":"`+tt.code+`"`) {
			t.Errorf("mapper order: want %s, got body %s", tt.code, m.Body.InlineString)
			continue
		}
		if m.StatusCode != tt.status {
			t.Errorf("%s: status_code = %d, want %d", tt.code, m.StatusCode, tt.status)
		}
		headers := map[string]string{}
		for _, h := range m.HeadersToAdd {
			headers[h.Header.Key] = h.Header.Value
		}
		if headers["x-kaito-error-source"] != tt.source {
			t.Errorf("%s: x-kaito-error-source = %q, want %q", tt.code, headers["x-kaito-error-source"], tt.source)
		}
		if _, ok := headers["retry-after"]; ok != tt.retryAfter {
			t.Errorf("%s: retry-after present = %v, want %v", tt.code, ok, tt.retryAfter)
		}
		modelHeader, routerFlag, extProc := "", false, false
		for _, f := range m.Filter.AndFilter.Filters {
			switch {
			case f.HeaderFilter != nil && f.HeaderFilter.Header.Name == "X-Gateway-Model-Name":
				modelHeader = "present"
				if f.HeaderFilter.Header.InvertMatch {
					modelHeader = "absent"
				}
			case f.ResponseFlagFilter != nil && slices.Contains(f.ResponseFlagFilter.Flags, "UH"):
				routerFlag = true
			case f.ExtensionFilter != nil:
				if f.ExtensionFilter.Name != "envoy.access_loggers.extension_filters.cel" ||
					f.ExtensionFilter.TypedConfig.Expression != extProcError {
					t.Errorf("%s: unexpected extension filter %+v", tt.code, *f.ExtensionFilter)
				}
				extProc = true
			}
		}
		if modelHeader != tt.modelHeader || routerFlag != tt.routerFlag || extProc != tt.extProcError {
			t.Errorf("%s: match (model header %q, router flag %v, ext_proc_error %v), want (%q, %v, %v)",
				tt.code, modelHeader, routerFlag, extProc, tt.modelHeader, tt.routerFlag, tt.extProcError)
		}
	}
	if len(mappers) != 0 {
		t.Errorf("%d unexpected trailing mappers", len(mappers))
	}
}
