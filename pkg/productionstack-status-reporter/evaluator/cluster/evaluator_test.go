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

package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/config"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
)

func deployment(ns, name string, labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
	}
}

func pod(ns, name, node string, labels map[string]string, ready corev1.ConditionStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec:       corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready, Reason: "ContainersNotReady"}},
		},
	}
}

// healthyDeployment returns a Deployment with one Ready pod.
func healthyDeployment(ns, name string) []runtime.Object {
	labels := map[string]string{"app": name}
	return []runtime.Object{deployment(ns, name, labels), pod(ns, name+"-0", "n0", labels, corev1.ConditionTrue)}
}

// healthyCluster returns every component the config probes, healthy.
func healthyCluster(cfg config.Config) []runtime.Object {
	var objs []runtime.Object
	for _, d := range [][2]string{
		{cfg.IstioNamespace, cfg.IstiodDeployment},
		{cfg.BBRNamespace, cfg.BBRDeployment},
		{cfg.KaitoNamespace, cfg.KaitoDeployment},
		{cfg.KedaScalerNamespace, cfg.KedaScalerDeployment},
		{cfg.KedaNamespace, "keda-operator"},
		{cfg.KedaNamespace, "keda-operator-metrics-apiserver"},
		{cfg.GatewayAuthNamespace, cfg.GatewayAuthDeployment},
		{cfg.GatewayAuthNamespace, cfg.GatewayAuthOperatorDeployment},
		{cfg.AzureAuthz.Namespace, cfg.AzureAuthz.Name},
		{cfg.LocalCSIDriver.Namespace, cfg.LocalCSIDriver.ManagerDeployment},
	} {
		if d[1] == "" {
			continue
		}
		objs = append(objs, healthyDeployment(d[0], d[1])...)
	}
	objs = append(objs, &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: cfg.LocalCSIDriver.Namespace, Name: cfg.LocalCSIDriver.NodeDaemonSet},
		Spec:       appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "csi-node"}}},
	})
	return objs
}

func evaluate(t *testing.T, cfg config.Config, objs ...runtime.Object) map[reason.Reason]evaluator.Finding {
	t.Helper()
	findings, err := New(fake.NewSimpleClientset(objs...), cfg).Evaluate(context.Background())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	out := map[reason.Reason]evaluator.Finding{}
	for _, f := range findings {
		out[f.Reason] = f
	}
	return out
}

func TestHealthyClusterHasNoFindings(t *testing.T) {
	cfg := config.DefaultConfig()
	if got := evaluate(t, cfg, healthyCluster(cfg)...); len(got) != 0 {
		t.Fatalf("healthy cluster produced findings: %v", got)
	}
}

// Only components that exist are checked: on an empty cluster nothing is
// reported.
func TestMissingComponentsAreNotReported(t *testing.T) {
	if got := evaluate(t, config.DefaultConfig()); len(got) != 0 {
		t.Fatalf("missing components must not be reported, got %v", got)
	}
}

// Each component has a reason of its own, including the KEDA and
// llm-gateway-auth Deployments that once shared one, so the reason alone names
// the component that is not ready.
func TestEachComponentHasItsOwnReason(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AzureAuthz = config.AzureAuthzRef{Name: "azure-authz", Namespace: "kube-system"}
	for _, tc := range []struct {
		namespace, deployment string
		want                  reason.Reason
	}{
		{cfg.KedaNamespace, "keda-operator", reason.ClusterKedaOperatorNotReady},
		{cfg.KedaNamespace, "keda-operator-metrics-apiserver", reason.ClusterKedaMetricsAPIServerNotReady},
		{cfg.GatewayAuthNamespace, cfg.GatewayAuthDeployment, reason.ClusterAPIKeyAuthzNotReady},
		{cfg.GatewayAuthNamespace, cfg.GatewayAuthOperatorDeployment, reason.ClusterAPIKeyOperatorNotReady},
		{cfg.AzureAuthz.Namespace, cfg.AzureAuthz.Name, reason.ClusterAzureAuthzNotReady},
	} {
		labels := map[string]string{"app": tc.deployment}
		objs := append(healthyCluster(cfg), pod(tc.namespace, tc.deployment+"-broken", "n0", labels, corev1.ConditionFalse))
		got := evaluate(t, cfg, objs...)
		if f, ok := got[tc.want]; len(got) != 1 || !ok || !strings.Contains(f.Message, tc.namespace+"/"+tc.deployment+" ") {
			t.Errorf("%s not ready: want only %s naming it, got %v", tc.deployment, tc.want, got)
		}
	}
}

