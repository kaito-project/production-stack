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

// Package controllers implements the productionstack-status-reporter: the
// single producer of the §1.2 control-plane reason catalogue as Kubernetes
// Events in kube-system. On every resync it asks the evaluator package to
// evaluate every reason across the cluster, modelharness, and modeldeployment
// layers (install-time misconfig + post-install drift), applies the §1.4
// cross-layer upstream-gating suppression, and emits the result as Events whose
// involvedObject is always a cluster-scoped resource (a Namespace or a
// CustomResourceDefinition).
package controllers

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/config"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/emit"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator/cluster"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator/harness"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator/modeldeployment"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator/weightdownload"
)

// layerID identifies one reporting layer in the StatusReporter's layer map.
type layerID int

const (
	layerCluster layerID = iota
	layerHarness
	layerModeldeployment
	layerWeightDownload
)

// layerOrder fixes the deterministic evaluate / emit sequence over the
// layer map.
var layerOrder = []layerID{layerCluster, layerHarness, layerModeldeployment, layerWeightDownload}

// layer bundles a reporting layer's Evaluator with the Emitter strategy that
// turns its Findings into events.
type layer struct {
	eval    evaluator.Evaluator
	emitter emit.LayerEmitter
}

// StatusReporter is the leader-elected manager Runnable that, on every resync,
// calls each layer Evaluator for the full §1.2 reason catalogue, applies the
// §1.4 cross-layer suppression, and surfaces the result as aggregated
// Kubernetes Events in kube-system.
type StatusReporter struct {
	cfg config.Config

	// layers maps each reporting layer to its Evaluator and the emit strategy
	// that turns its Findings into events; layerOrder fixes the deterministic
	// evaluate / emit sequence. Each strategy owns its startup-grace
	// debounce state and clock across passes.
	layers map[layerID]layer
}

// NewStatusReporter constructs a StatusReporter and the layer evaluators it
// drives.
func NewStatusReporter(cs kubernetes.Interface, dyn dynamic.Interface, dc discovery.DiscoveryInterface, cfg config.Config) *StatusReporter {
	clock := time.Now
	grace := cfg.StartupGracePeriod
	r := &StatusReporter{cfg: cfg}
	r.layers = map[layerID]layer{
		layerCluster: {
			eval:    cluster.New(cs, dc, cfg),
			emitter: emit.AllFindings(cs, clock, grace, "cluster"),
		},
		layerHarness: {
			eval:    harness.New(cs, dyn, cfg),
			emitter: emit.PerGroup(cs, clock, grace, func(f evaluator.Finding) string { return f.WorkloadNamespace }, "harness"),
		},
		layerModeldeployment: {
			eval:    modeldeployment.New(cs, dyn),
			emitter: emit.PerGroup(cs, clock, grace, func(f evaluator.Finding) string { return f.GroupKey }, "modeldeployment"),
		},
		layerWeightDownload: {
			eval:    weightdownload.New(cs, dyn, cfg),
			emitter: emit.PerGroup(cs, clock, grace, func(f evaluator.Finding) string { return f.GroupKey }, "inferencesetWeightDownloadSlow"),
		},
	}
	return r
}

// NeedLeaderElection ensures only the leader replica emits events, so HA
// replicas do not duplicate the event stream.
func (r *StatusReporter) NeedLeaderElection() bool { return true }

// Start runs the resync loop until ctx is cancelled. It satisfies
// manager.Runnable.
func (r *StatusReporter) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("status-reporter")
	logger.Info("starting status reporter", "resyncInterval", r.cfg.ResyncInterval)

	r.reconcile(ctx, logger)

	ticker := time.NewTicker(r.cfg.ResyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.reconcile(ctx, logger)
		}
	}
}

// ReconcileOnce runs a single evaluation/emission pass. Exposed for tests.
func (r *StatusReporter) ReconcileOnce(ctx context.Context) {
	r.reconcile(ctx, logr.Discard())
}

// reconcile performs one full evaluation + emission pass.
func (r *StatusReporter) reconcile(ctx context.Context, logger logr.Logger) {
	// 1. Evaluate every layer fully (unfiltered) so we can compute which
	//    downstream reasons are actually being suppressed for the §1.4
	//    transparency suffix. The cluster evaluator never errors; the
	//    namespace-scoped evaluators error only when discovery fails.
	findings := make(map[layerID][]evaluator.Finding, len(layerOrder))
	for _, id := range layerOrder {
		findings[id] = r.evaluate(ctx, r.layers[id].eval, logger)
	}

	// 2. Compute suppression sets (§1.4).
	supp := emit.ComputeSuppression(findings[layerCluster], findings[layerHarness], findings[layerModeldeployment])

	// 3. Emit every layer in order, gating transient startup states (scheme 1 +
	//    2). Each layer strategy owns its debounce bookkeeping and prunes it; the
	//    pass logger travels on the context.
	ctx = logr.NewContext(ctx, logger)
	for _, id := range layerOrder {
		r.layers[id].emitter.Emit(ctx, findings[id], supp)
	}
}

// evaluate runs one Evaluator, logging any error.
func (r *StatusReporter) evaluate(ctx context.Context, e evaluator.Evaluator, logger logr.Logger) []evaluator.Finding {
	findings, err := e.Evaluate(ctx)
	if err != nil {
		logger.Error(err, "evaluate layer", "evaluator", e.Name())
	}
	return findings
}
