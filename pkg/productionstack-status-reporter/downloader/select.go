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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Annotations KAITO reads from the Workspace — which it copies from the
// InferenceSet's spec.template.metadata.annotations — to choose how the model
// weights are obtained.
const (
	annotationUseLocalWeights = "kaito.sh/use-local-weights"
	annotationModelStreaming  = "kaito.sh/model-streaming"
	annotationRuntime         = "kaito.sh/runtime"

	modelStreamingDisabled = "disabled"
	runtimeTransformers    = "transformers"
)

// Path is the download path KAITO uses for an InferenceSet's model weights.
type Path string

const (
	// PathNone means KAITO downloads nothing: the InferenceSet has no preset
	// (a user-defined pod template) or loads local weights from the node.
	PathNone Path = "None"
	// PathVLLM is the vLLM self-download inside the model pod.
	PathVLLM Path = "VLLM"
	// PathModelMirror is a ModelMirror (model streaming).
	PathModelMirror Path = "ModelMirror"
)

// Select returns the download path KAITO uses for the InferenceSet, mirroring
// KAITO's own decision (modelstreaming.ModelStreamingEnabled and the preset
// inference generator). modelStreamingEnabled is KAITO's ModelStreaming
// feature gate, which is not visible on any Kubernetes object (§1.4).
func Select(is *unstructured.Unstructured, modelStreamingEnabled bool) Path {
	if presetName(is) == "" {
		return PathNone
	}
	ann, _, _ := unstructured.NestedStringMap(is.Object, "spec", "template", "metadata", "annotations")
	if ann[annotationUseLocalWeights] == "true" {
		return PathNone
	}
	if modelStreamingEnabled &&
		ann[annotationModelStreaming] != modelStreamingDisabled &&
		ann[annotationRuntime] != runtimeTransformers {
		return PathModelMirror
	}
	return PathVLLM
}

// presetName returns the InferenceSet's spec.template.inference.preset.name.
func presetName(is *unstructured.Unstructured) string {
	name, _, _ := unstructured.NestedString(is.Object, "spec", "template", "inference", "preset", "name")
	return name
}
