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
// enumerates the InferenceSets in every managed namespace and evaluates every
// §1.2 modeldeployment chain reason for each.
package modeldeployment

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator/util"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
	"github.com/kaito-project/production-stack/pkg/util/kube"
)

// GroupVersionResources for the CRDs the modeldeployment evaluator reads
// read-only. KAITO, karpenter, Gateway API and GAIE types are not vendored, so
// they are consumed via the dynamic client as unstructured objects.
var (
	workspaceGVR     = schema.GroupVersionResource{Group: "kaito.sh", Version: "v1beta1", Resource: "workspaces"}
	nodeClaimGVR     = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodeclaims"}
	httpRouteGVR     = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
	inferencePoolGVR = schema.GroupVersionResource{Group: "inference.networking.k8s.io", Version: "v1", Resource: "inferencepools"}
)

// KAITO stamps these labels on every karpenter NodeClaim it creates for a
// Workspace, letting us map a Workspace to its (cluster-scoped) NodeClaims.
const (
	labelWorkspaceName      = "kaito.sh/workspace"
	labelWorkspaceNamespace = "kaito.sh/workspacenamespace"
)

// infraProvisioningGracePeriod debounces a *retryable* NodeClaim Launched=False
// failure: karpenter re-attempts VM creation with alternative SKUs/zones (the
// Azure provider caches a bad SKU/zone for ~1h), so a transient capacity/quota
// blip clears within minutes and must not alarm, while a genuine shortage (e.g.
// zero quota) persists past this window and is surfaced. Non-retryable failures
// bypass this and are surfaced immediately.
const infraProvisioningGracePeriod = 10 * time.Minute

// Evaluator enumerates the InferenceSets in every managed namespace and
// evaluates every modeldeployment-layer chain reason (§1.2) for each. The
// orthogonal inferencesetWeightDownloadSlow reason is evaluated separately by
// the weightdownload Evaluator.
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
// evaluates every modeldeployment-layer chain reason for each. It returns an
// error only when namespace discovery fails.
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
	cs := e.clientset
	dyn := e.dynamic
	name := is.GetName()
	obj := evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: namespace}
	groupKey := fmt.Sprintf("inferenceset/%s/%s", namespace, name)
	var findings []evaluator.Finding

	add := func(r reason.Reason, exempt bool, createdAt time.Time, msg string) {
		findings = append(findings, evaluator.Finding{
			Reason:             r,
			Object:             obj,
			Message:            msg,
			WorkloadNamespace:  namespace,
			GroupKey:           groupKey,
			StartupGraceExempt: exempt,
			ResourceCreatedAt:  createdAt,
		})
	}

	// inferencesetInfraProvisioningFailed — GPU node provisioning failure,
	// detected on the child Workspace's karpenter NodeClaim(s) rather than the
	// Workspace condition. KAITO does NOT surface a terminal reason on the
	// Workspace: on a provisioning failure it stays NodeClaimReady=False /
	// NodeClaimNotReady, indistinguishable from normal in-progress provisioning.
	// The authoritative signal is the underlying NodeClaim's Launched=False
	// condition, whose reason/message carries the Azure cause (subscription/family
	// quota, zonal/regional capacity, SKU not available). A non-retryable reason
	// (SKU unavailable in region, or all candidate instance types exhausted) is
	// surfaced immediately; a retryable capacity/quota reason is debounced via
	// infraProvisioningGracePeriod because karpenter automatically retries with an
	// alternative SKU/zone and short outages recover on their own. When this fires
	// the node never becomes Ready and the model pod is never created, so the
	// pod-readiness check below stays silent and this is the sole event.
	if f, ok := infraProvisioningFinding(ctx, dyn, namespace, name, obj, groupKey); ok {
		findings = append(findings, f)
	}

	// inferencesetModelPodsNotReady — a vLLM model-server pod has a terminal
	// container failure (ImagePullBackOff / CrashLoopBackOff / OOMKilled / ...)
	// on its init (model-weights-downloader) or main container. KAITO's vLLM
	// StartupProbe legitimately budgets up to ~30 min for a healthy model to
	// load, so a merely still-starting pod (Ready=False without a terminal
	// reason) is NOT surfaced here; if such a pod never becomes healthy the
	// startup probe eventually restarts it into CrashLoopBackOff, which this
	// branch then catches. A slow-but-progressing weight download is covered by
	// inferencesetWeightDownloadSlow. Surfaced immediately since a terminal
	// container state is a real failure regardless of the startup budget.
	if llmPod, terminal, cause := modelPodUnavailable(ctx, cs, namespace, name); terminal {
		add(reason.InferencesetModelPodsNotReady, true, time.Time{}, fmt.Sprintf(
			"InferenceSet %s/%s: model pod %s is not ready: %s.", namespace, name, llmPod, cause))
	}

	// inferencesetEPPNotReady — EPP Deployment readiness.
	if eppName, eppCreatedAt, msg := eppNotReady(ctx, cs, namespace, name); msg != "" {
		add(reason.InferencesetEPPNotReady, false, eppCreatedAt, fmt.Sprintf(
			"InferenceSet %s/%s: EPP Deployment %s %s.", namespace, name, eppName, msg))
	}

	// inferencesetRouteNotReady — HTTPRoute parent / InferencePool status.
	if rtCreatedAt, msg := routeNotReady(ctx, dyn, namespace, name); msg != "" {
		add(reason.InferencesetRouteNotReady, false, rtCreatedAt, fmt.Sprintf(
			"InferenceSet %s/%s: %s.", namespace, name, msg))
	}

	return findings
}

