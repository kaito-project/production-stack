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

// Package cluster implements the cluster-layer Evaluator: it probes the §1.2
// cluster reasons (missing CRDs and shared control-plane Deployment readiness).
package cluster

import (
	"context"
	"fmt"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/config"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
	"github.com/kaito-project/production-stack/pkg/util/kube"
)

// Evaluator probes every cluster-layer reason (§1.2): missing CRDs and the
// readiness of the shared control-plane Deployments.
type Evaluator struct {
	clientset kubernetes.Interface
	discovery discovery.DiscoveryInterface
	cfg       config.Config
}

// New constructs a cluster Evaluator.
func New(cs kubernetes.Interface, dc discovery.DiscoveryInterface, cfg config.Config) *Evaluator {
	return &Evaluator{clientset: cs, discovery: dc, cfg: cfg}
}

// Name identifies the evaluator for logging.
func (e *Evaluator) Name() string { return "cluster" }

// Evaluate probes every cluster-layer reason (§1.2). It never returns an error
// for individual probe failures — a probe that cannot be evaluated is treated
// as "unknown" (not emitted) so a transient API hiccup does not flap the event
// stream.
func (e *Evaluator) Evaluate(ctx context.Context) ([]evaluator.Finding, error) {
	var findings []evaluator.Finding
	// active deduplicates findings so each reason is reported at most once.
	active := map[reason.Reason]bool{}

	// clusterCRDMissing — one finding per missing CRD (involvedObject = CRD).
	for _, crd := range missingCRDs(e.discovery) {
		active[reason.ClusterCRDMissing] = true
		findings = append(findings, evaluator.Finding{
			Reason:   reason.ClusterCRDMissing,
			Object:   evaluator.InvolvedObject{Kind: evaluator.KindCRD, Name: crd.Name},
			Message:  fmt.Sprintf("required CustomResourceDefinition %s is not registered with the API server; install the chart that ships it.", crd.Name),
			GroupKey: "crd/" + crd.Name,
		})
	}

	// Deployment-readiness based cluster reasons.
	type depReason struct {
		r         reason.Reason
		namespace string
		name      string
		component string
	}
	cfg := e.cfg
	checks := []depReason{
		{reason.ClusterIstioControlPlaneNotReady, cfg.IstioNamespace, cfg.IstiodDeployment, "istiod control plane"},
		{reason.ClusterGatewayAuthNotReady, cfg.GatewayAuthNamespace, cfg.GatewayAuthDeployment, "llm-gateway-auth ext_authz"},
		{reason.ClusterBBRNotReady, cfg.BBRNamespace, cfg.BBRDeployment, "body-based-routing"},
		{reason.ClusterKaitoControllerNotReady, cfg.KaitoNamespace, cfg.KaitoDeployment, "KAITO workspace controller"},
		{reason.ClusterKedaKaitoScalerNotReady, cfg.KedaScalerNamespace, cfg.KedaScalerDeployment, "keda-kaito-scaler"},
		{reason.ClusterKedaNotReady, cfg.KedaNamespace, "keda-operator", "KEDA control-plane"},
		{reason.ClusterKedaNotReady, cfg.KedaNamespace, "keda-operator-metrics-apiserver", "KEDA control-plane"},
	}
	// clusterNodeProvisionerNotReady — checked only when a provisioner is registered.
	if cfg.NodeProvisioner.Name != "" {
		checks = append(checks, depReason{
			reason.ClusterNodeProvisionerNotReady, cfg.NodeProvisioner.Namespace, cfg.NodeProvisioner.Name, "node-provisioner",
		})
	}
	for _, chk := range checks {
		if active[chk.r] {
			continue // one finding per reason is enough
		}
		missing, notReady, cause, notReadySince := kube.DeploymentPodUnavailable(ctx, e.clientset, chk.namespace, chk.name)
		switch {
		case missing:
			// Deployment deleted — debounced via the startup grace gate so a
			// chart upgrade/reinstall does not flap the event stream.
			active[chk.r] = true
			findings = append(findings, evaluator.Finding{
				Reason:   chk.r,
				Object:   evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: chk.namespace},
				Message:  fmt.Sprintf("%s Deployment %s/%s is missing; install/upgrade the chart that ships it.", chk.component, chk.namespace, chk.name),
				GroupKey: "cluster/" + string(chk.r),
			})
		case notReady:
			// A pod's Ready condition is False. Debounce on how long it has been
			// not-ready (ResourceCreatedAt = Ready lastTransitionTime): a rolling
			// upgrade recovers within the grace window and stays silent, while a
			// genuinely stuck pod surfaces once it persists past the window.
			active[chk.r] = true
			findings = append(findings, evaluator.Finding{
				Reason:            chk.r,
				Object:            evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: chk.namespace},
				Message:           fmt.Sprintf("%s pod in %s/%s is not ready: %s.", chk.component, chk.namespace, chk.name, cause),
				GroupKey:          "cluster/" + string(chk.r),
				ResourceCreatedAt: notReadySince,
			})
		}
	}

	return findings, nil
}

