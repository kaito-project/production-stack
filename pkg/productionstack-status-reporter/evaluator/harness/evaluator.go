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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator/util"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
	"github.com/kaito-project/production-stack/pkg/util/kube"
)

// gatewayGVR is the Gateway API type the harness evaluator reads read-only. It
// is not vendored, so it is consumed via the dynamic client as unstructured.
var gatewayGVR = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}

// gatewayProgrammingGracePeriod is the startup-grace window applied to
// ModelharnessGatewayProgrammingFailed. Gateway programming waits on the Istio
// auto-provisioned Envoy data plane (and, for LoadBalancer Services, cloud
// load-balancer address assignment), which legitimately takes several minutes
// on a fresh install or when a node must be scaled up — much longer than the
// global StartupGracePeriod tuned for long-lived control-plane pods.
const gatewayProgrammingGracePeriod = 5 * time.Minute

// Evaluator probes every modelharness-layer reason (§1.2) across all managed
// namespaces.
type Evaluator struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
}

// New constructs a harness Evaluator.
func New(cs kubernetes.Interface, dyn dynamic.Interface) *Evaluator {
	return &Evaluator{clientset: cs, dynamic: dyn}
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
