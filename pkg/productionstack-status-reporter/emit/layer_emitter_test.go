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
	"testing"
	"time"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
)

// withholdDuringStartup implements scheme 1 (debounce) for findings without a
// backing object and scheme 2 (object age) for those with one, and always lets
// exempt findings through. Each layer owns this startup gate.
func TestStartupGateWithhold(t *testing.T) {
	grace := 3 * time.Minute
	now := time.Now()
	g := newStartupGate(func() time.Time { return now }, grace)

	// Exempt findings are never withheld.
	exempt := evaluator.Finding{Reason: reason.InferencesetModelPodsNotReady, GroupKey: "g", StartupGraceExempt: true}
	if g.withholdDuringStartup(exempt, now, map[string]bool{}) {
		t.Errorf("exempt finding must not be withheld")
	}

	// Scheme 2: young backing object is withheld, old one surfaces.
	young := evaluator.Finding{Reason: reason.InferencesetEPPNotReady, GroupKey: "epp", ResourceCreatedAt: now.Add(-1 * time.Minute)}
	if !g.withholdDuringStartup(young, now, map[string]bool{}) {
		t.Errorf("young resource must be withheld")
	}
	old := evaluator.Finding{Reason: reason.InferencesetEPPNotReady, GroupKey: "epp", ResourceCreatedAt: now.Add(-10 * time.Minute)}
	if g.withholdDuringStartup(old, now, map[string]bool{}) {
		t.Errorf("old resource must surface")
	}

	// Scheme 1: no backing object -> debounce on first observation, surface
	// once the problem has persisted for the grace window.
	touched := map[string]bool{}
	crd := evaluator.Finding{Reason: reason.ClusterCRDMissing, GroupKey: "crd/x"}
	if !g.withholdDuringStartup(crd, now, touched) {
		t.Errorf("first observation must be withheld (debounce)")
	}
	if !g.withholdDuringStartup(crd, now.Add(1*time.Minute), touched) {
		// still within grace window relative to first observation
		t.Errorf("within grace window must stay withheld")
	}
	if g.withholdDuringStartup(crd, now.Add(grace+time.Second), touched) {
		t.Errorf("after grace window must surface")
	}
}

// prune drops the debounce timers of findings not observed this pass, so a
// problem that recovers and later recurs is debounced afresh.
func TestStartupGatePrune(t *testing.T) {
	g := newStartupGate(time.Now, 3*time.Minute)
	g.notReadySince["crd/x|clusterCRDMissing"] = time.Now()
	g.prune(map[string]bool{})
	if len(g.notReadySince) != 0 {
		t.Errorf("prune left %d stale entries", len(g.notReadySince))
	}
}
