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
	"strconv"
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

	// reporterSelector matches the reporter Deployment, wherever it is
	// installed (kaito-system for the E2E install, kube-system on AI Manager).
	reporterSelector = "app.kubernetes.io/name=productionstack-status-reporter"

	// reporterEmitSlack covers what DebouncedEmitTimeout does not model: the
	// scaled-down pod terminating before the Deployment counts as podless, the
	// evaluation pass itself, and the Event write.
	reporterEmitSlack = 90 * time.Second
)

// ReporterTimings are the running reporter's timing flags. Fields default to the
// chart values when the reporter runs without the flag.
type ReporterTimings struct {
	// Resync is how often every reason is re-evaluated (--resync-seconds).
	Resync time.Duration
	// StartupGrace is how long a finding without a backing timestamp, such as
	// a Deployment scaled to zero, must persist before it is published
	// (--startup-grace-seconds).
	StartupGrace time.Duration
	// EventRefresh is how often a still-active Warning is rewritten
	// (--event-refresh-seconds).
	EventRefresh time.Duration
}

// defaultReporterTimings are the productionstack-status-reporter chart defaults.
// The E2E install shortens StartupGrace to 30s and EventRefresh to 120s; other
// installs may keep these.
var defaultReporterTimings = ReporterTimings{
	Resync:       60 * time.Second,
	StartupGrace: 60 * time.Second,
	EventRefresh: 300 * time.Second,
}

// DebouncedEmitTimeout bounds how long the reporter takes to publish a finding
// it debounces by first observation, such as a Deployment scaled to zero. It
// first sees the problem on the next resync, withholds it until a later resync
// finds it has persisted for StartupGrace, and when StartupGrace is a multiple
// of Resync, tick jitter can push that to one resync later still.
func (t ReporterTimings) DebouncedEmitTimeout() time.Duration {
	graceTicks := (t.StartupGrace + t.Resync - 1) / t.Resync
	return t.Resync + graceTicks*t.Resync + t.Resync + reporterEmitSlack
}

// GetReporterTimings reads the timing flags of the running reporter Deployment.
func GetReporterTimings(ctx context.Context) (ReporterTimings, error) {
	cs, err := GetK8sClientset()
	if err != nil {
		return ReporterTimings{}, err
	}
	list, err := cs.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: reporterSelector})
	if err != nil {
		return ReporterTimings{}, fmt.Errorf("list reporter Deployments: %w", err)
	}
	if len(list.Items) != 1 {
		return ReporterTimings{}, fmt.Errorf("expected one Deployment matching %q, found %d", reporterSelector, len(list.Items))
	}
	var args []string
	for _, c := range list.Items[0].Spec.Template.Spec.Containers {
		args = append(args, c.Args...)
	}
	return reporterTimingsFromArgs(args)
}

func reporterTimingsFromArgs(args []string) (ReporterTimings, error) {
	timings := defaultReporterTimings
	flags := map[string]*time.Duration{
		"--resync-seconds=":        &timings.Resync,
		"--startup-grace-seconds=": &timings.StartupGrace,
		"--event-refresh-seconds=": &timings.EventRefresh,
	}
	for _, arg := range args {
		for prefix, field := range flags {
			value, ok := strings.CutPrefix(arg, prefix)
			if !ok {
				continue
			}
			seconds, err := strconv.Atoi(value)
			// The reporter accepts a zero startup grace; the other two must be positive.
			if err != nil || seconds < 0 || (seconds == 0 && prefix != "--startup-grace-seconds=") {
				return ReporterTimings{}, fmt.Errorf("invalid reporter flag %q", arg)
			}
			*field = time.Duration(seconds) * time.Second
		}
	}
	return timings, nil
}

// ReporterEventRefreshInterval returns how often the running reporter rewrites
// a still-active Warning (--event-refresh-seconds). The E2E install shortens it
// to 120s, while other installs may keep the chart default.
func ReporterEventRefreshInterval(ctx context.Context) (time.Duration, error) {
	timings, err := GetReporterTimings(ctx)
	if err != nil {
		return 0, err
	}
	return timings.EventRefresh, nil
}

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

// ReporterEventsForObjectSince returns the reporter Events whose involvedObject
// name is objectName and whose LastTimestamp is after since.
func ReporterEventsForObjectSince(ctx context.Context, objectName string, since metav1.Time) ([]corev1.Event, error) {
	events, err := ListReporterEvents(ctx)
	if err != nil {
		return nil, err
	}
	var out []corev1.Event
	for _, ev := range events {
		if ev.InvolvedObject.Name == objectName && ev.LastTimestamp.After(since.Time) {
			out = append(out, ev)
		}
	}
	return out, nil
}
