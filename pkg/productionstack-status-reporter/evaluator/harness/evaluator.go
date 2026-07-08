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

// Package harness implements the modelharness-layer Evaluator: it probes the
// §1.2 modelharness reasons across every managed namespace.
package harness

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/config"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator/util"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
	"github.com/kaito-project/production-stack/pkg/util/kube"
)

// GroupVersionResources for the CRDs the harness evaluator reads read-only.
// Gateway API, KAITO, Istio and Cilium types are not vendored, so they are
// consumed via the dynamic client as unstructured objects.
var (
	gatewayGVR     = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}
	apiKeyGVR      = schema.GroupVersionResource{Group: "kaito.sh", Version: "v1alpha1", Resource: "apikeys"}
	envoyFilterGVR = schema.GroupVersionResource{Group: "networking.istio.io", Version: "v1alpha3", Resource: "envoyfilters"}
	authPolicyGVR  = schema.GroupVersionResource{Group: "security.istio.io", Version: "v1", Resource: "authorizationpolicies"}
	// ciliumNetworkPolicyGVR is a best-effort probe target; a missing Cilium CRD
	// simply yields no finding (Cilium is an optional dependency).
	ciliumNetworkPolicyGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}
)

// gatewayProgrammingGracePeriod is the startup-grace window applied to
// ModelharnessGatewayProgrammingFailed. Gateway programming waits on the Istio
// auto-provisioned Envoy data plane (and, for LoadBalancer Services, cloud
// load-balancer address assignment), which legitimately takes several minutes
// on a fresh install or when a node must be scaled up — much longer than the
// global StartupGracePeriod tuned for long-lived control-plane pods.
const gatewayProgrammingGracePeriod = 5 * time.Minute

// requiredEnvoyFilter names a per-namespace EnvoyFilter that charts/modelharness
// renders on the Gateway HCM, together with a short description of its role.
type requiredEnvoyFilter struct {
	name    string
	purpose string
}

// unconditionalEnvoyFilters are the per-namespace EnvoyFilters charts/modelharness
// always renders on the Gateway HCM, regardless of chart values.
var unconditionalEnvoyFilters = []requiredEnvoyFilter{
	{"bbr-ext-proc", "routes by request body via Body-Based Routing ext_proc"},
	{"model-not-found-direct", "returns an OpenAI-format 404 for unknown models"},
	{"gateway-filter-outage-local-reply", "maps gateway 5xx local replies onto the unified error envelope"},
}

// apikeyEnvoyFilter is rendered only when API-key auth is enabled (same
// `auth.enabled` guard as the APIKey CR).
var apikeyEnvoyFilter = requiredEnvoyFilter{
	name:    "apikey-ext-authz",
	purpose: "enforces API-key authentication on the Gateway",
}

// Evaluator probes every modelharness-layer reason (§1.2) across all managed
// namespaces.
type Evaluator struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
	cfg       config.Config
}

// New constructs a harness Evaluator.
func New(cs kubernetes.Interface, dyn dynamic.Interface, cfg config.Config) *Evaluator {
	return &Evaluator{clientset: cs, dynamic: dyn, cfg: cfg}
}

// Name identifies the evaluator for logging.
func (e *Evaluator) Name() string { return "modelharness" }

// Evaluate probes every modelharness-layer reason across all managed
// namespaces. It returns an error only when namespace discovery fails.
func (e *Evaluator) Evaluate(ctx context.Context) ([]evaluator.Finding, error) {
	namespaces, err := util.DiscoverNamespaces(ctx, e.clientset)
	if err != nil {
		return nil, err
	}
	var findings []evaluator.Finding
	for _, ns := range namespaces {
		findings = append(findings, e.evaluateNamespace(ctx, ns)...)
	}
	return findings, nil
}