// isTerminalLaunchFailure reports whether a NodeClaim Launched=False failure is
// non-retryable and therefore should be surfaced without waiting out the grace
// window. The Azure provider caches an unavailable SKU for ~23h (SKUNotAvailable),
// and KAITO treats "all requested instance types were unavailable during launch"
// (every candidate exhausted) as a terminal error that stops the reconcile.
// Retryable quota/capacity reasons (SubscriptionQuotaReached, AllocationFailure,
// ZonalAllocationFailure, CreateInstanceFailed) are debounced instead, because
// karpenter automatically retries with an alternative SKU/zone.
func isTerminalLaunchFailure(rsn, message string) bool {
	return rsn == "SKUNotAvailable" || strings.Contains(message, "unavailable during launch")
}

// modelPodUnavailable inspects the vLLM model-server pods backing an
// InferenceSet and returns the first one that is not serving (its Pod Ready
// condition is False); podName is empty when every pod is serving (or none was
// found). KAITO renders the model server as a StatefulSet (<workspace>-N), so
// an InferenceSet may have several pods (replicas, or disaggregated
// prefill/decode); every pod carries the
// inferenceset.kaito.sh/created-by=<name> label used as the selector here.
//
// terminal reports whether that pod has a non-transient container failure
// (ImagePullBackOff / CrashLoopBackOff / OOMKilled / ...) on its init
// (model-weights-downloader) or main (vLLM) container, as opposed to being
// merely still-starting within KAITO's ~30-minute StartupProbe budget. A pod
// with a terminal failure is preferred over a still-starting one. cause is a
// human-readable hint for the event message, set only when terminal.
//
// Unlike kube.DeploymentPodUnavailable this deliberately does NOT report a
// missing pod or a Pending/unschedulable one: KAITO only creates the model pod
// after its GPU node is already Ready, so a pod without a Ready condition is a
// brief bind race rather than a schedulability failure, and a wholly absent pod
// is already covered by inferencesetInfraProvisioningFailed.
func modelPodUnavailable(ctx context.Context, cs kubernetes.Interface, namespace, name string) (podName string, terminal bool, cause string) {
	// Model-server pods are rendered by the KAITO Workspace controller (not the
	// chart). KAITO stamps inferenceset.kaito.sh/created-by=<name> onto the pod
	// template, so select by that label rather than the chart's identifying
	// label or a nonexistent component label.
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", evaluator.LabelCreatedBy, name),
	})
	if err != nil {
		return "", false, ""
	}
	var benignName string
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue // being replaced during a rollout — not an anomaly
		}
		ready := kube.PodReadyCondition(p)
		if ready == nil {
			// The node was Ready before the pod was created, so an absent Ready
			// condition is a transient bind race, not a schedulability failure.
			continue
		}
		if ready.Status != corev1.ConditionFalse {
			continue
		}
		if tcause, ok := kube.PodTerminalContainerCause(p); ok {
			return p.Name, true, tcause // terminal failure wins immediately
		}
		if benignName == "" {
			benignName = p.Name // first still-starting pod; surfaced only via weightdownload
		}
	}
	return benignName, false, ""
}

// getWorkspaces returns the KAITO Workspaces created by the InferenceSet,
// selected via the inferenceset.kaito.sh/created-by label (child Workspace
// names are assigned by the KAITO controller and do not match the
// InferenceSet name). Returns nil when unavailable.
func getWorkspaces(ctx context.Context, dyn dynamic.Interface, namespace, name string) []*unstructured.Unstructured {
	list, err := dyn.Resource(workspaceGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", evaluator.LabelCreatedBy, name),
	})
	if err != nil {
		return nil
	}
	out := make([]*unstructured.Unstructured, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	return out
}

