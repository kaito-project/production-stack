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

package emit

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
)

// suffixInfo records, for one emitted upstream cluster finding, the downstream
// reasons it is actually suppressing and in how many namespaces (§1.4).
type suffixInfo struct {
	reasons map[reason.Reason]bool
	nsCount int
}

// Suppression is the computed §1.4 result for one reconcile pass.
type Suppression struct {
	// gated is the global set of downstream reasons currently suppressed and
	// therefore filtered from emission. Populated by cluster-wide upstream
	// reasons, which apply to every namespace.
	gated map[reason.Reason]bool
	// gatedNS is the namespace-scoped set of downstream reasons suppressed by an
	// active modelharness reason, keyed by (reason, namespace): a broken
	// namespace Gateway only gates route errors within its OWN namespace.
	gatedNS map[nsReason]bool
	// suffix maps an emitted cluster finding's GroupKey to its transparency
	// suffix payload.
	suffix map[string]suffixInfo
}

// nsReason keys a namespace-scoped suppression entry: a downstream reason gated
// only within a specific workload namespace.
type nsReason struct {
	reason    reason.Reason
	namespace string
}

func (s Suppression) isGated(f evaluator.Finding) bool {
	if s.gated[f.Reason] {
		return true
	}
	return s.gatedNS[nsReason{reason: f.Reason, namespace: f.WorkloadNamespace}]
}

// ComputeSuppression builds the §1.4 suppression sets: which downstream
// reasons are gated by which active upstream cluster reasons, and which of
// them actually fired (for the transparency suffix). It works purely from the
// emitted Findings: the active cluster reasons are the cluster findings' own
// reasons, and the missing-CRD names are the involvedObject names of the
// clusterCRDMissing findings.
func ComputeSuppression(clusterFindings, harnessFindings, mdFindings []evaluator.Finding) Suppression {
	supp := Suppression{
		gated:   map[reason.Reason]bool{},
		gatedNS: map[nsReason]bool{},
		suffix:  map[string]suffixInfo{},
	}

	// fired[reason] -> set of namespaces where that downstream reason actually
	// fired this pass.
	fired := map[reason.Reason]map[string]bool{}
	record := func(f evaluator.Finding) {
		if f.WorkloadNamespace == "" {
			return
		}
		if fired[f.Reason] == nil {
			fired[f.Reason] = map[string]bool{}
		}
		fired[f.Reason][f.WorkloadNamespace] = true
	}
	for _, f := range harnessFindings {
		record(f)
	}
	for _, f := range mdFindings {
		record(f)
	}

	// Helper to accumulate a gated set and build the per-finding suffix info.
	build := func(groupKey string, candidates []reason.Reason) {
		if len(candidates) == 0 {
			return
		}
		actually := map[reason.Reason]bool{}
		nsSet := map[string]bool{}
		for _, r := range candidates {
			supp.gated[r] = true
			for ns := range fired[r] {
				actually[r] = true
				nsSet[ns] = true
			}
		}
		if len(actually) > 0 {
			supp.suffix[groupKey] = suffixInfo{reasons: actually, nsCount: len(nsSet)}
		}
	}

	for _, f := range clusterFindings {
		switch f.Reason {
		case reason.ClusterCRDMissing:
			build(f.GroupKey, reason.SuppressedByCRD(f.Object.Name))
		default:
			if reason.HasSuppressionRow(f.Reason) {
				build(f.GroupKey, reason.SuppressedBy(f.Reason))
			}
		}
	}

	// Namespace-scoped harness→modeldeployment suppression: an active
	// modelharness reason gates the mapped downstream reasons ONLY within its own
	// workload namespace (a broken namespace Gateway makes every attached
	// HTTPRoute report Accepted=False, so the redundant per-InferenceSet route
	// errors in that namespace are gated while the Gateway problem surfaces once).
	for _, f := range harnessFindings {
		if f.WorkloadNamespace == "" {
			continue
		}
		for _, r := range reason.SuppressedWithinNamespaceBy(f.Reason) {
			supp.gatedNS[nsReason{reason: r, namespace: f.WorkloadNamespace}] = true
		}
	}
	return supp
}

// startupGate holds one layer's startup-grace debounce state: the clock it reads
// the pass time from, the grace window it gates warnings by, and the persistent
// first-observed map it updates in place and prunes each pass. Every
// LayerEmitter embeds one so each layer owns its own debounce bookkeeping — the
// keys are (GroupKey, Reason), which never collide across layers.
type startupGate struct {
	clock func() time.Time
	grace time.Duration
	// notReadySince records, per (GroupKey, Reason), when a startup-grace-gated
	// finding without a backing object was first observed, so it is only
	// surfaced once it has persisted for the grace window (debounce).
	notReadySince map[string]time.Time
}

func newStartupGate(clock func() time.Time, grace time.Duration) startupGate {
	return startupGate{clock: clock, grace: grace, notReadySince: map[string]time.Time{}}
}

