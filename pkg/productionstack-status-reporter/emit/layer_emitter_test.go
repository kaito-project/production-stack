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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/config"
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
	exempt := evaluator.Finding{Reason: reason.Reason("QuotaExceeded"), GroupKey: "g", StartupGraceExempt: true}
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
	crd := evaluator.Finding{Reason: reason.ClusterBBRNotReady, GroupKey: "cluster/clusterBBRNotReady"}
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
	g.notReadySince["cluster/clusterBBRNotReady|clusterBBRNotReady"] = time.Now()
	g.prune(map[string]bool{})
	if len(g.notReadySince) != 0 {
		t.Errorf("prune left %d stale entries", len(g.notReadySince))
	}
}

// A pod that has no node yet is withheld for the node-provisioning window: a
// shorter or zero global grace does not shorten it, and a longer one is kept.
func TestStartupGateHoldsPodAwaitingNode(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		grace    time.Duration
		age      time.Duration
		withheld bool
	}{
		{"held past the global grace", time.Minute, 4 * time.Minute, true},
		{"surfaces after the node window", time.Minute, evaluator.NodeProvisioningGracePeriod + time.Second, false},
		{"held when the global grace is zero", 0, 4 * time.Minute, true},
		{"a longer global grace is kept", 10 * time.Minute, 6 * time.Minute, true},
	} {
		g := newStartupGate(func() time.Time { return now }, tc.grace)
		f := evaluator.Finding{Reason: reason.InferencesetEPPNotReady, GroupKey: "epp", ResourceCreatedAt: now.Add(-tc.age), AwaitingNode: true}
		if got := g.withholdDuringStartup(f, now, map[string]bool{}); got != tc.withheld {
			t.Errorf("%s: withheld=%v, want %v", tc.name, got, tc.withheld)
		}
	}
}

// The findings of one Event are gated one by one, so a pod still waiting for a
// node does not hide another that is failing: the Event is written once, with
// the message of the first finding past its window.
func TestEmitReportsFailingPodWhileAnotherAwaitsNode(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	cs := fake.NewSimpleClientset()
	e := NewEmitter(cs, func() time.Time { return now }, time.Minute, 5*time.Minute, "cluster")
	finding := func(msg string, awaitingNode bool) evaluator.Finding {
		return evaluator.Finding{
			Reason:            reason.ClusterKedaNotReady,
			Object:            evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: "keda"},
			Message:           msg,
			GroupKey:          "cluster/keda",
			ResourceCreatedAt: now.Add(-2 * time.Minute),
			AwaitingNode:      awaitingNode,
		}
	}
	waiting := finding("keda-operator waits for a node", true)
	e.Emit(ctx, []evaluator.Finding{waiting, finding("keda-operator-metrics-apiserver crashes", false), finding("a later crashing pod", false)})

	ev, err := cs.CoreV1().Events(config.ReportingNamespace).Get(ctx, NameForLookup(waiting).Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the crashing pod must be reported while another waits for a node: %v", err)
	}
	if ev.Message != "keda-operator-metrics-apiserver crashes" || ev.Count != 1 {
		t.Errorf("event message=%q count=%d, want the first finding past its window, written once", ev.Message, ev.Count)
	}
}