// listWorkspaceNodeClaims returns the cluster-scoped karpenter NodeClaims KAITO
// created for a Workspace, matched by the kaito.sh/workspace and
// kaito.sh/workspacenamespace labels KAITO stamps on every NodeClaim. Returns
// nil when unavailable (e.g. the karpenter CRD is absent).
func listWorkspaceNodeClaims(ctx context.Context, dyn dynamic.Interface, wsNamespace, wsName string) []*unstructured.Unstructured {
	list, err := dyn.Resource(nodeClaimGVR).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s", labelWorkspaceName, wsName, labelWorkspaceNamespace, wsNamespace),
	})
	if err != nil {
		return nil
	}
	out := make([]*unstructured.Unstructured, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	return out
}

// infraProvisioningFinding inspects the karpenter NodeClaims backing an
// InferenceSet's child Workspaces and returns an
// inferencesetInfraProvisioningFailed finding when a NodeClaim's Launched
// condition is False (Azure refused to create the VM). A non-retryable failure
// is returned immediately and exempt from the startup grace gate; a retryable
// failure is anchored on the NodeClaim's age and debounced via
// infraProvisioningGracePeriod. A terminal failure takes precedence.
func infraProvisioningFinding(ctx context.Context, dyn dynamic.Interface, namespace, name string, obj evaluator.InvolvedObject, groupKey string) (evaluator.Finding, bool) {
	var retryable *evaluator.Finding
	for _, ws := range getWorkspaces(ctx, dyn, namespace, name) {
		for _, nc := range listWorkspaceNodeClaims(ctx, dyn, ws.GetNamespace(), ws.GetName()) {
			st, rsn, msg, ok := kube.ConditionStatus(nc, "Launched")
			if !ok || st != "False" {
				continue
			}
			if isTerminalLaunchFailure(rsn, msg) {
				return evaluator.Finding{
					Reason:             reason.InferencesetInfraProvisioningFailed,
					Object:             obj,
					Message:            fmt.Sprintf("InferenceSet %s/%s: GPU node provisioning failed — NodeClaim %s Launched=False (%s): %s; the requested SKU is unavailable in the region and will not be retried.", namespace, name, nc.GetName(), rsn, msg),
					WorkloadNamespace:  namespace,
					GroupKey:           groupKey,
					StartupGraceExempt: true,
				}, true
			}
			if retryable == nil {
				retryable = &evaluator.Finding{
					Reason:              reason.InferencesetInfraProvisioningFailed,
					Object:              obj,
					Message:             fmt.Sprintf("InferenceSet %s/%s: GPU node provisioning failing — NodeClaim %s Launched=False (%s): %s; karpenter is retrying with an alternative SKU/zone.", namespace, name, nc.GetName(), rsn, msg),
					WorkloadNamespace:   namespace,
					GroupKey:            groupKey,
					ResourceCreatedAt:   nc.GetCreationTimestamp().Time,
					GracePeriodOverride: infraProvisioningGracePeriod,
				}
			}
		}
	}
	if retryable != nil {
		return *retryable, true
	}
	return evaluator.Finding{}, false
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

// routeNotReady checks the HTTPRoute parent acceptance and InferencePool
// readiness for an InferenceSet. It returns the offending object's
// creationTimestamp (for the startup grace gate) and a cause when not ready.
func routeNotReady(ctx context.Context, dyn dynamic.Interface, namespace, name string) (time.Time, string) {
	routes, err := dyn.Resource(httpRouteGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", evaluator.LabelInferenceSet, name),
	})
	if err == nil {
		for i := range routes.Items {
			r := &routes.Items[i]
			parents, ok, _ := unstructured.NestedSlice(r.Object, "status", "parents")
			if !ok {
				continue
			}
			for _, pr := range parents {
				pm, ok := pr.(map[string]interface{})
				if !ok {
					continue
				}
				conds, _, _ := unstructured.NestedSlice(pm, "conditions")
				for _, c := range conds {
					cm, ok := c.(map[string]interface{})
					if !ok {
						continue
					}
					t, _ := cm["type"].(string)
					st, _ := cm["status"].(string)
					if (t == "Accepted" || t == "ResolvedRefs") && st == "False" {
						msg, _ := cm["message"].(string)
						return r.GetCreationTimestamp().Time, fmt.Sprintf("HTTPRoute %s %s=False: %s", r.GetName(), t, msg)
					}
				}
			}
		}
	}

	// InferencePool selector matches zero pods.
	pools, err := dyn.Resource(inferencePoolGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", evaluator.LabelInferenceSet, name),
	})
	if err == nil {
		for i := range pools.Items {
			p := &pools.Items[i]
			if st, _, msg, ok := kube.ConditionStatus(p, "ResolvedRefs"); ok && st == "False" {
				return p.GetCreationTimestamp().Time, fmt.Sprintf("InferencePool %s ResolvedRefs=False: %s", p.GetName(), msg)
			}
		}
	}
	return time.Time{}, ""
}
