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
// cluster reasons (shared control-plane Deployment readiness).
package cluster

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/config"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
	"github.com/kaito-project/production-stack/pkg/util/kube"
)

// Evaluator probes every cluster-layer reason (§1.2): the readiness of the
// shared control-plane Deployments.
type Evaluator struct {
	clientset kubernetes.Interface
	cfg       config.Config
}

// New constructs a cluster Evaluator.
func New(cs kubernetes.Interface, cfg config.Config) *Evaluator {
	return &Evaluator{clientset: cs, cfg: cfg}
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
				GroupKey: "cluster/" + chk.namespace,
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
				GroupKey:          "cluster/" + chk.namespace,
				ResourceCreatedAt: notReadySince,
			})
		}
	}

	return findings, nil
}