// requiredCRD describes one CRD the production stack depends on. The CRD set
// is derived from what charts/productionstack, charts/modelharness and
// charts/modeldeployment render or reference, including RBAC grants for
// runtime informers (§1.2 clusterCRDMissing).
type requiredCRD struct {
	// Name is the CRD object name (resource.group), used as the
	// involvedObject for clusterCRDMissing and the suppression key.
	Name         string
	GroupVersion string
	Resource     string
}

// requiredCRDs is the closed set probed for clusterCRDMissing.
var requiredCRDs = []requiredCRD{
	// Gateway API.
	{Name: "gateways.gateway.networking.k8s.io", GroupVersion: "gateway.networking.k8s.io/v1", Resource: "gateways"},
	{Name: "httproutes.gateway.networking.k8s.io", GroupVersion: "gateway.networking.k8s.io/v1", Resource: "httproutes"},
	// Gateway API Inference Extension (GAIE).
	{Name: "inferencepools.inference.networking.k8s.io", GroupVersion: "inference.networking.k8s.io/v1", Resource: "inferencepools"},
	// KAITO.
	{Name: "inferencesets.kaito.sh", GroupVersion: "kaito.sh/v1beta1", Resource: "inferencesets"},
	{Name: "workspaces.kaito.sh", GroupVersion: "kaito.sh/v1beta1", Resource: "workspaces"},
	{Name: "apikeys.kaito.sh", GroupVersion: "kaito.sh/v1alpha1", Resource: "apikeys"},
	// Istio.
	{Name: "envoyfilters.networking.istio.io", GroupVersion: "networking.istio.io/v1alpha3", Resource: "envoyfilters"},
	{Name: "authorizationpolicies.security.istio.io", GroupVersion: "security.istio.io/v1", Resource: "authorizationpolicies"},
	// KEDA.
	{Name: "scaledobjects.keda.sh", GroupVersion: "keda.sh/v1alpha1", Resource: "scaledobjects"},
	{Name: "clustertriggerauthentications.keda.sh", GroupVersion: "keda.sh/v1alpha1", Resource: "clustertriggerauthentications"},
}

// missingCRDs returns the required CRDs that are not yet registered with the
// API server, discovered via the discovery API (which does not require the
// CRD types to be served — only advertised).
func missingCRDs(dc discovery.DiscoveryInterface) []requiredCRD {
	// Cache resource lists per groupVersion to avoid duplicate calls.
	served := map[string]map[string]bool{}
	var missing []requiredCRD
	for _, crd := range requiredCRDs {
		set, ok := served[crd.GroupVersion]
		if !ok {
			set = map[string]bool{}
			if list, err := dc.ServerResourcesForGroupVersion(crd.GroupVersion); err == nil {
				for _, api := range list.APIResources {
					set[api.Name] = true
				}
			}
			served[crd.GroupVersion] = set
		}
		if !set[crd.Resource] {
			missing = append(missing, crd)
		}
	}
	return missing
}