// withholdDuringStartup reports whether a startup-grace-gated warning finding
// should be withheld this pass because the underlying problem may still be a
// transient startup state. Exempt findings (confirmed terminal failures) are
// never withheld. now is the pass clock, and touched collects the observed
// debounce keys so prune can drop the timers that were not.
//
// Scheme 2 (object-age gating): when the finding's backing resource creation
// time is known, the warning is withheld only while the resource is still
// inside its startup window; an old-but-broken resource surfaces immediately,
// and this survives reporter restarts (the timestamp is persistent).
//
// Scheme 1 (debounce): for findings without a backing object (missing CRDs or
// Deployments) the warning is withheld until the problem has persisted for the
// grace window, so a chart that has not finished installing at reporter start
// does not flap the event stream.
func (g *startupGate) withholdDuringStartup(f evaluator.Finding, now time.Time, touched map[string]bool) bool {
	if f.StartupGraceExempt {
		return false
	}
	grace := g.grace
	if f.GracePeriodOverride > 0 {
		grace = f.GracePeriodOverride
	}
	if grace <= 0 {
		return false
	}
	if !f.ResourceCreatedAt.IsZero() {
		return now.Sub(f.ResourceCreatedAt) < grace
	}
	key := f.GroupKey + "|" + string(f.Reason)
	touched[key] = true
	since, ok := g.notReadySince[key]
	if !ok {
		g.notReadySince[key] = now
		return true
	}
	return now.Sub(since) < grace
}

// prune drops debounce timers for findings that were not observed this pass, so
// a problem that recovers and later recurs is debounced afresh.
func (g *startupGate) prune(touched map[string]bool) {
	for k := range g.notReadySince {
		if !touched[k] {
			delete(g.notReadySince, k)
		}
	}
}

// LayerEmitter turns one reporting layer's Findings into events. Each layer owns
// its startup-grace debounce state and clock across passes; the pass logger is
// carried on the context. Each layer (cluster, modelharness, modeldeployment,
// weight-download) plugs in the strategy that matches its aggregation rules.
type LayerEmitter interface {
	Emit(ctx context.Context, findings []evaluator.Finding, supp Suppression)
}

// AllFindings returns a strategy that emits every non-withheld finding on its
// own cluster-scoped involvedObject, appending the §1.4 transparency suffix
// where the finding is actively suppressing downstream reasons. Used by the
// cluster layer.
func AllFindings(cs kubernetes.Interface, clock func() time.Time, grace time.Duration, label string) LayerEmitter {
	return &allFindings{w: newWriter(cs), gate: newStartupGate(clock, grace), label: label}
}

// PerGroup returns a strategy that groups findings by keyOf, drops gated (§1.4)
// and startup-withheld findings, then emits every surviving reason (deduplicated
// to one event per reason) for each group. Used by the modelharness (per
// namespace), modeldeployment (per InferenceSet) and weight-download (per
// InferenceSet) layers.
func PerGroup(cs kubernetes.Interface, clock func() time.Time, grace time.Duration, keyOf func(evaluator.Finding) string, label string) LayerEmitter {
	return &perGroup{w: newWriter(cs), gate: newStartupGate(clock, grace), keyOf: keyOf, label: label}
}

type allFindings struct {
	w     *writer
	gate  startupGate
	label string
}

func (s *allFindings) Emit(ctx context.Context, findings []evaluator.Finding, supp Suppression) {
	logger := logr.FromContextOrDiscard(ctx)
	now := s.gate.clock()
	touched := map[string]bool{}
	var toEmit []evaluator.Finding
	for _, f := range findings {
		if s.gate.withholdDuringStartup(f, now, touched) {
			continue
		}
		toEmit = append(toEmit, f)
	}
	s.gate.prune(touched)
	for _, f := range toEmit {
		message := f.Message
		if info, ok := supp.suffix[f.GroupKey]; ok {
			message += reason.TransparencySuffix(info.reasons, info.nsCount)
		}
		if err := s.w.write(ctx, f, message); err != nil {
			logger.Error(err, "emit "+s.label+" finding", "reason", string(f.Reason))
		}
	}
}

// perGroup emits every surviving reason for each group. There is no priority
// collapse: each reason is an independent signal that must surface so
// concurrent faults are never hidden behind one another, and the absence of a
// Warning is the only "healthy" signal. Findings are also debounced through the
// startup grace gate unless they are StartupGraceExempt.
type perGroup struct {
	w     *writer
	gate  startupGate
	keyOf func(evaluator.Finding) string
	label string
}

func (s *perGroup) Emit(ctx context.Context, findings []evaluator.Finding, supp Suppression) {
	logger := logr.FromContextOrDiscard(ctx)
	now := s.gate.clock()
	touched := map[string]bool{}
	byKey := map[string][]evaluator.Finding{}
	var order []string
	for _, f := range findings {
		if supp.isGated(f) {
			continue
		}
		if s.gate.withholdDuringStartup(f, now, touched) {
			continue
		}
		k := s.keyOf(f)
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], f)
	}
	s.gate.prune(touched)
	// For each group, deduplicate to one finding per reason (keeping the last
	// seen while preserving first-seen order) and emit one event per reason.
	for _, k := range order {
		findingByReason := map[reason.Reason]evaluator.Finding{}
		var reasonOrder []reason.Reason
		for _, f := range byKey[k] {
			if _, seen := findingByReason[f.Reason]; !seen {
				reasonOrder = append(reasonOrder, f.Reason)
			}
			findingByReason[f.Reason] = f
		}

		for _, rsn := range reasonOrder {
			f := findingByReason[rsn]
			if err := s.w.write(ctx, f, ""); err != nil {
				logger.Error(err, "emit "+s.label+" finding", "reason", string(rsn), "group", k)
			}
		}
	}
}