func TestLocalCSIDriver(t *testing.T) {
	cfg := config.DefaultConfig()
	ns := cfg.LocalCSIDriver.Namespace
	nodeLabels := map[string]string{"app": "csi-node"}
	fakeNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "fake-gpu", Labels: map[string]string{fakeNodeLabel: "true"}}}

	t.Run("manager missing", func(t *testing.T) {
		var objs []runtime.Object
		for _, o := range healthyCluster(cfg) {
			if d, ok := o.(*appsv1.Deployment); ok && d.Name == cfg.LocalCSIDriver.ManagerDeployment {
				continue
			}
			objs = append(objs, o)
		}
		if f, ok := evaluate(t, cfg, objs...)[reason.ClusterLocalCSIManagerNotReady]; ok {
			t.Fatalf("a missing manager must not be reported, got %+v", f)
		}
	})
	t.Run("manager scaled to zero", func(t *testing.T) {
		var objs []runtime.Object
		for _, o := range healthyCluster(cfg) {
			if p, ok := o.(*corev1.Pod); ok && p.Labels["app"] == cfg.LocalCSIDriver.ManagerDeployment {
				continue
			}
			if d, ok := o.(*appsv1.Deployment); ok && d.Name == cfg.LocalCSIDriver.ManagerDeployment {
				d = d.DeepCopy()
				d.Spec.Replicas = new(int32)
				o = d
			}
			objs = append(objs, o)
		}
		f, ok := evaluate(t, cfg, objs...)[reason.ClusterLocalCSIManagerNotReady]
		if !ok || !strings.Contains(f.Message, "scaled to zero") {
			t.Fatalf("a manager scaled to zero must flag clusterLocalCSIManagerNotReady, got %+v", f)
		}
	})
	t.Run("node DaemonSet missing", func(t *testing.T) {
		var objs []runtime.Object
		for _, o := range healthyCluster(cfg) {
			if _, ok := o.(*appsv1.DaemonSet); ok {
				continue
			}
			objs = append(objs, o)
		}
		if f, ok := evaluate(t, cfg, objs...)[reason.ClusterLocalCSINodeNotReady]; ok {
			t.Fatalf("a missing node DaemonSet must not be reported, got %+v", f)
		}
	})
	t.Run("node pod not ready on a real node", func(t *testing.T) {
		objs := append(healthyCluster(cfg), pod(ns, "csi-node-a", "aks-0", nodeLabels, corev1.ConditionFalse))
		f, ok := evaluate(t, cfg, objs...)[reason.ClusterLocalCSINodeNotReady]
		if !ok || !strings.Contains(f.Message, "aks-0") {
			t.Fatalf("not-ready node pod must flag clusterLocalCSINodeNotReady naming the node, got %+v", f)
		}
	})
	t.Run("node pod not ready on a gpu-node-mocker node is ignored", func(t *testing.T) {
		objs := append(healthyCluster(cfg), fakeNode, pod(ns, "csi-node-f", "fake-gpu", nodeLabels, corev1.ConditionFalse))
		if f, ok := evaluate(t, cfg, objs...)[reason.ClusterLocalCSINodeNotReady]; ok {
			t.Fatalf("pod on fake node must be ignored, got %+v", f)
		}
	})
	t.Run("node pod on a node that has only just registered waits for its node", func(t *testing.T) {
		now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
		for _, tc := range []struct {
			nodeAge time.Duration
			want    bool
		}{{time.Minute, true}, {10 * time.Minute, false}} {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "aks-1", CreationTimestamp: metav1.NewTime(now.Add(-tc.nodeAge))}}
			e := New(fake.NewSimpleClientset(append(healthyCluster(cfg), node, pod(ns, "csi-node-b", "aks-1", nodeLabels, corev1.ConditionFalse))...), cfg)
			e.clock = func() time.Time { return now }
			findings, err := e.Evaluate(context.Background())
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if len(findings) != 1 || findings[0].Reason != reason.ClusterLocalCSINodeNotReady || findings[0].AwaitingNode != tc.want {
				t.Errorf("node registered %s ago: want clusterLocalCSINodeNotReady with AwaitingNode=%t, got %+v", tc.nodeAge, tc.want, findings)
			}
		}
	})
	t.Run("a Node that cannot be read leaves the node pod unknown this pass", func(t *testing.T) {
		cs := fake.NewSimpleClientset(append(healthyCluster(cfg), pod(ns, "csi-node-c", "aks-2", nodeLabels, corev1.ConditionFalse))...)
		cs.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("apiserver unavailable")
		})
		findings, err := New(cs, cfg).Evaluate(context.Background())
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if len(findings) != 0 {
			t.Fatalf("an unreadable Node must not report its node pod, got %+v", findings)
		}
	})
	t.Run("disabled when namespace is empty", func(t *testing.T) {
		c := cfg
		c.LocalCSIDriver.Namespace = ""
		got := evaluate(t, c)
		if _, ok := got[reason.ClusterLocalCSIManagerNotReady]; ok {
			t.Fatalf("manager check must be skipped when disabled")
		}
		if _, ok := got[reason.ClusterLocalCSINodeNotReady]; ok {
			t.Fatalf("node check must be skipped when disabled")
		}
	})
}
