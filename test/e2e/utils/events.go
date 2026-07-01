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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ReporterEventNamespace is where the productionstack-status-reporter
	// publishes all of its control-plane status Events.
	ReporterEventNamespace = "kube-system"

	// ReporterEventSource is the Event source.component the reporter stamps;
	// it is the field-selector used to scope all reporter-event assertions.
	ReporterEventSource = "productionstack-status-reporter"
)

// ListReporterEvents returns every Event in kube-system emitted by the
// productionstack-status-reporter (source.component selector), newest first
// by lastTimestamp.
func ListReporterEvents(ctx context.Context) ([]corev1.Event, error) {
	cs, err := GetK8sClientset()
	if err != nil {
		return nil, err
	}
	list, err := cs.CoreV1().Events(ReporterEventNamespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("source=%s", ReporterEventSource),
	})
	if err != nil {
		return nil, fmt.Errorf("list reporter events: %w", err)
	}
	return list.Items, nil
}

// FindReporterEvent returns the most recent reporter Event with the given
// reason whose involvedObject name matches objectName (pass "" to match any
// object). found=false when none matches.
func FindReporterEvent(ctx context.Context, reason, objectName string) (corev1.Event, bool, error) {
	events, err := ListReporterEvents(ctx)
	if err != nil {
		return corev1.Event{}, false, err
	}
	var best corev1.Event
	found := false
	for i := range events {
		ev := events[i]
		if ev.Reason != reason {
			continue
		}
		if objectName != "" && ev.InvolvedObject.Name != objectName {
			continue
		}
		if !found || ev.LastTimestamp.After(best.LastTimestamp.Time) {
			best = ev
			found = true
		}
	}
	return best, found, nil
}

// WaitForReporterEvent blocks until a reporter Event with the given reason
// (and optional involvedObject name) is present, returning it. Use to assert
// a Warning/Normal reason appears within one or more reporter resyncs.
func WaitForReporterEvent(ctx context.Context, reason, objectName string, timeout time.Duration) (corev1.Event, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ev, found, err := FindReporterEvent(ctx, reason, objectName)
		if err == nil && found {
			return ev, nil
		}
		time.Sleep(PollInterval)
	}
	return corev1.Event{}, fmt.Errorf("timed out waiting for reporter event reason=%s object=%q", reason, objectName)
}

// WaitForReporterEventSince blocks until a reporter Event with the given reason
// (and optional involvedObject name) whose LastTimestamp is strictly after
// `since` is present, returning it.
//
// Reporter Events are deterministic, persistent objects: the reporter
// re-emits a reason by aggregating onto the same named Event (bumping Count +
// LastTimestamp) rather than creating a fresh one, and a cleared reason's
// Event object lingers until GC. A plain reason match therefore cannot tell a
// freshly-emitted Event from a stale one left by an earlier perturbation or a
// previous suite run. Capture `since` from a FindReporterEvent baseline taken
// *before* the perturbation so the assertion only passes on a genuinely new
// emission. Pass a zero `since` (metav1.Time{}) to match any event.
//
// Both `since` and the Event's LastTimestamp are stamped by the reporter, so
// this comparison is robust to clock skew between the test runner and the
// cluster.
func WaitForReporterEventSince(ctx context.Context, reason, objectName string, since metav1.Time, timeout time.Duration) (corev1.Event, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ev, found, err := FindReporterEvent(ctx, reason, objectName)
		if err == nil && found && ev.LastTimestamp.After(since.Time) {
			return ev, nil
		}
		time.Sleep(PollInterval)
	}
	return corev1.Event{}, fmt.Errorf("timed out waiting for reporter event reason=%s object=%q newer than %s",
		reason, objectName, since)
}

// ReporterEventBaseline returns the LastTimestamp of the most recent reporter
// Event with the given reason (and optional involvedObject name), or the zero
// time when none exists yet. Pair it with WaitForReporterEventSince to assert
// a genuinely new emission rather than matching a stale Event.
func ReporterEventBaseline(ctx context.Context, reason, objectName string) (metav1.Time, error) {
	ev, found, err := FindReporterEvent(ctx, reason, objectName)
	if err != nil || !found {
		return metav1.Time{}, err
	}
	return ev.LastTimestamp, nil
}

// EnsureNoReporterEvent verifies that NO reporter Event with the given reason
// (and optional involvedObject name) appears for the whole quiet window. Use
// to assert suppression / windowed-evaluation negatives.
func EnsureNoReporterEvent(ctx context.Context, reason, objectName string, quiet time.Duration) error {
	deadline := time.Now().Add(quiet)
	for time.Now().Before(deadline) {
		ev, found, err := FindReporterEvent(ctx, reason, objectName)
		if err == nil && found {
			return fmt.Errorf("unexpected reporter event reason=%s object=%q message=%q",
				reason, objectName, ev.Message)
		}
		time.Sleep(PollInterval)
	}
	return nil
}

// EnsureNoReporterEventSince verifies that NO reporter Event with the given
// reason (and optional involvedObject name) is emitted newer than `since` for
// the whole quiet window. Use to assert that a Warning stops firing after the
// underlying problem is fixed, now that the reporter publishes no positive
// recovery event.
func EnsureNoReporterEventSince(ctx context.Context, reason, objectName string, since metav1.Time, quiet time.Duration) error {
	deadline := time.Now().Add(quiet)
	for time.Now().Before(deadline) {
		ev, found, err := FindReporterEvent(ctx, reason, objectName)
		if err == nil && found && ev.LastTimestamp.After(since.Time) {
			return fmt.Errorf("unexpected fresh reporter event reason=%s object=%q message=%q",
				reason, objectName, ev.Message)
		}
		time.Sleep(PollInterval)
	}
	return nil
}

// ReporterEventMessageContains reports whether the latest reporter Event with
// the given reason has a message containing substr.
func ReporterEventMessageContains(ctx context.Context, reason, substr string) (bool, error) {
	ev, found, err := FindReporterEvent(ctx, reason, "")
	if err != nil || !found {
		return false, err
	}
	return strings.Contains(ev.Message, substr), nil
}
