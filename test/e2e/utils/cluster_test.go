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

package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

type podLogTransport func(*http.Request) (*http.Response, error)

func (transport podLogTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestGetPodLogsHasDeadlineAndPreservesNotFound(t *testing.T) {
	for _, statusCode := range []int{http.StatusOK, http.StatusNotFound} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			clientset, err := kubernetes.NewForConfig(&rest.Config{
				Host: "https://example.invalid",
				Transport: podLogTransport(func(request *http.Request) (*http.Response, error) {
					deadline, ok := request.Context().Deadline()
					if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > HTTPTimeout {
						t.Fatalf("expected bounded log request, deadline=%v present=%v", deadline, ok)
					}
					if request.URL.Path != "/api/v1/namespaces/test/pods/epp/log" || request.URL.Query().Get("container") != "epp" {
						t.Fatalf("unexpected log request: %s", request.URL)
					}
					return &http.Response{
						StatusCode: statusCode,
						Header:     http.Header{"Content-Type": []string{"text/plain"}},
						Body:       io.NopCloser(strings.NewReader("pod logs")),
					}, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			logs, err := GetPodLogs(clientset, "test", "epp", "epp")
			if statusCode == http.StatusNotFound {
				if !apierrors.IsNotFound(err) {
					t.Fatalf("expected NotFound, got %v", err)
				}
			} else if err != nil || logs != "pod logs" {
				t.Fatalf("logs=%q error=%v", logs, err)
			}
		})
	}
}

func TestDeploymentReplicasReady(t *testing.T) {
	for _, test := range []struct {
		name                        string
		want, desired, total, ready int32
		observed                    int64
		exact, success              bool
	}{
		{name: "zero still serving", desired: 0, total: 1, ready: 1, observed: 2, exact: true},
		{name: "zero still draining", total: 1, observed: 2, exact: true},
		{name: "zero stale generation", observed: 1, exact: true},
		{name: "zero wrong target", desired: 1, observed: 2, exact: true},
		{name: "zero converged", observed: 2, exact: true, success: true},
		{name: "exact one still draining", want: 1, desired: 1, total: 2, ready: 1, observed: 2, exact: true},
		{name: "exact one converged", want: 1, desired: 1, total: 1, ready: 1, observed: 2, exact: true, success: true},
		{name: "at least one", want: 1, desired: 2, total: 2, ready: 2, observed: 2, success: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(test.desired)},
				Status:     appsv1.DeploymentStatus{ObservedGeneration: test.observed, Replicas: test.total, ReadyReplicas: test.ready},
			}
			if err := deploymentReplicasReady(deployment, test.want, test.exact); (err == nil) != test.success {
				t.Fatalf("ready error = %v, want success=%t", err, test.success)
			}
		})
	}
}

func TestWaitForDeploymentReplicasDoesNotAcceptZeroEarly(t *testing.T) {
	clientset := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "epp", Namespace: "test", Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(0))},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, ReadyReplicas: 1},
	})
	err := waitForDeploymentReplicas(context.Background(), clientset, "test", "epp", 0, 20*time.Millisecond, true)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "replicas=1") {
		t.Fatalf("expected timeout retaining replica state, got %v", err)
	}
}

func TestDeploymentReplicasReadyWaitsForTerminatingPods(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(0))},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 2, TerminatingReplicas: ptr.To(int32(1))},
	}
	if err := deploymentReplicasReady(deployment, 0, true); err == nil {
		t.Fatal("terminating replicas must prevent scale-down completion")
	}
	deployment.Status.TerminatingReplicas = ptr.To(int32(0))
	if err := deploymentReplicasReady(deployment, 0, true); err != nil {
		t.Fatal(err)
	}
}

func TestPollUntilReadyCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := pollUntilReady(ctx, time.Minute, "test condition", func(context.Context) error {
		calls++
		cancel()
		return fmt.Errorf("not ready yet")
	})
	if !errors.Is(err, context.Canceled) || calls != 1 || !strings.Contains(err.Error(), "not ready yet") {
		t.Fatalf("poll error=%v calls=%d", err, calls)
	}
}

func TestDeploymentReplicaGuardRestoresOriginalCount(t *testing.T) {
	for _, rejectScale := range []bool{false, true} {
		t.Run(fmt.Sprintf("rejectScale=%t", rejectScale), func(t *testing.T) {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "router", Namespace: "test", Generation: 1},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
				Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 3, ReadyReplicas: 3},
			}
			clientset := fake.NewSimpleClientset(deployment)
			var targets []int32
			clientset.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() != "scale" {
					return false, nil, nil
				}
				return true, &autoscalingv1.Scale{Spec: autoscalingv1.ScaleSpec{Replicas: *deployment.Spec.Replicas}}, nil
			})
			clientset.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if action.GetSubresource() != "scale" {
					return false, nil, nil
				}
				scale := action.(k8stesting.UpdateAction).GetObject().(*autoscalingv1.Scale)
				targets = append(targets, scale.Spec.Replicas)
				if rejectScale && scale.Spec.Replicas == 0 {
					return true, nil, errors.New("scale failed")
				}
				deployment.Spec.Replicas = ptr.To(scale.Spec.Replicas)
				deployment.Status.Replicas = scale.Spec.Replicas
				deployment.Status.ReadyReplicas = scale.Spec.Replicas
				err := clientset.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("deployments"), deployment, "test")
				return true, scale, err
			})
			guard, err := newDeploymentReplicaGuard(context.Background(), clientset, "test", "router")
			if err != nil {
				t.Fatal(err)
			}
			if guard.OriginalReplicas() != 3 {
				t.Fatal("original count not captured")
			}
			if err := guard.ScaleAndWait(context.Background(), 0, time.Second); (err != nil) != rejectScale {
				t.Fatalf("scale error=%v rejectScale=%t", err, rejectScale)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if err := guard.Restore(context.Background(), time.Second); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(targets, []int32{0, 3, 3}) {
				t.Fatalf("scale targets=%v", targets)
			}
		})
	}
}
