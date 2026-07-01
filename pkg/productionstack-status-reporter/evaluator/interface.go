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

// Package evaluator holds the read-only probes that evaluate the §1.2 reason
// catalogue across the cluster, modelharness and modeldeployment layers. Each
// probe is an Evaluator that owns its own resource discovery and returns the
// active Findings; it performs no event emission or suppression — those remain
// the status reporter's responsibility, which only calls Evaluate on each
// Evaluator and acts on the returned Findings.
package evaluator

import (
	"context"
	"time"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
)

const (
	// LabelInferenceSet is stamped by charts/modeldeployment on every
	// chart-owned object, used to group objects by their owning InferenceSet.
	LabelInferenceSet = "kaito.sh/inferenceset"
	// LabelCreatedBy is stamped by the KAITO InferenceSet controller on every
	// child object it renders (Workspaces and their pods), used to correlate
	// them back to the owning InferenceSet (whose name they do not share).
	LabelCreatedBy = "inferenceset.kaito.sh/created-by"
)

// Evaluator probes one layer of the §1.2 reason catalogue. Evaluate is called
// once per reconcile pass; it discovers the resources it needs and returns one
// Finding per active warning across every managed namespace. It never emits
// events or applies suppression.
type Evaluator interface {
	// Name identifies the evaluator for logging.
	Name() string
	// Evaluate returns the active Findings for this layer. An error is
	// returned only when the evaluator could not enumerate its subjects (e.g.
	// namespace discovery failed), so the caller can avoid treating an empty
	// result as a recovery.
	Evaluate(ctx context.Context) ([]Finding, error)
}

// ObjectKind is the kind of a cluster-scoped involvedObject. Per §1.1 events
// in kube-system may only reference cluster-scoped resources.
type ObjectKind string

const (
	// KindNamespace references a Namespace (the containing namespace of the
	// problematic resource).
	KindNamespace ObjectKind = "Namespace"
	// KindCRD references a CustomResourceDefinition (only for
	// clusterCRDMissing).
	KindCRD ObjectKind = "CustomResourceDefinition"
)

// InvolvedObject is the cluster-scoped object an Event points at.
type InvolvedObject struct {
	Kind ObjectKind
	Name string
}

// Finding is one evaluated reason ready to be surfaced as an Event.
type Finding struct {
	Reason  reason.Reason
	Object  InvolvedObject
	Message string
	// WorkloadNamespace is the workload namespace the finding belongs to, used
	// for §1.4 suppression accounting and per-InferenceSet grouping. Empty for
	// cluster-layer findings that are not namespace-scoped.
	WorkloadNamespace string
	// GroupKey uniquely identifies the logical subject (e.g. the InferenceSet)
	// so its findings are grouped and emitted together, one event per reason.
	GroupKey string
	// StartupGraceExempt marks a finding that must be surfaced immediately,
	// bypassing the startup grace gate. Set for confirmed (terminal) failures
	// such as the Workspace / model-pod reasons that already discriminate
	// failed from in-progress states.
	StartupGraceExempt bool
	// ResourceCreatedAt is the creation time of the resource the finding is
	// about, used by the startup grace gate for object-age gating. Zero when
	// there is no backing object (e.g. a missing CRD or Deployment).
	ResourceCreatedAt time.Time
	// GracePeriodOverride, when greater than zero, replaces the global
	// StartupGracePeriod for this finding's startup-grace gate. Evaluators set
	// it for reasons whose normal transient window differs from the default
	// (e.g. Gateway programming, which waits on data-plane / load-balancer
	// provisioning that legitimately takes several minutes). Zero means use the
	// global StartupGracePeriod.
	GracePeriodOverride time.Duration
}
