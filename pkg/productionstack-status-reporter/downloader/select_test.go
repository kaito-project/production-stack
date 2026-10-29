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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// newInferenceSet builds a minimal InferenceSet in team-a. An empty preset
// leaves spec.template.inference.preset unset (a pod-template InferenceSet).
func newInferenceSet(name, preset, config string, annotations map[string]string) *unstructured.Unstructured {
	inference := map[string]interface{}{}
	if preset != "" {
		inference["preset"] = map[string]interface{}{"name": preset}
	}
	if config != "" {
		inference["config"] = config
	}
	template := map[string]interface{}{"inference": inference}
	if annotations != nil {
		ann := map[string]interface{}{}
		for k, v := range annotations {
			ann[k] = v
		}
		template["metadata"] = map[string]interface{}{"annotations": ann}
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kaito.sh/v1beta1",
		"kind":       "InferenceSet",
		"metadata":   map[string]interface{}{"name": name, "namespace": "team-a"},
		"spec":       map[string]interface{}{"template": template},
	}}
}

func TestSelect(t *testing.T) {
	cases := []struct {
		name      string
		preset    string
		ann       map[string]string
		streaming bool
		want      Path
	}{
		{"pod template downloads nothing", "", nil, true, PathNone},
		{"local weights download nothing", "qwen/qwen3-8b", map[string]string{annotationUseLocalWeights: "true"}, true, PathNone},
		{"streaming enabled uses the mirror", "qwen/qwen3-8b", nil, true, PathModelMirror},
		{"streaming opt-out self-downloads", "qwen/qwen3-8b", map[string]string{annotationModelStreaming: "disabled"}, true, PathVLLM},
		{"transformers runtime self-downloads", "qwen/qwen3-8b", map[string]string{annotationRuntime: "transformers"}, true, PathVLLM},
		{"feature gate off self-downloads", "qwen/qwen3-8b", nil, false, PathVLLM},
		{"use-local-weights false is ignored", "qwen/qwen3-8b", map[string]string{annotationUseLocalWeights: "false"}, false, PathVLLM},
	}
	for _, tc := range cases {
		if got := Select(newInferenceSet("qwen", tc.preset, "", tc.ann), tc.streaming); got != tc.want {
			t.Errorf("%s: Select=%q, want %q", tc.name, got, tc.want)
		}
	}
}
