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

// Package emit is the low-level control-plane Event publisher. It enforces the
// §1.1 schema (cluster-scoped involvedObject, source.component =
// productionstack-status-reporter, Warning/Normal type) and implements the
// standard Event aggregation behaviour by hand: a repeat of the same
// (reason, involvedObject, message) bumps count + lastTimestamp on the existing
// Event instead of creating a new one. The "what to publish and when"
// (startup gating, recovery) is decided by the per-pass Emitter;
// the writer below only knows how to persist a single Event to the API.
package emit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/config"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
)

// writer persists control-plane Events to kube-system. It is stateless beyond
// the clientset, so the in-memory transition state used by the reporter lives
// on the per-pass Emitter, not here.
type writer struct {
	clientset kubernetes.Interface
}

// newWriter constructs a writer that persists Events via the supplied clientset.
func newWriter(cs kubernetes.Interface) *writer {
	return &writer{clientset: cs}
}

// objectReference builds the cluster-scoped involvedObject reference. CRDs and
// Namespaces are both cluster-scoped, so the Event's metadata.namespace stays
// kube-system without violating the cross-namespace Event validation (§1.1).
func objectReference(obj evaluator.InvolvedObject) corev1.ObjectReference {
	switch obj.Kind {
	case evaluator.KindCRD:
		return corev1.ObjectReference{
			APIVersion: "apiextensions.k8s.io/v1",
			Kind:       "CustomResourceDefinition",
			Name:       obj.Name,
		}
	default:
		return corev1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Namespace",
			Name:       obj.Name,
		}
	}
}

// eventName derives a stable, unique-per-(subject,reason) Event name so repeats
// land on the same Event and get aggregated. The subject is the finding's
// GroupKey (unique per InferenceSet / namespace / cluster reason): because the
// involvedObject is always a cluster-scoped Namespace (§1.1), two subjects in the
// same namespace with the same reason would otherwise collapse onto one Event,
// so the GroupKey — not just the involvedObject — is part of the name.
func eventName(f evaluator.Finding) string {
	subject := f.GroupKey
	if subject == "" {
		subject = string(f.Object.Kind) + "." + f.Object.Name
	}
	return sanitizeEventName(subject + "." + string(f.Reason))
}

// sanitizeEventName maps an arbitrary identity string to a deterministic,
// RFC 1123-valid Event name (lowercase [a-z0-9.-], bounded to 253 chars). Any
// disallowed character (e.g. the '/' separators in a GroupKey) becomes '-'; an
// over-long name is truncated and suffixed with a hash to keep it unique.
func sanitizeEventName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), ".-")
	if name == "" {
		name = "event"
	}
	if len(name) > 253 {
		sum := sha256.Sum256([]byte(s))
		name = name[:253-17] + "-" + hex.EncodeToString(sum[:8])
	}
	return name
}

// write creates or aggregates the Event for the finding.
func (w *writer) write(ctx context.Context, f evaluator.Finding) error {
	message := f.Message
	name := eventName(f)
	ref := objectReference(f.Object)
	now := metav1.Now()
	eventType := reason.EventType(f.Reason)

	existing, err := w.clientset.CoreV1().Events(config.ReportingNamespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		// Aggregate: bump count + lastTimestamp; refresh message in case it
		// changed.
		patch := existing.DeepCopy()
		patch.Count++
		patch.LastTimestamp = now
		patch.Message = message
		patch.Type = eventType
		if _, uerr := w.clientset.CoreV1().Events(config.ReportingNamespace).Update(ctx, patch, metav1.UpdateOptions{}); uerr != nil {
			return fmt.Errorf("update event %s: %w", name, uerr)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get event %s: %w", name, err)
	}

	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: config.ReportingNamespace,
		},
		InvolvedObject: ref,
		Reason:         string(f.Reason),
		Message:        message,
		Type:           eventType,
		// EventTime must be set so the apiserver applies the "new" Event
		// validation branch: only then is a cluster-scoped involvedObject
		// (empty namespace) allowed to be reported from kube-system. Without
		// it, legacyValidateEvent requires the Event namespace to be "" or
		// "default" and rejects kube-system with "involvedObject.namespace:
		// does not match event.namespace".
		EventTime:           metav1.NewMicroTime(now.Time),
		FirstTimestamp:      now,
		LastTimestamp:       now,
		Count:               1,
		Source:              corev1.EventSource{Component: config.ReporterComponent},
		ReportingController: config.ReporterComponent,
		ReportingInstance:   config.ReporterComponent,
		Action:              "Evaluate",
	}
	if _, cerr := w.clientset.CoreV1().Events(config.ReportingNamespace).Create(ctx, event, metav1.CreateOptions{}); cerr != nil {
		if apierrors.IsAlreadyExists(cerr) {
			// Lost a race with another reconcile; fall back to an aggregate.
			return w.aggregateExisting(ctx, name, message, eventType)
		}
		return fmt.Errorf("create event %s: %w", name, cerr)
	}
	return nil
}

func (w *writer) aggregateExisting(ctx context.Context, name, message, eventType string) error {
	existing, err := w.clientset.CoreV1().Events(config.ReportingNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get event %s after AlreadyExists: %w", name, err)
	}
	existing.Count++
	existing.LastTimestamp = metav1.Now()
	existing.Message = message
	existing.Type = eventType
	if _, uerr := w.clientset.CoreV1().Events(config.ReportingNamespace).Update(ctx, existing, metav1.UpdateOptions{}); uerr != nil {
		return fmt.Errorf("update event %s after AlreadyExists: %w", name, uerr)
	}
	return nil
}

// NameForLookup exposes the deterministic Event name for tests / callers that
// need to locate the emitted Event for a Finding (keyed by its GroupKey + reason).
func NameForLookup(f evaluator.Finding) types.NamespacedName {
	return types.NamespacedName{
		Namespace: config.ReportingNamespace,
		Name:      eventName(f),
	}
}
