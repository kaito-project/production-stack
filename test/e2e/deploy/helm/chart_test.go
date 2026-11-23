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
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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

// TestModelHarnessModelsAPIRoute pins the model-discovery route: a
// harness-owned HTTPRoute (so an App Routing cluster publishes the namespace
// hostname before any modeldeployment exists), the ReferenceGrant that lets
// it reach the status reporter across namespaces, and no discovery routes
// left in the catch-all EnvoyFilter.
func TestModelHarnessModelsAPIRoute(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart rendering tests")
	}
	const ns = "team-a"
	for _, tt := range []struct {
		name          string
		values        []string
		wantHostnames bool
		wantGrant     bool
	}{
		{name: "non-cloud", wantGrant: true},
		{name: "azure", values: []string{"cloudprovider=azure", "azure.defaultDomain.zoneName=example.aksapp.io"},
			wantHostnames: true, wantGrant: true},
		{name: "reporter in the workload namespace", values: []string{"modelsAPI.serviceNamespace=" + ns}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			chart := filepath.Join("..", "..", "..", "..", "charts", "modelharness")
			args := []string{"template", "harness", chart, "--set", "namespace=" + ns}
			for _, value := range tt.values {
				args = append(args, "--set", value)
			}
			output, err := exec.Command("helm", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, output)
			}

			type object struct {
				Kind     string            `json:"kind"`
				Metadata metav1.ObjectMeta `json:"metadata"`
				Spec     json.RawMessage   `json:"spec"`
			}
			var route struct {
				ParentRefs []struct {
					Name string `json:"name"`
				} `json:"parentRefs"`
				Hostnames []string `json:"hostnames"`
				Rules     []struct {
					Matches []struct {
						Path struct {
							Type  string `json:"type"`
							Value string `json:"value"`
						} `json:"path"`
					} `json:"matches"`
					Filters []struct {
						Type                  string `json:"type"`
						RequestHeaderModifier struct {
							Set []struct {
								Name  string `json:"name"`
								Value string `json:"value"`
							} `json:"set"`
						} `json:"requestHeaderModifier"`
					} `json:"filters"`
					BackendRefs []struct {
						Name      string `json:"name"`
						Namespace string `json:"namespace"`
						Port      int    `json:"port"`
					} `json:"backendRefs"`
					Timeouts struct {
						Request string `json:"request"`
					} `json:"timeouts"`
				} `json:"rules"`
			}
			var grant struct {
				From []struct {
					Group     string `json:"group"`
					Kind      string `json:"kind"`
					Namespace string `json:"namespace"`
				} `json:"from"`
				To []struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"to"`
			}
			var listenerHostname string
			var routeFound, grantFound bool
			for _, doc := range strings.Split(string(output), "\n---") {
				var obj object
				if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
					t.Fatalf("parse rendered object: %v\n%s", err, doc)
				}
				switch {
				case obj.Kind == "HTTPRoute" && obj.Metadata.Name == "models-api":
					if obj.Metadata.Namespace != ns {
						t.Errorf("models-api HTTPRoute namespace = %q, want %q", obj.Metadata.Namespace, ns)
					}
					if err := json.Unmarshal(obj.Spec, &route); err != nil {
						t.Fatalf("decode models-api HTTPRoute: %v", err)
					}
					routeFound = true
				case obj.Kind == "ReferenceGrant" && obj.Metadata.Name == "modelharness-"+ns+"-models-api":
					if obj.Metadata.Namespace != "kube-system" {
						t.Errorf("ReferenceGrant namespace = %q, want the reporter namespace kube-system", obj.Metadata.Namespace)
					}
					if err := json.Unmarshal(obj.Spec, &grant); err != nil {
						t.Fatalf("decode ReferenceGrant: %v", err)
					}
					grantFound = true
				case obj.Kind == "Gateway":
					var gw struct {
						Listeners []struct {
							Hostname string `json:"hostname"`
						} `json:"listeners"`
					}
					if err := json.Unmarshal(obj.Spec, &gw); err != nil {
						t.Fatalf("decode Gateway: %v", err)
					}
					if len(gw.Listeners) > 0 {
						listenerHostname = gw.Listeners[0].Hostname
					}
				case obj.Kind == "EnvoyFilter" && obj.Metadata.Name == "model-not-found-direct":
					if strings.Contains(string(obj.Spec), "productionstack-status-reporter") {
						t.Errorf("model-not-found-direct still routes to the status reporter; discovery belongs to the models-api HTTPRoute")
					}
				}
			}

			if !routeFound {
				t.Fatal("models-api HTTPRoute not rendered")
			}
			if len(route.ParentRefs) != 1 || route.ParentRefs[0].Name != ns {
				t.Errorf("models-api parentRefs = %+v, want the namespace Gateway %q", route.ParentRefs, ns)
			}
			if tt.wantHostnames {
				if len(route.Hostnames) != 1 || route.Hostnames[0] != listenerHostname {
					t.Errorf("models-api hostnames = %v, want the Gateway listener hostname %q", route.Hostnames, listenerHostname)
				}
			} else if len(route.Hostnames) != 0 {
				t.Errorf("models-api hostnames = %v, want none", route.Hostnames)
			}
			if len(route.Rules) != 1 || len(route.Rules[0].Matches) != 1 || len(route.Rules[0].BackendRefs) != 1 {
				t.Fatalf("models-api rules = %+v, want one rule with one match and one backend", route.Rules)
			}
			rule := route.Rules[0]
			if path := rule.Matches[0].Path; path.Type != "PathPrefix" || path.Value != "/v1/models" {
				t.Errorf("models-api path match = %+v, want PathPrefix /v1/models", path)
			}
			var namespaceHeader string
			for _, filter := range rule.Filters {
				for _, header := range filter.RequestHeaderModifier.Set {
					if filter.Type == "RequestHeaderModifier" && header.Name == "X-Kaito-Gateway-Namespace" {
						namespaceHeader = header.Value
					}
				}
			}
			if namespaceHeader != ns {
				t.Errorf("models-api sets X-Kaito-Gateway-Namespace = %q, want %q", namespaceHeader, ns)
			}
			wantNamespace := "kube-system"
			if !tt.wantGrant {
				wantNamespace = ns
			}
			if backend := rule.BackendRefs[0]; backend.Name != "productionstack-status-reporter" ||
				backend.Namespace != wantNamespace || backend.Port != 8082 {
				t.Errorf("models-api backend = %+v, want productionstack-status-reporter in %s on 8082", backend, wantNamespace)
			}
			if rule.Timeouts.Request != "10s" {
				t.Errorf("models-api timeouts.request = %q, want 10s", rule.Timeouts.Request)
			}

			if grantFound != tt.wantGrant {
				t.Fatalf("models-api ReferenceGrant rendered = %v, want %v", grantFound, tt.wantGrant)
			}
			if tt.wantGrant {
				if len(grant.From) != 1 || grant.From[0].Group != "gateway.networking.k8s.io" ||
					grant.From[0].Kind != "HTTPRoute" || grant.From[0].Namespace != ns {
					t.Errorf("ReferenceGrant from = %+v, want HTTPRoutes in %s", grant.From, ns)
				}
				if len(grant.To) != 1 || grant.To[0].Kind != "Service" || grant.To[0].Name != "productionstack-status-reporter" {
					t.Errorf("ReferenceGrant to = %+v, want the productionstack-status-reporter Service", grant.To)
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

// TestModelHarnessBBRPlacement pins where BBR enters the Gateway HTTP filter
// chain: directly after the last ext_authz filter the auth EnvoyFilters insert,
// or first when auth renders none. It must not anchor on the InferencePool
// ext_proc filter, which Istio adds only to a Gateway that has an InferencePool
// attached, or BBR would be missing until the first modeldeployment exists.
func TestModelHarnessBBRPlacement(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart rendering tests")
	}
	azure := []string{"cloudprovider=azure", "azure.defaultDomain.zoneName=example.aksapp.io",
		"azure.entra.tenantId=00000000-0000-0000-0000-000000000000", "azure.entra.audiences[0]=api://test"}
	for _, tt := range []struct {
		name   string
		values []string
		anchor string // empty: no auth filter, so BBR is inserted first
	}{
		{name: "auth disabled"},
		{name: "apikey", values: []string{"auth.enabled=true"}, anchor: "envoy.filters.http.ext_authz"},
		{name: "apikey after jwt_authn", values: []string{"auth.enabled=true", "auth.extAuthz.operation=INSERT_AFTER"},
			anchor: "envoy.filters.http.ext_authz"},
		{name: "apikey on azure", values: append([]string{"auth.enabled=true"}, azure...),
			anchor: "envoy.filters.http.ext_authz"},
		{name: "azure", values: append([]string{"auth.enabled=true", "auth.mode=azure"}, azure...),
			anchor: "envoy.filters.http.ext_authz"},
		{name: "hybrid", values: append([]string{"auth.enabled=true", "auth.mode=hybrid"}, azure...),
			anchor: "ext_authz.azure"},
		{name: "hybrid without azure renders no auth", values: []string{"auth.enabled=true", "auth.mode=hybrid"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			chart := filepath.Join("..", "..", "..", "..", "charts", "modelharness")
			args := []string{"template", "harness", chart, "--set", "namespace=ns"}
			for _, value := range tt.values {
				args = append(args, "--set", value)
			}
			output, err := exec.Command("helm", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, output)
			}

			type envoyFilter struct {
				Kind     string            `json:"kind"`
				Metadata metav1.ObjectMeta `json:"metadata"`
				Spec     struct {
					Priority      int `json:"priority"`
					ConfigPatches []struct {
						ApplyTo string `json:"applyTo"`
						Match   struct {
							Listener struct {
								FilterChain struct {
									Filter struct {
										SubFilter struct {
											Name string `json:"name"`
										} `json:"subFilter"`
									} `json:"filter"`
								} `json:"filterChain"`
							} `json:"listener"`
						} `json:"match"`
						Patch struct {
							Operation string `json:"operation"`
							Value     struct {
								Name string `json:"name"`
							} `json:"value"`
						} `json:"patch"`
					} `json:"configPatches"`
				} `json:"spec"`
			}
			var bbr, cors *envoyFilter
			authPriority := map[string]int{} // inserted ext_authz filter name -> its EnvoyFilter priority
			for _, doc := range strings.Split(string(output), "\n---") {
				var ef envoyFilter
				if err := yaml.Unmarshal([]byte(doc), &ef); err != nil {
					t.Fatalf("parse rendered object: %v\n%s", err, doc)
				}
				if ef.Kind != "EnvoyFilter" {
					continue
				}
				switch ef.Metadata.Name {
				case "bbr-ext-proc":
					bbr = &ef
				case "gateway-cors":
					cors = &ef
				}
				for _, patch := range ef.Spec.ConfigPatches {
					if patch.ApplyTo == "HTTP_FILTER" && strings.Contains(patch.Patch.Value.Name, "ext_authz") {
						authPriority[patch.Patch.Value.Name] = ef.Spec.Priority
					}
				}
			}
			if bbr == nil {
				t.Fatal("bbr-ext-proc EnvoyFilter not rendered")
			}
			patch := bbr.Spec.ConfigPatches[0]
			if patch.Patch.Value.Name != "envoy.filters.http.ext_proc.bbr" {
				t.Fatalf("first bbr-ext-proc patch inserts %q, want the BBR filter", patch.Patch.Value.Name)
			}
			anchor := patch.Match.Listener.FilterChain.Filter.SubFilter.Name
			if tt.anchor == "" {
				if patch.Patch.Operation != "INSERT_FIRST" || anchor != "" {
					t.Errorf("BBR placement = %s %q, want INSERT_FIRST without an anchor", patch.Patch.Operation, anchor)
				}
				if len(authPriority) != 0 {
					t.Errorf("auth renders ext_authz filters %v, so BBR must follow them", authPriority)
				}
			} else {
				if patch.Patch.Operation != "INSERT_AFTER" || anchor != tt.anchor {
					t.Errorf("BBR placement = %s %q, want INSERT_AFTER %q", patch.Patch.Operation, anchor, tt.anchor)
				}
				// The anchor must be a filter the auth EnvoyFilters insert, applied
				// before BBR's patch, or Istio drops the BBR insertion silently.
				if priority, ok := authPriority[anchor]; !ok {
					t.Errorf("anchor %q is not inserted by any auth EnvoyFilter (inserted: %v)", anchor, authPriority)
				} else if bbr.Spec.Priority <= priority {
					t.Errorf("bbr-ext-proc priority %d must exceed the auth EnvoyFilter priority %d", bbr.Spec.Priority, priority)
				}
			}
			if cors != nil && bbr.Spec.Priority >= cors.Spec.Priority {
				t.Errorf("bbr-ext-proc priority %d must stay below gateway-cors %d so kaito.cors stays first",
					bbr.Spec.Priority, cors.Spec.Priority)
			}
		})
	}
}

// TestModelHarnessStripsRoutingHeadersBeforeBBR pins that the gateway ignores
// routing headers a client sets itself. BBR sets them only when the request
// body names a model, so a Lua filter directly before BBR must remove every
// header the modeldeployment HTTPRoute routes on.
func TestModelHarnessStripsRoutingHeadersBeforeBBR(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart rendering tests")
	}
	render := func(chart string, args ...string) []byte {
		t.Helper()
		args = append([]string{"template", "release", filepath.Join("..", "..", "..", "..", "charts", chart)}, args...)
		output, err := exec.Command("helm", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("helm template %s: %v\n%s", chart, err, output)
		}
		return output
	}

	var bbr struct {
		Spec struct {
			ConfigPatches []struct {
				ApplyTo string `json:"applyTo"`
				Match   struct {
					Listener struct {
						FilterChain struct {
							Filter struct {
								SubFilter struct {
									Name string `json:"name"`
								} `json:"subFilter"`
							} `json:"filter"`
						} `json:"filterChain"`
					} `json:"listener"`
				} `json:"match"`
				Patch struct {
					Operation string `json:"operation"`
					Value     struct {
						Name        string `json:"name"`
						TypedConfig struct {
							DefaultSourceCode struct {
								InlineString string `json:"inline_string"`
							} `json:"default_source_code"`
						} `json:"typed_config"`
					} `json:"value"`
				} `json:"patch"`
			} `json:"configPatches"`
		} `json:"spec"`
	}
	output := render("modelharness", "--set", "namespace=ns", "--show-only", "templates/envoyfilter-bbr.yaml")
	if err := yaml.Unmarshal(output, &bbr); err != nil {
		t.Fatalf("decode bbr-ext-proc EnvoyFilter: %v", err)
	}
	const bbrFilter = "envoy.filters.http.ext_proc.bbr"
	bbrInserted, lua := false, ""
	for _, p := range bbr.Spec.ConfigPatches {
		if p.ApplyTo != "HTTP_FILTER" {
			continue
		}
		switch p.Patch.Value.Name {
		case bbrFilter:
			bbrInserted = true
		case "kaito.strip-routing-headers":
			anchor := p.Match.Listener.FilterChain.Filter.SubFilter.Name
			if p.Patch.Operation != "INSERT_BEFORE" || anchor != bbrFilter {
				t.Errorf("strip filter placement = %s %q, want INSERT_BEFORE %q", p.Patch.Operation, anchor, bbrFilter)
			}
			// Istio applies the patches of one EnvoyFilter in order, so the
			// anchor exists only if the BBR insertion comes first.
			if !bbrInserted {
				t.Error("the strip filter patch precedes the BBR insertion it anchors on")
			}
			lua = p.Patch.Value.TypedConfig.DefaultSourceCode.InlineString
		}
	}
	if lua == "" {
		t.Fatal("bbr-ext-proc renders no kaito.strip-routing-headers filter")
	}

	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "default", args: []string{"--show-only", "templates/httproute.yaml"}},
		{name: "azure", args: []string{"--show-only", "templates/azure/httproute.yaml",
			"--set", "cloudprovider=azure", "--set", "azure.defaultDomain.zoneName=example.aksapp.io"}},
	} {
		var route struct {
			Spec struct {
				Rules []struct {
					Matches []struct {
						Headers []struct {
							Name string `json:"name"`
						} `json:"headers"`
					} `json:"matches"`
				} `json:"rules"`
			} `json:"spec"`
		}
		output := render("modeldeployment", append([]string{"--set", "model=test-model"}, tt.args...)...)
		if err := yaml.Unmarshal(output, &route); err != nil {
			t.Fatalf("%s: decode HTTPRoute: %v\n%s", tt.name, err, output)
		}
		routed := 0
		for _, rule := range route.Spec.Rules {
			for _, match := range rule.Matches {
				for _, h := range match.Headers {
					routed++
					if !strings.Contains(lua, fmt.Sprintf("%q", strings.ToLower(h.Name))) {
						t.Errorf("%s: the HTTPRoute routes on %s, which the strip filter does not remove", tt.name, h.Name)
					}
				}
			}
		}
		if routed == 0 {
			t.Errorf("%s: the HTTPRoute matches on no header", tt.name)
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
// consolidated outage local_reply: mapper order (first match wins) and the
// status each mapper keys on. Every mapper requires a 5xx, so a 4xx keeps its
// own status and body. With the model header present, the 503 an EPP returns
// on purpose because it cannot place a request (endpoint picker protocol) is
// model_unavailable and any other 5xx is epp_unavailable; with it absent,
// only the status BBR's ext_proc filter is pinned to is bbr_unavailable. The
// mappers must use only core AccessLogFilter types, so the patch needs no
// optional Envoy extension.
func TestModelHarnessOutageReplyMappers(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart rendering tests")
	}
	chart := filepath.Join("..", "..", "..", "..", "charts", "modelharness")
	render := func(template string) []byte {
		t.Helper()
		output, err := exec.Command("helm", "template", "harness", chart, "--set", "namespace=ns",
			"--show-only", template).CombinedOutput()
		if err != nil {
			t.Fatalf("helm template %s: %v\n%s", template, err, output)
		}
		return output
	}

	type filter struct {
		HeaderFilter *struct {
			Header struct {
				Name        string `json:"name"`
				InvertMatch bool   `json:"invert_match"`
			} `json:"header"`
		} `json:"header_filter"`
		ResponseFlagFilter *struct {
			Flags []string `json:"flags"`
		} `json:"response_flag_filter"`
		StatusCodeFilter *struct {
			Comparison struct {
				Op    string `json:"op"`
				Value struct {
					DefaultValue int `json:"default_value"`
				} `json:"value"`
			} `json:"comparison"`
		} `json:"status_code_filter"`
		ExtensionFilter *struct {
			Name string `json:"name"`
		} `json:"extension_filter"`
		AndFilter *struct {
			Filters []filter `json:"filters"`
		} `json:"and_filter"`
	}
	type envoyFilter struct {
		Spec struct {
			ConfigPatches []struct {
				Patch struct {
					Value struct {
						TypedConfig struct {
							StatusOnError *struct {
								Code json.RawMessage `json:"code"`
							} `json:"status_on_error"`
							LocalReplyConfig struct {
								Mappers []struct {
									Filter       filter `json:"filter"`
									StatusCode   int    `json:"status_code"`
									HeadersToAdd []struct {
										Header struct {
											Key   string `json:"key"`
											Value string `json:"value"`
										} `json:"header"`
									} `json:"headers_to_add"`
									Body struct {
										InlineString string `json:"inline_string"`
									} `json:"body"`
									BodyFormatOverride *struct {
										TextFormatSource struct {
											InlineString string `json:"inline_string"`
										} `json:"text_format_source"`
										ContentType string `json:"content_type"`
									} `json:"body_format_override"`
								} `json:"mappers"`
							} `json:"local_reply_config"`
						} `json:"typed_config"`
					} `json:"value"`
				} `json:"patch"`
			} `json:"configPatches"`
		} `json:"spec"`
	}

	// BBR's ext_proc status_on_error is what mapper #5 must match exactly.
	var bbr envoyFilter
	if err := yaml.Unmarshal(render("templates/envoyfilter-bbr.yaml"), &bbr); err != nil {
		t.Fatalf("decode BBR EnvoyFilter: %v", err)
	}
	bbrStatus := 0
	for _, p := range bbr.Spec.ConfigPatches {
		if p.Patch.Value.TypedConfig.StatusOnError == nil {
			continue
		}
		var code any
		if err := json.Unmarshal(p.Patch.Value.TypedConfig.StatusOnError.Code, &code); err != nil {
			t.Fatalf("decode BBR status_on_error: %v", err)
		}
		statusCodes := map[string]int{"InternalServerError": 500, "BadGateway": 502, "ServiceUnavailable": 503, "GatewayTimeout": 504}
		switch c := code.(type) {
		case float64:
			bbrStatus = int(c)
		case string:
			bbrStatus = statusCodes[c]
		}
	}
	if bbrStatus < 500 {
		t.Fatalf("BBR ext_proc status_on_error = %d, want a pinned 5xx (extend the status code names above if it is valid)", bbrStatus)
	}

	var ef envoyFilter
	if err := yaml.Unmarshal(render("templates/envoyfilter-outage-reply.yaml"), &ef); err != nil {
		t.Fatalf("decode outage EnvoyFilter: %v", err)
	}
	if len(ef.Spec.ConfigPatches) != 1 {
		t.Fatalf("expected 1 config patch, got %d", len(ef.Spec.ConfigPatches))
	}
	mappers := ef.Spec.ConfigPatches[0].Patch.Value.TypedConfig.LocalReplyConfig.Mappers

	for _, tt := range []struct {
		code        string
		status      int
		source      string
		modelHeader string // "present", "absent" or "" (not matched)
		routerFlag  bool
		match       string // status_code_filter, e.g. "GE 500", or "" (none)
		retryAfter  bool
	}{
		{code: "ext_authz_unavailable", status: 502, source: "authz", match: "GE 500"},
		{code: "model_unavailable", status: 503, source: "inferenceset", modelHeader: "present", routerFlag: true, match: "GE 500", retryAfter: true},
		{code: "gateway_unavailable", status: 502, source: "gateway", modelHeader: "absent", routerFlag: true, match: "GE 500"},
		{code: "model_unavailable", status: 503, source: "inferenceset", modelHeader: "present", match: "EQ 503", retryAfter: true},
		{code: "epp_unavailable", status: 502, source: "epp", modelHeader: "present", match: "GE 500"},
		{code: "bbr_unavailable", status: 502, source: "bbr", modelHeader: "absent", match: fmt.Sprintf("EQ %d", bbrStatus)},
		{code: "gateway_internal_error", source: "gateway", match: "GE 500"},
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
		// Envoy keeps a content-type set through headers_to_add only if it
		// differs from the one the local reply already had, and an EPP
		// ImmediateResponse already carries application/json; otherwise it
		// uses the formatter's, so the formatter must set it.
		if o := m.BodyFormatOverride; o == nil || o.ContentType != "application/json" ||
			o.TextFormatSource.InlineString != "%LOCAL_REPLY_BODY%" {
			t.Errorf("%s: body_format_override = %+v, want the mapper body with content_type application/json", tt.code, o)
		}
		filters := []filter{m.Filter}
		if m.Filter.AndFilter != nil {
			filters = m.Filter.AndFilter.Filters
		}
		modelHeader, routerFlag, match := "", false, ""
		for _, f := range filters {
			switch {
			case f.HeaderFilter != nil && f.HeaderFilter.Header.Name == "X-Gateway-Model-Name":
				modelHeader = "present"
				if f.HeaderFilter.Header.InvertMatch {
					modelHeader = "absent"
				}
			case f.ResponseFlagFilter != nil && slices.Contains(f.ResponseFlagFilter.Flags, "UH"):
				routerFlag = true
			case f.StatusCodeFilter != nil:
				match = fmt.Sprintf("%s %d", f.StatusCodeFilter.Comparison.Op, f.StatusCodeFilter.Comparison.Value.DefaultValue)
			case f.ExtensionFilter != nil:
				t.Errorf("%s: extension filter %q: the mappers must use only core AccessLogFilter types",
					tt.code, f.ExtensionFilter.Name)
			}
		}
		if modelHeader != tt.modelHeader || routerFlag != tt.routerFlag || match != tt.match {
			t.Errorf("%s: match (model header %q, router flag %v, status %q), want (%q, %v, %q)",
				tt.code, modelHeader, routerFlag, match, tt.modelHeader, tt.routerFlag, tt.match)
		}
	}
	if len(mappers) != 0 {
		t.Errorf("%d unexpected trailing mappers", len(mappers))
	}
}

// TestModelDeploymentHTTPRouteSetsNoErrorSource pins that responses proxied
// from the inference pods carry no x-kaito-error-source. The header marks only
// errors the gateway generates itself (the modelharness local_reply mappers and
// catch-all routes), so the model's HTTPRoute must not stamp it on every
// response, successes included.
func TestModelDeploymentHTTPRouteSetsNoErrorSource(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart rendering tests")
	}
	chart := filepath.Join("..", "..", "..", "..", "charts", "modeldeployment")
	for _, tt := range []struct {
		name     string
		template string
		values   []string
	}{
		{name: "default", template: "templates/httproute.yaml"},
		{name: "azure", template: "templates/azure/httproute.yaml",
			values: []string{"cloudprovider=azure", "azure.defaultDomain.zoneName=example.aksapp.io"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"template", "model", chart, "--show-only", tt.template, "--set", "model=test-model"}
			for _, value := range tt.values {
				args = append(args, "--set", value)
			}
			output, err := exec.Command("helm", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, output)
			}
			type header struct {
				Name string `json:"name"`
			}
			var route struct {
				Kind string `json:"kind"`
				Spec struct {
					Rules []struct {
						Filters []struct {
							ResponseHeaderModifier *struct {
								Set []header `json:"set"`
								Add []header `json:"add"`
							} `json:"responseHeaderModifier"`
						} `json:"filters"`
					} `json:"rules"`
				} `json:"spec"`
			}
			if err := yaml.Unmarshal(output, &route); err != nil {
				t.Fatalf("decode HTTPRoute: %v\n%s", err, output)
			}
			if route.Kind != "HTTPRoute" || len(route.Spec.Rules) == 0 {
				t.Fatalf("expected an HTTPRoute with rules, got kind %q with %d rules", route.Kind, len(route.Spec.Rules))
			}
			for _, rule := range route.Spec.Rules {
				for _, filter := range rule.Filters {
					if filter.ResponseHeaderModifier == nil {
						continue
					}
					for _, h := range append(filter.ResponseHeaderModifier.Set, filter.ResponseHeaderModifier.Add...) {
						if strings.EqualFold(h.Name, "x-kaito-error-source") {
							t.Errorf("the HTTPRoute stamps %s on every proxied response, successes included", h.Name)
						}
					}
				}
			}
		})
	}
}

// TestPriorityClassName pins the PriorityClass of the pods that share CPU nodes
// with the namespace gateway; an empty value omits the field. See
// charts/productionstack/README.md#pod-priority.
func TestPriorityClassName(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart rendering tests")
	}
	charts := filepath.Join("..", "..", "..", "..", "charts")
	subcharts := filepath.Join(charts, "productionstack", "charts")
	for _, tt := range []struct {
		name  string
		chart string
		key   string
		set   []string
		// shadowPodKey also sets the class of the shadow pods the mocker creates.
		shadowPodKey string
	}{
		{name: "epp", chart: filepath.Join(charts, "modeldeployment"), key: "epp.priorityClassName", set: []string{"model=test-model"}},
		{name: "bbr", chart: filepath.Join(subcharts, "body-based-routing"), key: "bbr.priorityClassName"},
		{name: "status-reporter", chart: filepath.Join(subcharts, "productionstack-status-reporter"), key: "priorityClassName"},
		{name: "keda-kaito-scaler", chart: filepath.Join(subcharts, "keda-kaito-scaler"), key: "priorityClassName"},
		{name: "gpu-node-mocker", chart: filepath.Join(charts, "gpu-node-mocker"), key: "priorityClassName",
			shadowPodKey: "shadowPod.priorityClassName"},
	} {
		for _, want := range []string{"system-cluster-critical", ""} {
			t.Run(tt.name+"/"+cmp.Or(want, "empty"), func(t *testing.T) {
				args := []string{"release", tt.chart}
				for _, value := range tt.set {
					args = append(args, "--set", value)
				}
				if want == "" {
					args = append(args, "--set", tt.key+"=")
					if tt.shadowPodKey != "" {
						args = append(args, "--set", tt.shadowPodKey+"=")
					}
				}
				deployments := objectsOfKind[appsv1.Deployment](t, renderChart(t, args...), "Deployment")
				if len(deployments) != 1 {
					t.Fatalf("expected 1 Deployment, got %d", len(deployments))
				}
				pod := deployments[0].Spec.Template.Spec
				if pod.PriorityClassName != want {
					t.Errorf("priorityClassName = %q, want %q", pod.PriorityClassName, want)
				}
				if tt.shadowPodKey == "" {
					return
				}
				var flags []string
				for _, arg := range pod.Containers[0].Args {
					if strings.HasPrefix(arg, "--shadow-pod-priority-class-name") {
						flags = append(flags, arg)
					}
				}
				var wantFlags []string
				if want != "" {
					wantFlags = []string{"--shadow-pod-priority-class-name=" + want}
				}
				if !slices.Equal(flags, wantFlags) {
					t.Errorf("shadow pod class flags = %q, want %q", flags, wantFlags)
				}
			})
		}
	}
}

// TestBodyBasedRoutingPodDisruptionBudget pins BBR's PodDisruptionBudget.
// Voluntary disruptions, such as Karpenter consolidation, must evict the BBR
// replicas one at a time so one keeps serving ext_proc, and the budget does so
// only if it selects the BBR pods in their namespace.
func TestBodyBasedRoutingPodDisruptionBudget(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart rendering tests")
	}
	chart := filepath.Join("..", "..", "..", "..", "charts", "productionstack", "charts", "body-based-routing")
	docs := renderChart(t, "bbr", chart, "--namespace", "release-ns", "--set", "namespaceOverride=bbr-ns")
	deployments := objectsOfKind[appsv1.Deployment](t, docs, "Deployment")
	pdbs := objectsOfKind[policyv1.PodDisruptionBudget](t, docs, "PodDisruptionBudget")
	if len(deployments) != 1 || len(pdbs) != 1 {
		t.Fatalf("expected 1 Deployment and 1 PodDisruptionBudget, got %d and %d", len(deployments), len(pdbs))
	}
	bbr, pdb := deployments[0], pdbs[0]
	if pdb.Spec.MinAvailable != nil || pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.String() != "1" {
		t.Errorf("want maxUnavailable 1 and no minAvailable, got maxUnavailable=%v minAvailable=%v",
			pdb.Spec.MaxUnavailable, pdb.Spec.MinAvailable)
	}
	selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		t.Fatalf("PodDisruptionBudget selector: %v", err)
	}
	if !selector.Matches(labels.Set(bbr.Spec.Template.Labels)) {
		t.Errorf("PodDisruptionBudget selector %q does not select the BBR pods %v", selector, bbr.Spec.Template.Labels)
	}
	if pdb.Namespace != bbr.Namespace {
		t.Errorf("PodDisruptionBudget namespace = %q, BBR runs in %q", pdb.Namespace, bbr.Namespace)
	}
}

// renderChart runs helm template with args and returns the rendered documents.
func renderChart(t *testing.T, args ...string) []string {
	t.Helper()
	output, err := exec.Command("helm", append([]string{"template"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.Split(string(output), "\n---")
}

// objectsOfKind decodes the rendered documents of one kind.
func objectsOfKind[T any](t *testing.T, docs []string, kind string) []T {
	t.Helper()
	var objects []T
	for _, doc := range docs {
		var meta metav1.TypeMeta
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatalf("decode rendered document: %v\n%s", err, doc)
		}
		if meta.Kind != kind {
			continue
		}
		var obj T
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("decode %s: %v\n%s", kind, err, doc)
		}
		objects = append(objects, obj)
	}
	return objects
}
