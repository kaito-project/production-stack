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

package downloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func sha6(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:6]
}

// ModelMirrorName must match KAITO's modelstreaming.ModelMirrorKey: the first
// 6 hex characters of the SHA-256 of the resolved model ID.
func TestModelMirrorName(t *testing.T) {
	cases := []struct {
		name, preset, config, modelID string
	}{
		{"hugging face preset", "Qwen/Qwen3-8B", "", "Qwen/Qwen3-8B"},
		{"legacy short name is resolved", "phi-4", "", "microsoft/phi-4"},
		{"custom preset uses the config", "custom", "my-model-config", "custom-my-model-config"},
	}
	for _, tc := range cases {
		if got, want := ModelMirrorName(tc.preset, tc.config), sha6(tc.modelID); got != want {
			t.Errorf("%s: ModelMirrorName=%q, want sha256(%q)[:6]=%q", tc.name, got, tc.modelID, want)
		}
	}
}

func modelMirror(name string, status map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kaito.sh/v1alpha1",
		"kind":       "ModelMirror",
		"metadata":   map[string]interface{}{"name": name, "namespace": "team-a"},
		"status":     status,
	}}
}

func TestModelMirrorDownloaderObserve(t *testing.T) {
	is := newInferenceSet("qwen", "Qwen/Qwen3-8B", "", nil)
	name := ModelMirrorName("Qwen/Qwen3-8B", "")
	downloading := map[string]interface{}{
		"phase":    "Pending",
		"download": map[string]interface{}{"speedBytesPerSecond": int64(6_000_000), "remainingSeconds": int64(900)},
	}
	cases := []struct {
		name      string
		mirror    *unstructured.Unstructured
		wantPhase Phase
		wantBps   float64
	}{
		{"mirror not created yet", nil, PhaseNotStarted, 0},
		{"pending without progress", modelMirror(name, map[string]interface{}{"phase": "Pending"}), PhaseNotStarted, 0},
		{"downloading", modelMirror(name, downloading), PhaseDownloading, 6_000_000},
		{"unknown remaining time still downloads", modelMirror(name, map[string]interface{}{
			"phase":    "Pending",
			"download": map[string]interface{}{"speedBytesPerSecond": int64(6_000_000), "remainingSeconds": int64(-1)},
		}), PhaseDownloading, 6_000_000},
		{"failed download", modelMirror(name, map[string]interface{}{
			"phase":          "Pending",
			"failureMessage": "download failed",
			"download":       map[string]interface{}{"speedBytesPerSecond": int64(6_000_000), "remainingSeconds": int64(900)},
		}), PhaseNotStarted, 0},
		{"zero speed", modelMirror(name, map[string]interface{}{
			"phase":    "Pending",
			"download": map[string]interface{}{"speedBytesPerSecond": int64(0), "remainingSeconds": int64(900)},
		}), PhaseNotStarted, 0},
		{"ready", modelMirror(name, map[string]interface{}{"phase": "Ready"}), PhaseCompleted, 0},
		{"another model's mirror is ignored", modelMirror("abcdef", downloading), PhaseNotStarted, 0},
	}
	for _, tc := range cases {
		var objs []runtime.Object
		if tc.mirror != nil {
			objs = append(objs, tc.mirror)
		}
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
			map[schema.GroupVersionResource]string{ModelMirrorGVR: "ModelMirrorList"}, objs...)
		got, err := NewModelMirrorDownloader(dyn).Observe(context.Background(), is)
		if err != nil {
			t.Fatalf("%s: Observe: %v", tc.name, err)
		}
		if got.Phase != tc.wantPhase || got.BytesPerSecond != tc.wantBps {
			t.Errorf("%s: got %+v, want phase %s / %v B/s", tc.name, got, tc.wantPhase, tc.wantBps)
		}
		if got.Phase == PhaseDownloading && got.Source != "modelmirror team-a/"+name {
			t.Errorf("%s: source=%q", tc.name, got.Source)
		}
	}
}
