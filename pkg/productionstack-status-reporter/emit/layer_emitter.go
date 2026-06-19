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
)

// startupGate holds one layer's startup-grace debounce state: the clock it reads
// the pass time from, the grace window it gates warnings by, and the persistent
// first-observed map it updates in place and prunes each pass. Every Emitter
// embeds one so each layer owns its own debounce bookkeeping — the keys are
// (GroupKey, Reason), which never collide across layers.
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

// Emitter turns one reporting layer's Findings into events. It owns the layer's
// startup-grace debounce state and clock across passes; the pass logger is
// carried on the context. It is shared by every layer (cluster, modelharness,
// modeldeployment, weight-download): each layer's evaluator already yields at
// most one finding per (subject, reason), so no per-layer aggregation strategy
// is needed.
type Emitter struct {
	w     *writer
	gate  startupGate
	label string
}

// NewEmitter constructs a layer Emitter.
func NewEmitter(cs kubernetes.Interface, clock func() time.Time, grace time.Duration, label string) *Emitter {
	return &Emitter{w: newWriter(cs), gate: newStartupGate(clock, grace), label: label}
}

// Emit gates transient startup findings, then writes one Event per surviving
// finding. Each reason is an independent signal that must surface, so there is
// no priority collapse — the absence of a Warning is the only "healthy" signal.
// Findings that resolve to the same Event identity (eventName) within one pass
// are collapsed to a single write (keeping the last, in first-seen order), so an
// accidental duplicate never inflates the Event Count or flips its message.
func (s *Emitter) Emit(ctx context.Context, findings []evaluator.Finding) {
	logger := logr.FromContextOrDiscard(ctx)
	now := s.gate.clock()
	touched := map[string]bool{}
	byEvent := map[string]evaluator.Finding{}
	var order []string
	for _, f := range findings {
		if s.gate.withholdDuringStartup(f, now, touched) {
			continue
		}
		name := eventName(f)
		if _, ok := byEvent[name]; !ok {
			order = append(order, name)
		}
		byEvent[name] = f
	}
	s.gate.prune(touched)
	for _, name := range order {
		f := byEvent[name]
		if err := s.w.write(ctx, f); err != nil {
			logger.Error(err, "emit "+s.label+" finding", "reason", string(f.Reason))
		}
	}
}
