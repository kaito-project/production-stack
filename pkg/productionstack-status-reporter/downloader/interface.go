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

// Package downloader observes the model-weights download of an InferenceSet
// for the inferencesetModelDownloadSlow reason (§1.4). KAITO obtains model
// weights on one of three paths — vLLM self-download inside the model pod, a
// ModelMirror, or local weights on the node — so each download path implements
// WeightDownloader, and Select picks the one KAITO uses for an InferenceSet.
package downloader

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Phase is the model-download state of one InferenceSet, as judged by the
// downloader that observes it.
type Phase string

const (
	// PhaseNotStarted means no download progress is observable yet (or the last
	// download attempt failed); the caller adds no throughput sample.
	PhaseNotStarted Phase = "NotStarted"
	// PhaseDownloading means the download is in progress and BytesPerSecond is a
	// valid throughput sample.
	PhaseDownloading Phase = "Downloading"
	// PhaseCompleted means the model weights are downloaded; the caller clears
	// its window so the finding becomes inactive.
	PhaseCompleted Phase = "Completed"
)

// DownloadStatus is the observed model-download state of one InferenceSet.
type DownloadStatus struct {
	// Phase is decided by the downloader.
	Phase Phase
	// BytesPerSecond is valid only when Phase is PhaseDownloading; it is the
	// throughput of the slowest active download source.
	BytesPerSecond float64
	// Source names the slowest active download source, e.g.
	// "model-pod <pod>" or "modelmirror <namespace>/<name>".
	Source string
}

// WeightDownloader observes the model-weights download of an InferenceSet on
// one download path.
type WeightDownloader interface {
	// Observe reports the current model-download state of one InferenceSet.
	Observe(ctx context.Context, is *unstructured.Unstructured) (DownloadStatus, error)
}
