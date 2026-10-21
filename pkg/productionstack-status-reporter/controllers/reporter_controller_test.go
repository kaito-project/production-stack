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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/config"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/emit"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator/util"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
)

func newTestReporter(objects ...runtime.Object) (*StatusReporter, *fake.Clientset) {
	cs := fake.NewSimpleClientset(objects...)
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	cfg := config.DefaultConfig()
	// Disable the startup grace gate by default so findings surface on the
	// first reconcile; tests that exercise the gate set it explicitly.
	cfg.StartupGracePeriod = 0
	r := NewStatusReporter(cs, dyn, cfg)
	return r, cs
}

// On an empty cluster every shared control-plane Deployment is absent, so the
// reporter must emit a cluster*NotReady Warning per missing Deployment, on a
// Namespace-scoped involvedObject, in kube-system, sourced by the reporter
// component.
func TestReconcileEmitsClusterDeploymentNotReady(t *testing.T) {
	r, cs := newTestReporter()
	ctx := context.Background()

	r.ReconcileOnce(ctx)

	name := emit.NameForLookup(evaluator.Finding{
		Reason:   reason.ClusterBBRNotReady,
		Object:   evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: "kaito-system"},
		GroupKey: "cluster/kaito-system",
	})
	ev, err := cs.CoreV1().Events(config.ReportingNamespace).Get(ctx, name.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected clusterBBRNotReady event: %v", err)
	}
	if ev.Type != corev1.EventTypeWarning {
		t.Errorf("event type=%q, want Warning", ev.Type)
	}
	if ev.Reason != string(reason.ClusterBBRNotReady) {
		t.Errorf("event reason=%q, want %q", ev.Reason, reason.ClusterBBRNotReady)
	}
	if ev.Source.Component != config.ReporterComponent {
		t.Errorf("event source=%q, want %q", ev.Source.Component, config.ReporterComponent)
	}
	if ev.InvolvedObject.Kind != "Namespace" {
		t.Errorf("involvedObject kind=%q, want Namespace", ev.InvolvedObject.Kind)
	}
}

// Repeated reconciles refresh the same Event (count bump) rather than create
// duplicates, but no more often than the refresh interval (§1.1).
func TestReconcileRefreshesRepeatEventsAtInterval(t *testing.T) {
	cs := fake.NewSimpleClientset()
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	cfg := config.DefaultConfig()
	cfg.StartupGracePeriod = 0
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	r := newStatusReporter(cs, dyn, cfg, func() time.Time { return now })
	ctx := context.Background()

	name := emit.NameForLookup(evaluator.Finding{
		Reason:   reason.ClusterBBRNotReady,
		Object:   evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: "kaito-system"},
		GroupKey: "cluster/kaito-system",
	})
	count := func() int32 {
		t.Helper()
		ev, err := cs.CoreV1().Events(config.ReportingNamespace).Get(ctx, name.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("expected clusterBBRNotReady event: %v", err)
		}
		return ev.Count
	}

	r.ReconcileOnce(ctx)
	now = now.Add(cfg.ResyncInterval)
	r.ReconcileOnce(ctx)
	if c := count(); c != 1 {
		t.Errorf("event count=%d within the refresh interval, want 1", c)
	}
	now = now.Add(cfg.EventRefreshInterval)
	r.ReconcileOnce(ctx)
	if c := count(); c != 2 {
		t.Errorf("event count=%d after the refresh interval, want 2", c)
	}
}

// managedInferenceSet returns a managed namespace and an InferenceSet in it
// whose InferenceSetReady condition is False with condReason since transition.
func managedInferenceSet(condReason string, transition time.Time) (*corev1.Namespace, *unstructured.Unstructured) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Labels: map[string]string{util.ManagedByLabel: util.ManagedByValue}}}
	is := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kaito.sh/v1beta1",
		"kind":       "InferenceSet",
		"metadata":   map[string]interface{}{"name": "qwen", "namespace": "team-a"},
		"status": map[string]interface{}{"conditions": []interface{}{map[string]interface{}{
			"type":               "InferenceSetReady",
			"status":             "False",
			"reason":             condReason,
			"message":            "inferenceset is not ready, 0/1 replicas are ready",
			"lastTransitionTime": transition.UTC().Format(time.RFC3339),
		}}},
	}}
	return ns, is
}

// A pass-through InferenceSetReady reason is published verbatim: a terminal
// one immediately, any other failure only once it has persisted for 5 minutes
// since the condition's lastTransitionTime.
func TestReconcilePassesThroughInferenceSetReady(t *testing.T) {
	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		reason      string
		wantAtOnce  bool
		wantAfter5m bool
	}{
		{"QuotaExceeded", true, true},
		{"DownloadFailed", false, true},
		{"NodeClaimNotReady", false, false},
	}
	for _, tc := range cases {
		ns, is := managedInferenceSet(tc.reason, start)
		cs := fake.NewSimpleClientset(ns)
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
			map[schema.GroupVersionResource]string{
				util.InferenceSetGVR: "InferenceSetList",
				{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}: "GatewayList",
			}, is)
		cfg := config.DefaultConfig()
		now := start.Add(time.Minute)
		r := newStatusReporter(cs, dyn, cfg, func() time.Time { return now })
		ctx := context.Background()
		published := func() bool {
			events, err := cs.CoreV1().Events(config.ReportingNamespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				t.Fatalf("list events: %v", err)
			}
			for _, ev := range events.Items {
				if ev.Reason == tc.reason && ev.InvolvedObject.Name == "team-a" {
					return true
				}
			}
			return false
		}

		r.ReconcileOnce(ctx)
		if got := published(); got != tc.wantAtOnce {
			t.Errorf("%s after 1m: published=%v, want %v", tc.reason, got, tc.wantAtOnce)
		}
		now = start.Add(5*time.Minute + time.Second)
		r.ReconcileOnce(ctx)
		if got := published(); got != tc.wantAfter5m {
			t.Errorf("%s after 5m: published=%v, want %v", tc.reason, got, tc.wantAfter5m)
		}
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