// evaluateNamespace probes every modelharness-layer reason (§1.2) for a single
// workload namespace.
func (e *Evaluator) evaluateNamespace(ctx context.Context, namespace string) []evaluator.Finding {
	var findings []evaluator.Finding
	obj := evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: namespace}
	groupKey := "harness/" + namespace

	add := func(r reason.Reason, msg string, createdAt time.Time) {
		findings = append(findings, evaluator.Finding{
			Reason:            r,
			Object:            obj,
			Message:           msg,
			WorkloadNamespace: namespace,
			GroupKey:          groupKey,
			ResourceCreatedAt: createdAt,
		})
	}

	// Gateways: Accepted / Programmed conditions.
	if gws, err := e.dynamic.Resource(gatewayGVR).Namespace(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for i := range gws.Items {
			gw := &gws.Items[i]
			if st, rsn, msg, ok := kube.ConditionStatus(gw, "Accepted"); ok && st == "False" {
				if rsn == "NoMatchingParent" || rsn == "InvalidParameters" || rsn == "UnsupportedValue" {
					add(reason.ModelharnessGatewayClassMissing, fmt.Sprintf(
						"Namespace %s: Gateway %s not accepted (%s): %s; check spec.gatewayClassName.",
						namespace, gw.GetName(), rsn, msg), gw.GetCreationTimestamp().Time)
				}
			}
			if st, rsn, msg, ok := kube.ConditionStatus(gw, "Programmed"); ok && st == "False" {
				findings = append(findings, evaluator.Finding{
					Reason: reason.ModelharnessGatewayProgrammingFailed,
					Object: obj,
					Message: fmt.Sprintf(
						"Namespace %s: Gateway %s programming failed (%s): %s.",
						namespace, gw.GetName(), rsn, msg),
					WorkloadNamespace:   namespace,
					GroupKey:            groupKey,
					ResourceCreatedAt:   gw.GetCreationTimestamp().Time,
					GracePeriodOverride: gatewayProgrammingGracePeriod,
				})
			} else if ok && st == "True" {
				// istiod marks Programmed=True as soon as it has created the
				// auto-provisioned Deployment/Service and assigned an address —
				// it does NOT wait for the Envoy pod to be scheduled/Ready. A
				// Pending/unschedulable data-plane pod therefore leaves every CR
				// condition green while no traffic can flow, so verify the pod
				// directly to close that control-plane-green / data-plane-down gap.
				e.evaluateGatewayDataPlane(ctx, namespace, gw.GetName(), obj, groupKey, &findings)
			}
		}
	}

	// AuthorizationPolicy ext_authz provider must match a registered
	// MeshConfig.extensionProviders entry.
	e.evaluateExtAuthzProvider(ctx, namespace, add)

	// APIKey CR reconcile status. The presence of an APIKey CR also signals
	// that API-key auth is enabled for this namespace: charts/modelharness
	// renders both the APIKey CR and the `apikey-ext-authz` EnvoyFilter under
	// the same `auth.enabled` guard, so the CR is a decoupled auth signal.
	authEnabled := false
	if keys, err := e.dynamic.Resource(apiKeyGVR).Namespace(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for i := range keys.Items {
			authEnabled = true
			k := &keys.Items[i]
			if st, rsn, msg, ok := kube.ConditionStatus(k, "Ready"); ok && st == "False" {
				add(reason.ModelharnessAPIKeyReconcileFailed, fmt.Sprintf(
					"Namespace %s: APIKey %s reconcile failed (%s): %s.",
					namespace, k.GetName(), rsn, msg), k.GetCreationTimestamp().Time)
			}
		}
	}

	// Required per-namespace EnvoyFilters. charts/modelharness provisions a set
	// of dataplane filters on the namespace Gateway HCM; EnvoyFilter carries no
	// status conditions, so absence (chart not fully installed / drift) is the
	// only reliable signal. A missing startup window is debounced via the
	// startup grace gate.
	if filters, err := e.dynamic.Resource(envoyFilterGVR).Namespace(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		present := make(map[string]bool, len(filters.Items))
		for i := range filters.Items {
			present[filters.Items[i].GetName()] = true
		}
		// Unconditionally rendered by charts/modelharness; the apikey filter is
		// added only when API-key auth is enabled (same guard as the APIKey CR).
		required := unconditionalEnvoyFilters
		if authEnabled {
			required = append(required, apikeyEnvoyFilter)
		}
		var missing []string
		for _, rf := range required {
			if !present[rf.name] {
				missing = append(missing, fmt.Sprintf("%s (%s)", rf.name, rf.purpose))
			}
		}
		if len(missing) > 0 {
			add(reason.ModelharnessEnvoyFilterMissing, fmt.Sprintf(
				"Namespace %s: required modelharness EnvoyFilter(s) missing: %s; re-apply charts/modelharness.",
				namespace, strings.Join(missing, "; ")), time.Time{})
		}
	}

	// NetworkPolicy referencing nonexistent ingress namespaces.
	e.evaluateNetworkPolicy(ctx, namespace, add)

	return findings
}

// gatewayDataPlaneLabel is the standard Gateway API label Istio stamps on the
// auto-provisioned data-plane Deployment/Service/Pods for a Gateway, letting us
// locate the backing Deployment by the Gateway's name.
const gatewayDataPlaneLabel = "gateway.networking.k8s.io/gateway-name"

// harnessOwnedBySelector is the stable ownership label charts/modelharness
// stamps on the Gateway (modelharness.labels). Istio propagates a Gateway's
// labels onto the resources it auto-provisions, so pairing this with the
// gateway-name label scopes the Deployment lookup to the modelharness-owned
// data plane and never matches a user-created Gateway that happens to share a
// name — the same ownership label the reporter keys off elsewhere.
const harnessOwnedBySelector = "kaito.sh/owned-by=modelharness"

