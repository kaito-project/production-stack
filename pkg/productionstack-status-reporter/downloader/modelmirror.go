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
	"fmt"

	"github.com/kaito-project/kaito/pkg/utils/plugin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// ModelMirrorGVR is the KAITO ModelMirror resource. It is read through the
// dynamic client as unstructured, with no dependency on the KAITO Go type.
var ModelMirrorGVR = schema.GroupVersionResource{Group: "kaito.sh", Version: "v1alpha1", Resource: "modelmirrors"}

// modelMirrorPhaseReady is the ModelMirror status.phase once the weights are
// downloaded (or immediately, for a static mirror).
const modelMirrorPhaseReady = "Ready"

// ModelMirrorDownloader observes a model-streaming download through the
// status of the InferenceSet's ModelMirror.
type ModelMirrorDownloader struct {
	dynamic dynamic.Interface
}

// NewModelMirrorDownloader returns a downloader that reads ModelMirror status.
func NewModelMirrorDownloader(dyn dynamic.Interface) *ModelMirrorDownloader {
	return &ModelMirrorDownloader{dynamic: dyn}
}

// Observe reads the InferenceSet's ModelMirror.
//   - Completed: status.phase is Ready.
//   - Downloading: the download Job is reporting progress — status.phase is
//     Pending, status.failureMessage is empty, and status.download carries a
//     positive speedBytesPerSecond and a non-zero remainingSeconds.
//   - NotStarted: anything else, including a mirror that does not exist yet or
//     whose last download failed (the failure is surfaced through
//     InferenceSetReady).
//
// speedBytesPerSecond is KAITO's cumulative average since the download
// started.
func (d *ModelMirrorDownloader) Observe(ctx context.Context, is *unstructured.Unstructured) (DownloadStatus, error) {
	namespace := is.GetNamespace()
	name := ModelMirrorName(presetName(is), inferenceConfig(is))
	mm, err := d.dynamic.Resource(ModelMirrorGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return DownloadStatus{Phase: PhaseNotStarted}, nil
		}
		return DownloadStatus{}, fmt.Errorf("get ModelMirror %s/%s: %w", namespace, name, err)
	}

	phase, _, _ := unstructured.NestedString(mm.Object, "status", "phase")
	if phase == modelMirrorPhaseReady {
		return DownloadStatus{Phase: PhaseCompleted}, nil
	}
	failure, _, _ := unstructured.NestedString(mm.Object, "status", "failureMessage")
	speed, speedOK := nestedNumber(mm.Object, "status", "download", "speedBytesPerSecond")
	remaining, remainingOK := nestedNumber(mm.Object, "status", "download", "remainingSeconds")
	if phase != "Pending" || failure != "" || !speedOK || !remainingOK || speed <= 0 || remaining == 0 {
		return DownloadStatus{Phase: PhaseNotStarted}, nil
	}
	return DownloadStatus{
		Phase:          PhaseDownloading,
		BytesPerSecond: speed,
		Source:         fmt.Sprintf("modelmirror %s/%s", namespace, name),
	}, nil
}

// ModelMirrorName returns the name of the ModelMirror KAITO creates for a
// preset, exactly as KAITO derives it (modelstreaming.ModelMirrorKey): the
// first 6 hex characters of the SHA-256 of the resolved model ID, where the
// model ID is "custom-<config>" for the custom preset and otherwise the preset
// name mapped through KAITO's legacy short-name catalogue.
func ModelMirrorName(preset, config string) string {
	modelID := plugin.ResolveHFModelID(preset)
	if plugin.IsCustomPreset(preset) {
		modelID = plugin.CustomModelNamePrefix + config
	}
	sum := sha256.Sum256([]byte(modelID))
	return hex.EncodeToString(sum[:])[:6]
}

// inferenceConfig returns the InferenceSet's spec.template.inference.config.
func inferenceConfig(is *unstructured.Unstructured) string {
	cfg, _, _ := unstructured.NestedString(is.Object, "spec", "template", "inference", "config")
	return cfg
}

// nestedNumber reads an integer or floating-point field of an unstructured
// object.
func nestedNumber(obj map[string]interface{}, fields ...string) (float64, bool) {
	v, found, err := unstructured.NestedFieldNoCopy(obj, fields...)
	if err != nil || !found {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}
