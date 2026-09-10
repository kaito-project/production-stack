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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestValidateCounterSnapshots(t *testing.T) {
	tests := []struct {
		name    string
		before  PodMetricSnapshot
		after   PodMetricSnapshot
		wantErr bool
	}{
		{
			name:   "stable pod set and increasing counters",
			before: PodMetricSnapshot{"pod-a": 10, "pod-b": 20},
			after:  PodMetricSnapshot{"pod-a": 12, "pod-b": 20},
		},
		{
			name:    "pod added",
			before:  PodMetricSnapshot{"pod-a": 10},
			after:   PodMetricSnapshot{"pod-a": 12, "pod-b": 1},
			wantErr: true,
		},
		{
			name:    "pod replaced",
			before:  PodMetricSnapshot{"pod-a": 10},
			after:   PodMetricSnapshot{"pod-b": 12},
			wantErr: true,
		},
		{
			name:    "counter reset",
			before:  PodMetricSnapshot{"pod-a": 10},
			after:   PodMetricSnapshot{"pod-a": 2},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateCounterSnapshots(test.before, test.after)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateCounterSnapshots() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestModelMetricWrappersPreservePresence(t *testing.T) {
	const model = "falcon"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/pods") {
			pods := corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}}
			for _, name := range []string{"positive", "zero", "missing"} {
				pods.Items = append(pods.Items, corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Name: name, Namespace: "test", Labels: map[string]string{"kaito.sh/shadow-pod-for": model},
				}})
			}
			writer.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(writer).Encode(pods); err != nil {
				t.Error(err)
			}
			return
		}
		switch {
		case strings.Contains(request.URL.Path, "/positive:"):
			_, _ = writer.Write([]byte(`vllm:request_success_total{model_name="falcon"} 7`))
		case strings.Contains(request.URL.Path, "/zero:"):
			_, _ = writer.Write([]byte(`vllm:request_success_total{model_name="falcon"} 0`))
		case strings.Contains(request.URL.Path, "/missing:"):
			_, _ = writer.Write([]byte(`# no samples yet`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	want := PodMetricSnapshot{"positive": 7, "zero": 0, "missing": 0}
	snapshot, present, err := ScrapeModelMetricWithPresence(ctx, clientset, "test", model, "vllm:request_success_total")
	if err != nil || present != 2 || !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("snapshot=%v present=%d err=%v", snapshot, present, err)
	}
	for name, scrape := range map[string]func() (PodMetricSnapshot, error){
		"generic": func() (PodMetricSnapshot, error) {
			return ScrapeModelMetric(ctx, clientset, "test", model, "vllm:request_success_total")
		},
		"success counter": func() (PodMetricSnapshot, error) { return ScrapeRequestSuccessTotal(ctx, clientset, "test", model) },
	} {
		t.Run(name, func(t *testing.T) {
			got, err := scrape()
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("snapshot=%v err=%v", got, err)
			}
		})
	}
	if TotalDelta(snapshot) != 7 || SumSnapshot(snapshot) != 7 {
		t.Fatal("snapshot sums changed")
	}
}
