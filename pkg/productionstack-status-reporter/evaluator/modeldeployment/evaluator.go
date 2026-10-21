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

// Package modeldeployment implements the modeldeployment-layer Evaluator: it
// enumerates the InferenceSets in every managed namespace and evaluates the
// §1.2 modeldeployment reasons for each — the pass-through KAITO
// InferenceSetReady failure and inferencesetEPPNotReady.
package modeldeployment

import (
	"context"
	"fmt"
	"time"

	kaitoconsts "github.com/kaito-project/kaito/pkg/utils/consts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator/util"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
	"github.com/kaito-project/production-stack/pkg/util/kube"
)

// inferenceSetReadyCondition is the InferenceSet condition on which KAITO
// aggregates the root cause of its not-ready child Workspaces.
const inferenceSetReadyCondition = "InferenceSetReady"

// failureGracePeriod debounces a pass-through InferenceSetReady failure that
// KAITO does not classify as terminal: the controllers keep retrying it, so it
// is surfaced only once it has persisted this long since the condition's
// lastTransitionTime. Terminal failures (consts.IsTerminatedReason) bypass it.
const failureGracePeriod = 5 * time.Minute

// Evaluator enumerates the InferenceSets in every managed namespace and
// evaluates every modeldeployment-layer reason (§1.2) for each. The orthogonal
// inferencesetModelDownloadSlow reason is evaluated separately by the
// weightdownload Evaluator.
type Evaluator struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
}

// New constructs a modeldeployment Evaluator.
func New(cs kubernetes.Interface, dyn dynamic.Interface) *Evaluator {
	return &Evaluator{clientset: cs, dynamic: dyn}
}

// Name identifies the evaluator for logging.
func (e *Evaluator) Name() string { return "modeldeployment" }

// Evaluate enumerates the InferenceSets across all managed namespaces and
// evaluates every modeldeployment-layer reason for each. It returns an error
// only when namespace discovery fails.
func (e *Evaluator) Evaluate(ctx context.Context) ([]evaluator.Finding, error) {
	namespaces, err := util.DiscoverNamespaces(ctx, e.clientset)
	if err != nil {
		return nil, err
	}
	var findings []evaluator.Finding
	for _, ns := range namespaces {
		sets, err := e.dynamic.Resource(util.InferenceSetGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for i := range sets.Items {
			findings = append(findings, e.evaluateInferenceSet(ctx, ns, &sets.Items[i])...)
		}
	}
	return findings, nil
}

func (e *Evaluator) evaluateInferenceSet(ctx context.Context, namespace string, is *unstructured.Unstructured) []evaluator.Finding {
	name := is.GetName()
	obj := evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: namespace}
	groupKey := fmt.Sprintf("inferenceset/%s/%s", namespace, name)
	var findings []evaluator.Finding

	if f, ok := inferenceSetReadyFinding(is, obj, groupKey); ok {
		findings = append(findings, f)
	}

	// inferencesetEPPNotReady — EPP Deployment readiness.
	if eppName, eppCreatedAt, msg := eppNotReady(ctx, e.clientset, namespace, name); msg != "" {
		findings = append(findings, evaluator.Finding{
			Reason:            reason.InferencesetEPPNotReady,
			Object:            obj,
			Message:           fmt.Sprintf("InferenceSet %s/%s: EPP Deployment %s %s.", namespace, name, eppName, msg),
			WorkloadNamespace: namespace,
			GroupKey:          groupKey,
			ResourceCreatedAt: eppCreatedAt,
		})
	}

	return findings
}

// readyCondition is the InferenceSetReady condition flattened for the
// reporter, with lastTransitionTime parsed for debounce anchoring.
type readyCondition struct {
	status             string
	reason             string
	message            string
	observedGeneration int64
	lastTransition     time.Time
}

// getInferenceSetReadyCondition extracts status.conditions[InferenceSetReady].
func getInferenceSetReadyCondition(is *unstructured.Unstructured) (readyCondition, bool) {
	conds, ok, _ := unstructured.NestedSlice(is.Object, "status", "conditions")
	if !ok {
		return readyCondition{}, false
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != inferenceSetReadyCondition {
			continue
		}
		out := readyCondition{}
		out.status, _ = m["status"].(string)
		out.reason, _ = m["reason"].(string)
		out.message, _ = m["message"].(string)
		out.observedGeneration, _, _ = unstructured.NestedInt64(m, "observedGeneration")
		if ts, _ := m["lastTransitionTime"].(string); ts != "" {
			if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
				out.lastTransition = parsed
			}
		}
		return out, true
	}
	return readyCondition{}, false
}

// inferenceSetReadyFinding passes through the failure KAITO reports on
// InferenceSetReady. KAITO already aggregates the root cause of the child
// Workspaces onto it — node provisioning, inference workload and ModelMirror
// failures — so the reason and message are surfaced verbatim. In-progress
// reasons (consts.IsProgressReason) are not failures and are not surfaced. A
// terminal reason (consts.IsTerminatedReason) is surfaced immediately; any
// other failure only once it has persisted for failureGracePeriod since the
// condition's lastTransitionTime. A condition written for an older generation
// of the InferenceSet is stale and ignored until KAITO reconciles it.
func inferenceSetReadyFinding(is *unstructured.Unstructured, obj evaluator.InvolvedObject, groupKey string) (evaluator.Finding, bool) {
	cond, ok := getInferenceSetReadyCondition(is)
	if !ok || cond.status != string(metav1.ConditionFalse) || cond.reason == "" {
		return evaluator.Finding{}, false
	}
	if kaitoconsts.IsProgressReason(cond.reason) {
		return evaluator.Finding{}, false
	}
	if cond.observedGeneration != 0 && cond.observedGeneration != is.GetGeneration() {
		return evaluator.Finding{}, false
	}
	return evaluator.Finding{
		Reason:              reason.Reason(cond.reason),
		Object:              obj,
		Message:             fmt.Sprintf("InferenceSet %s/%s: %s", is.GetNamespace(), is.GetName(), cond.message),
		WorkloadNamespace:   is.GetNamespace(),
		GroupKey:            groupKey,
		StartupGraceExempt:  kaitoconsts.IsTerminatedReason(cond.reason),
		ResourceCreatedAt:   cond.lastTransition,
		GracePeriodOverride: failureGracePeriod,
	}, true
}

// eppNotReady checks the EPP Deployment for an InferenceSet by delegating to
// the shared kube.DeploymentPodUnavailable probe. The EPP Deployment name is
// deterministic: charts/modeldeployment names it `<inferenceset>-inferencepool-epp`
// (the modeldeployment.eppServiceName helper) and the InferenceSet name equals
// the chart's deployment name, so it is derived directly rather than via a label
// list (which could match more than one Deployment). It returns the Deployment
// name, a debounce anchor for the startup grace gate (the pod's NotReady
// transition; zero when the Deployment is missing, so the emit falls back to the
// reason-level debounce), and a message predicate when the Deployment is missing
// or its pod is not ready. A missing Deployment is surfaced too: without the EPP
// the InferencePool has no endpoint picker and routing is broken.
func eppNotReady(ctx context.Context, cs kubernetes.Interface, namespace, name string) (string, time.Time, string) {
	eppName := name + "-inferencepool-epp"
	missing, unavailable, cause, notReadySince := kube.DeploymentPodUnavailable(ctx, cs, namespace, eppName)
	switch {
	case missing:
		return eppName, time.Time{}, "is missing; re-apply charts/modeldeployment to restore the endpoint picker"
	case unavailable:
		return eppName, notReadySince, "not ready: " + cause
	}
	return eppName, time.Time{}, ""
}
