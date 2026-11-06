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
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

// holdingFixture fakes a Deployment that another controller scales back up on
// chosen polls, and the reporter Event that appears once the Deployment has
// stayed at zero for emitAfter consecutive polls.
type holdingFixture struct {
	clientset  *fake.Clientset
	deployment *appsv1.Deployment
	scaleCalls []int32
}

// newHoldingFixture reverts the Deployment to 2 replicas on the Get calls for
// which revert returns true (1-based).
func newHoldingFixture(t *testing.T, revert func(get int) bool, emitAfter int) *holdingFixture {
	t.Helper()
	interval := PollInterval
	PollInterval = time.Millisecond
	t.Cleanup(func() { PollInterval = interval })

	f := &holdingFixture{deployment: &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "body-based-router", Namespace: "kube-system"},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(0))},
	}}
	f.clientset = fake.NewSimpleClientset(f.deployment)
	gets, zeroPolls := 0, 0
	f.clientset.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "scale" {
			return true, &autoscalingv1.Scale{Spec: autoscalingv1.ScaleSpec{Replicas: *f.deployment.Spec.Replicas}}, nil
		}
		gets++
		if revert(gets) {
			f.deployment.Spec.Replicas = ptr.To(int32(2))
		}
		if *f.deployment.Spec.Replicas == 0 {
			zeroPolls++
		} else {
			zeroPolls = 0
		}
		if zeroPolls == emitAfter {
			_ = f.clientset.Tracker().Add(&corev1.Event{
				ObjectMeta:    metav1.ObjectMeta{Name: "bbr", Namespace: ReporterEventNamespace},
				Reason:        "clusterBBRNotReady",
				Source:        corev1.EventSource{Component: ReporterEventSource},
				LastTimestamp: metav1.Now(),
			})
		}
		return true, f.deployment.DeepCopy(), nil
	})
	f.clientset.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		scale := action.(k8stesting.UpdateAction).GetObject().(*autoscalingv1.Scale)
		f.scaleCalls = append(f.scaleCalls, scale.Spec.Replicas)
		f.deployment.Spec.Replicas = ptr.To(scale.Spec.Replicas)
		return true, scale, nil
	})
	return f
}

func (f *holdingFixture) wait(timeout time.Duration) (corev1.Event, error) {
	return waitForReporterEventHoldingScaledToZero(context.Background(), f.clientset,
		"kube-system", "body-based-router", "clusterBBRNotReady", "", metav1.Time{}, timeout)
}

func TestWaitForReporterEventHoldingScaledToZero(t *testing.T) {
	t.Run("returns the Event while the Deployment stays at zero", func(t *testing.T) {
		f := newHoldingFixture(t, func(int) bool { return false }, 3)
		if _, err := f.wait(time.Second); err != nil {
			t.Fatal(err)
		}
		if len(f.scaleCalls) != 0 {
			t.Fatalf("scale calls = %v, want none", f.scaleCalls)
		}
	})

	// Regression: an add-on manager restored the replicas mid-wait, the pods
	// came back, and the Warning never appeared before the timeout.
	t.Run("scales back to zero after a revert", func(t *testing.T) {
		f := newHoldingFixture(t, func(get int) bool { return get == 2 }, 3)
		if _, err := f.wait(time.Second); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(f.scaleCalls, []int32{0}) {
			t.Fatalf("scale calls = %v, want one scale-down", f.scaleCalls)
		}
	})

	t.Run("gives up when the Deployment keeps being scaled back up", func(t *testing.T) {
		f := newHoldingFixture(t, func(get int) bool { return get%2 == 0 }, 5)
		_, err := f.wait(time.Second)
		if err == nil || !strings.Contains(err.Error(), "scaled back up") {
			t.Fatalf("err = %v, want the scaled-back-up error", err)
		}
		if len(f.scaleCalls) != maxScaledToZeroRestores {
			t.Fatalf("scale calls = %v, want %d scale-downs", f.scaleCalls, maxScaledToZeroRestores)
		}
	})
}

func TestReporterTimingsFromArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    ReporterTimings
		wantErr bool
	}{
		{
			name: "chart defaults when the flags are absent",
			args: []string{"--leader-elect"},
			want: ReporterTimings{Resync: 60 * time.Second, StartupGrace: 60 * time.Second, EventRefresh: 300 * time.Second},
		},
		{
			name: "E2E install",
			args: []string{"--resync-seconds=60", "--startup-grace-seconds=30", "--event-refresh-seconds=120"},
			want: ReporterTimings{Resync: 60 * time.Second, StartupGrace: 30 * time.Second, EventRefresh: 120 * time.Second},
		},
		{
			name: "zero startup grace is allowed",
			args: []string{"--startup-grace-seconds=0"},
			want: ReporterTimings{Resync: 60 * time.Second, StartupGrace: 0, EventRefresh: 300 * time.Second},
		},
		{name: "zero resync", args: []string{"--resync-seconds=0"}, wantErr: true},
		{name: "negative grace", args: []string{"--startup-grace-seconds=-1"}, wantErr: true},
		{name: "not a number", args: []string{"--event-refresh-seconds=5m"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := reporterTimingsFromArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDebouncedEmitTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timings ReporterTimings
		want    time.Duration
	}{
		// Regression: with the chart's 60s grace, a fixed 3m wait timed out
		// on clusterBBRNotReady.
		{
			name:    "chart defaults",
			timings: ReporterTimings{Resync: 60 * time.Second, StartupGrace: 60 * time.Second},
			want:    270 * time.Second,
		},
		{
			name:    "E2E install rounds the grace up to one resync",
			timings: ReporterTimings{Resync: 60 * time.Second, StartupGrace: 30 * time.Second},
			want:    270 * time.Second,
		},
		{
			name:    "grace longer than one resync",
			timings: ReporterTimings{Resync: 60 * time.Second, StartupGrace: 90 * time.Second},
			want:    330 * time.Second,
		},
		{
			name:    "no grace",
			timings: ReporterTimings{Resync: 30 * time.Second},
			want:    150 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.timings.DebouncedEmitTimeout(); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}
