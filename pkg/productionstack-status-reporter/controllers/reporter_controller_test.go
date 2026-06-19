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

package controllers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/config"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/emit"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
)

func newTestReporter(objects ...runtime.Object) (*StatusReporter, *fake.Clientset) {
	cs := fake.NewSimpleClientset(objects...)
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	cfg := config.DefaultConfig()
	// Disable the startup grace gate by default so findings surface on the
	// first reconcile; tests that exercise the gate set it explicitly.
	cfg.StartupGracePeriod = 0
	r := NewStatusReporter(cs, dyn, cs.Discovery(), cfg)
	return r, cs
}

// On an empty cluster every required CRD is absent, so the reporter must emit
// a clusterCRDMissing Warning per missing CRD, on a CRD-scoped involvedObject,
// in kube-system, sourced by the reporter component.
func TestReconcileEmitsClusterCRDMissing(t *testing.T) {
	r, cs := newTestReporter()
	ctx := context.Background()

	r.ReconcileOnce(ctx)

	name := emit.NameForLookup(reason.ClusterCRDMissing, evaluator.InvolvedObject{Kind: evaluator.KindCRD, Name: "inferencesets.kaito.sh"})
	ev, err := cs.CoreV1().Events(config.ReportingNamespace).Get(ctx, name.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected clusterCRDMissing event for inferencesets CRD: %v", err)
	}
	if ev.Type != corev1.EventTypeWarning {
		t.Errorf("event type=%q, want Warning", ev.Type)
	}
	if ev.Reason != string(reason.ClusterCRDMissing) {
		t.Errorf("event reason=%q, want %q", ev.Reason, reason.ClusterCRDMissing)
	}
	if ev.Source.Component != config.ReporterComponent {
		t.Errorf("event source=%q, want %q", ev.Source.Component, config.ReporterComponent)
	}
	if ev.InvolvedObject.Kind != "CustomResourceDefinition" {
		t.Errorf("involvedObject kind=%q, want CustomResourceDefinition", ev.InvolvedObject.Kind)
	}
}

// Repeated reconciles must aggregate onto the same Event (count bump) rather
// than create duplicates (§1.1).
func TestReconcileAggregatesRepeatEvents(t *testing.T) {
	r, cs := newTestReporter()
	ctx := context.Background()

	r.ReconcileOnce(ctx)
	r.ReconcileOnce(ctx)

	name := emit.NameForLookup(reason.ClusterCRDMissing, evaluator.InvolvedObject{Kind: evaluator.KindCRD, Name: "inferencesets.kaito.sh"})
	ev, err := cs.CoreV1().Events(config.ReportingNamespace).Get(ctx, name.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected aggregated event: %v", err)
	}
	if ev.Count < 2 {
		t.Errorf("event count=%d, want >= 2 after two reconciles", ev.Count)
	}
}

// No namespaces carry the discovery label, so no modelharness events are
// produced.
func TestReconcileNoManagedNamespacesNoHarnessEvents(t *testing.T) {
	r, cs := newTestReporter()
	ctx := context.Background()

	r.ReconcileOnce(ctx)

	events, err := cs.CoreV1().Events(config.ReportingNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, ev := range events.Items {
		if reason.LayerOf(reason.Reason(ev.Reason)) == reason.LayerModelharness {
			t.Errorf("unexpected modelharness event %q with no managed namespaces", ev.Reason)
		}
	}
}