// evaluateGatewayDataPlane verifies the Envoy data plane Istio auto-provisions
// for a Programmed Gateway is actually running: it locates the backing
// Deployment(s) by the Gateway-name + modelharness ownership labels and flags a
// Pending/unschedulable or not-ready pod as modelharnessGatewayDataPlaneNotReady.
// The grace override matches modelharnessGatewayProgrammingFailed because a
// fresh gateway pod may legitimately take minutes to schedule (e.g. while a
// node scales up).
func (e *Evaluator) evaluateGatewayDataPlane(ctx context.Context, namespace, gwName string, obj evaluator.InvolvedObject, groupKey string, findings *[]evaluator.Finding) {
	deps, err := e.clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s", gatewayDataPlaneLabel, gwName, harnessOwnedBySelector),
	})
	if err != nil {
		return
	}
	for i := range deps.Items {
		dep := &deps.Items[i]
		missing, unavailable, cause, since := kube.DeploymentPodUnavailable(ctx, e.clientset, namespace, dep.Name)
		if missing || !unavailable {
			continue
		}
		*findings = append(*findings, evaluator.Finding{
			Reason: reason.ModelharnessGatewayDataPlaneNotReady,
			Object: obj,
			Message: fmt.Sprintf(
				"Namespace %s: Gateway %s is Programmed but its data-plane pod is not running (%s); check scheduling/quota/nodes.",
				namespace, gwName, cause),
			WorkloadNamespace:   namespace,
			GroupKey:            groupKey,
			ResourceCreatedAt:   since,
			GracePeriodOverride: gatewayProgrammingGracePeriod,
		})
	}
}

// evaluateExtAuthzProvider compares every AuthorizationPolicy CUSTOM provider
// reference in the namespace against the cluster MeshConfig.extensionProviders
// registry (read from the istio ConfigMap). A provider that is not registered
// is a local chart misconfiguration → modelharnessExtAuthzProviderMissing.
func (e *Evaluator) evaluateExtAuthzProvider(ctx context.Context, namespace string, add func(reason.Reason, string, time.Time)) {
	registered := meshExtensionProviders(ctx, e.clientset, e.cfg)
	if registered == nil {
		return // MeshConfig unavailable — cannot evaluate, skip.
	}
	policies, err := e.dynamic.Resource(authPolicyGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	for i := range policies.Items {
		ap := &policies.Items[i]
		provider := kube.NestedString(ap.Object, "spec", "provider", "name")
		if provider == "" {
			continue
		}
		if !registered[provider] {
			add(reason.ModelharnessExtAuthzProviderMissing, fmt.Sprintf(
				"Namespace %s: AuthorizationPolicy '%s' references extension provider '%s' which is not registered in MeshConfig.extensionProviders; re-apply charts/modelharness with the correct providerName.",
				namespace, ap.GetName(), provider), ap.GetCreationTimestamp().Time)
		}
	}
}

// meshExtensionProviders reads the registered ext_authz / ext_proc provider
// names from the istio MeshConfig ConfigMap. Returns nil when the ConfigMap is
// unavailable.
func meshExtensionProviders(ctx context.Context, cs kubernetes.Interface, cfg config.Config) map[string]bool {
	cm, err := cs.CoreV1().ConfigMaps(cfg.IstioNamespace).Get(ctx, "istio", metav1.GetOptions{})
	if err != nil {
		return nil
	}
	meshYAML, ok := cm.Data["mesh"]
	if !ok {
		return nil
	}
	var mesh struct {
		ExtensionProviders []struct {
			Name string `json:"name"`
		} `json:"extensionProviders"`
	}
	if err := yaml.Unmarshal([]byte(meshYAML), &mesh); err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, p := range mesh.ExtensionProviders {
		if p.Name != "" {
			out[p.Name] = true
		}
	}
	return out
}

// evaluateNetworkPolicy verifies that the per-namespace CiliumNetworkPolicy
// `inference-pods-ingress` that charts/modelharness renders is present. The
// policy default-denies East-West ingress to inference pods, so its absence
// (chart not fully installed / drift, or the Cilium reconcile never compiled
// it) is a security regression. CiliumNetworkPolicy carries no readiness
// condition, so existence is the only reliable signal; a missing startup
// window is debounced via the startup grace gate.
func (e *Evaluator) evaluateNetworkPolicy(ctx context.Context, namespace string, add func(reason.Reason, string, time.Time)) {
	policies, err := e.dynamic.Resource(ciliumNetworkPolicyGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return // Cilium CRD absent or list failed — best effort, skip.
	}
	for i := range policies.Items {
		if policies.Items[i].GetName() == "inference-pods-ingress" {
			return
		}
	}
	add(reason.ModelharnessNetworkPolicyMissing, fmt.Sprintf(
		"Namespace %s: CiliumNetworkPolicy inference-pods-ingress is missing; re-apply charts/modelharness to restore East-West ingress isolation for inference pods.",
		namespace), time.Time{})
}
