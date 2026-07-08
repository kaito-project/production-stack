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
// (suppression, startup gating, recovery) is decided by the per-pass Emitter;
// the writer below only knows how to persist a single Event to the API.
package emit

import (
	"context"
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

// eventName derives a stable, unique-per-(reason,object) Event name so repeats
// land on the same object and get aggregated.
func eventName(f evaluator.Finding) string {
	base := fmt.Sprintf("%s.%s.%s", strings.ToLower(string(f.Object.Kind)), f.Object.Name, f.Reason)
	// Namespaces / reasons are DNS-safe; lower-case to satisfy name rules.
	return strings.ToLower(base)
}

// write creates or aggregates the Event for the finding. message overrides
// f.Message when non-empty (used to append the §1.4 transparency suffix).
func (w *writer) write(ctx context.Context, f evaluator.Finding, message string) error {
	if message == "" {
		message = f.Message
	}
	name := eventName(f)
	ref := objectReference(f.Object)
	now := metav1.Now()
	eventType := reason.EventType(f.Reason)

	existing, err := w.clientset.CoreV1().Events(config.ReportingNamespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		// Aggregate: bump count + lastTimestamp; refresh message in case the
		// suppression suffix changed.
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
// need to locate the emitted Event by (reason, object).
func NameForLookup(r reason.Reason, obj evaluator.InvolvedObject) types.NamespacedName {
	return types.NamespacedName{
		Namespace: config.ReportingNamespace,
		Name:      eventName(evaluator.Finding{Reason: r, Object: obj}),
	}
}
