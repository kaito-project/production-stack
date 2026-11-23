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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// Evaluate probes every cluster-layer reason (§1.2). Only the readiness of a
// component that exists is checked: the deploying party guarantees that every
// component it installs exists, so a missing Deployment or DaemonSet is not
// reported, which also keeps a component that is not installed on this cluster
// from being flagged. It never returns an error for individual probe failures —
// a probe that cannot be evaluated is treated as "unknown" (not emitted) so a
// transient API hiccup does not flap the event stream.
func (e *Evaluator) Evaluate(ctx context.Context) ([]evaluator.Finding, error) {
	var findings []evaluator.Finding
	// reported indexes the finding of each reason, so each reason is reported
	// at most once.
	reported := map[reason.Reason]int{}
	add := func(f evaluator.Finding) {
		i, ok := reported[f.Reason]
		switch {
		case !ok:
			reported[f.Reason] = len(findings)
			findings = append(findings, f)
		case findings[i].AwaitingNode && !f.AwaitingNode:
			// Several components share some reasons. A pod still waiting for a
			// node, whose startup grace is longer, gives way to a component
			// that is not, so the wait never hides one that is failing;
			// otherwise the first component checked is reported.
			findings[i] = f
		}
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
		{reason.ClusterBBRNotReady, cfg.BBRNamespace, cfg.BBRDeployment, "body-based-routing"},
		{reason.ClusterKaitoControllerNotReady, cfg.KaitoNamespace, cfg.KaitoDeployment, "KAITO workspace controller"},
		{reason.ClusterKedaKaitoScalerNotReady, cfg.KedaScalerNamespace, cfg.KedaScalerDeployment, "keda-kaito-scaler"},
		{reason.ClusterKedaNotReady, cfg.KedaNamespace, "keda-operator", "KEDA control-plane"},
		{reason.ClusterKedaNotReady, cfg.KedaNamespace, "keda-operator-metrics-apiserver", "KEDA control-plane"},
	}
	// clusterGatewayAuthNotReady — the llm-gateway-auth components. Which
	// Deployments exist depends on the per-namespace auth.mode the gateway-auth
	// subcharts were installed for: apikey-authz and apikey-operator
	// (apikey/hybrid) and/or azure-authz (azure/hybrid). They share this one
	// reason; an absent one is not reported, and add reports the shared reason
	// at most once.
	if cfg.GatewayAuthDeployment != "" {
		checks = append(checks, depReason{
			reason.ClusterGatewayAuthNotReady, cfg.GatewayAuthNamespace, cfg.GatewayAuthDeployment, "apikey-authz ext_authz",
		})
	}
	if cfg.GatewayAuthOperatorDeployment != "" {
		checks = append(checks, depReason{
			reason.ClusterGatewayAuthNotReady, cfg.GatewayAuthNamespace, cfg.GatewayAuthOperatorDeployment, "apikey-operator",
		})
	}
	if cfg.AzureAuthz.Name != "" {
		checks = append(checks, depReason{
			reason.ClusterGatewayAuthNotReady, cfg.AzureAuthz.Namespace, cfg.AzureAuthz.Name, "azure-authz ext_authz",
		})
	}
	// clusterLocalCSIDriverNotReady — the manager half of the local CSI driver;
	// the node DaemonSet half is probed below.
	if cfg.LocalCSIDriver.Namespace != "" && cfg.LocalCSIDriver.ManagerDeployment != "" {
		checks = append(checks, depReason{
			reason.ClusterLocalCSIDriverNotReady, cfg.LocalCSIDriver.Namespace, cfg.LocalCSIDriver.ManagerDeployment, "local CSI driver manager",
		})
	}
	for _, chk := range checks {
		// A missing Deployment is not reported (see Evaluate).
		if u, notReady := kube.DeploymentPodUnavailable(ctx, e.clientset, chk.namespace, chk.name); notReady {
			// A pod's Ready condition is False, a pod is still Pending, or the
			// Deployment is scaled to zero. Debounce on how long it has been
			// unavailable (ResourceCreatedAt): a rolling upgrade recovers within
			// the grace window and stays silent, while a genuinely stuck pod
			// surfaces once it persists past the window.
			add(evaluator.Finding{
				Reason:            chk.r,
				Object:            evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: chk.namespace},
				Message:           fmt.Sprintf("%s pod in %s/%s is not ready: %s.", chk.component, chk.namespace, chk.name, u.Cause),
				GroupKey:          "cluster/" + chk.namespace,
				ResourceCreatedAt: u.Since,
				AwaitingNode:      u.AwaitingNode,
			})
		}
	}

	if f, ok := e.localCSINodeFinding(ctx); ok {
		add(f)
	}

	return findings, nil
}

// fakeNodeLabel is stamped by gpu-node-mocker on every simulated Node. Such
// nodes have no kubelet, so the local CSI node DaemonSet can never run there.
const fakeNodeLabel = "kaito.sh/fake-node"

// localCSINodeFinding probes the local CSI driver node DaemonSet: any of its
// pods that is not Ready flags clusterLocalCSIDriverNotReady, except pods on
// gpu-node-mocker nodes. A missing DaemonSet is not reported (see Evaluate).
func (e *Evaluator) localCSINodeFinding(ctx context.Context) (evaluator.Finding, bool) {
	ref := e.cfg.LocalCSIDriver
	if ref.Namespace == "" || ref.NodeDaemonSet == "" {
		return evaluator.Finding{}, false
	}
	fakeNodes, err := e.fakeNodes(ctx)
	if err != nil {
		return evaluator.Finding{}, false // unknown this pass
	}
	u, notReady := kube.DaemonSetPodUnavailable(ctx, e.clientset, ref.Namespace, ref.NodeDaemonSet,
		func(name string) bool { return fakeNodes[name] })
	if !notReady {
		return evaluator.Finding{}, false
	}
	return evaluator.Finding{
		Reason:            reason.ClusterLocalCSIDriverNotReady,
		Object:            evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: ref.Namespace},
		Message:           fmt.Sprintf("local CSI driver node pod of DaemonSet %s/%s on Node %s is not ready: %s.", ref.Namespace, ref.NodeDaemonSet, u.NodeName, u.Cause),
		GroupKey:          "cluster/" + ref.Namespace,
		ResourceCreatedAt: u.Since,
		AwaitingNode:      u.AwaitingNode,
	}, true
}

// fakeNodes returns the names of the Nodes created by gpu-node-mocker.
func (e *Evaluator) fakeNodes(ctx context.Context) (map[string]bool, error) {
	list, err := e.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: fakeNodeLabel})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(list.Items))
	for i := range list.Items {
		out[list.Items[i].Name] = true
	}
	return out, nil
}
