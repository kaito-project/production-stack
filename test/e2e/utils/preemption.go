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
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	g "github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// systemClusterCritical is the class the charts set. Only a
// system-node-critical pod outranks it: user-defined classes stop at one
// billion.
const systemClusterCritical = "system-cluster-critical"

// preemptionKnownGaps are the pods, by name prefix, whose charts still set no
// PriorityClass, so any higher-priority pod can preempt them. Remove an entry
// once the pinned chart sets one.
var preemptionKnownGaps = []struct{ podPrefix, reason string }{
	{"apikey-authz-", "the pinned llm-gateway-auth chart sets no PriorityClass"},
	{"apikey-operator-", "the pinned llm-gateway-auth chart sets no PriorityClass"},
	{"azure-authz-", "the pinned llm-gateway-auth chart sets no PriorityClass"},
	{"csi-local-manager-", "the local-csi-driver chart sets no PriorityClass"},
}

// FailSuiteOnPreemption fails the suite when the scheduler preempts a pod while
// the suite runs. A suite enables it once, at the top level:
//
//	var _ = utils.FailSuiteOnPreemption()
//
// Two kinds of preemption, which the charts cannot prevent, are only recorded
// as report entries: of a pod at system-cluster-critical, which only a
// system-node-critical pod can preempt, and of a pod in preemptionKnownGaps.
//
// The check watches pods for the DisruptionTarget condition the scheduler sets
// on a victim before deleting it, not the Preempted Events: an Event does not
// carry the victim's priority, and the API server deletes Events after an hour
// while a run can take longer. Both nodes run once, on the first parallel
// process.
func FailSuiteOnPreemption() bool {
	var watcher *preemptionWatcher
	g.ReportBeforeSuite(func(ctx g.SpecContext, report g.Report) {
		if report.SuiteConfig.DryRun || report.PreRunStats.SpecsThatWillRun == 0 {
			return
		}
		clientset, err := GetK8sClientset()
		if err != nil {
			g.Fail(fmt.Sprintf("create the clientset for the preemption check: %v", err))
		}
		if watcher, err = watchPreemptions(ctx, clientset); err != nil {
			g.Fail(fmt.Sprintf("watch pods for preemption: %v", err))
		}
	}, g.NodeTimeout(2*time.Minute))
	g.ReportAfterSuite("no pod preempted", func(g.Report) {
		if watcher == nil {
			return
		}
		var preempted []string
		for _, p := range watcher.stop() {
			if reason := p.toleratedBecause(); reason != "" {
				g.AddReportEntry("tolerated preemption", fmt.Sprintf("%s (tolerated: %s)", p, reason))
				continue
			}
			preempted = append(preempted, p.String())
		}
		if len(preempted) > 0 {
			g.Fail(fmt.Sprintf("pods preempted while the suite ran (%d):\n%s",
				len(preempted), strings.Join(preempted, "\n")))
		}
	})
	return true
}

// podPreemption is one pod the scheduler preempted.
type podPreemption struct {
	pod           types.NamespacedName
	priorityClass string
	node          string
	at            time.Time
}

// toleratedBecause returns why the suite tolerates the preemption, or "" if it
// fails the suite.
func (p podPreemption) toleratedBecause() string {
	if p.priorityClass == systemClusterCritical {
		return "only a system-node-critical pod can preempt it"
	}
	for _, gap := range preemptionKnownGaps {
		if strings.HasPrefix(p.pod.Name, gap.podPrefix) {
			return gap.reason
		}
	}
	return ""
}

func (p podPreemption) String() string {
	return fmt.Sprintf("%s %s (%s) on node %s", p.at.UTC().Format(time.RFC3339), p.pod,
		cmp.Or(p.priorityClass, "no PriorityClass"), p.node)
}

// preemptionWatcher records the pods the scheduler preempts after it started.
type preemptionWatcher struct {
	factory informers.SharedInformerFactory
	stopCh  chan struct{}

	mu sync.Mutex
	// seen holds every preempted pod, including those preempted before the
	// watch started, so each is handled once.
	seen        map[types.UID]bool
	preemptions []podPreemption
}

func watchPreemptions(ctx context.Context, clientset kubernetes.Interface) (*preemptionWatcher, error) {
	w := &preemptionWatcher{
		factory: informers.NewSharedInformerFactory(clientset, 0),
		stopCh:  make(chan struct{}),
		seen:    map[types.UID]bool{},
	}
	informer := w.factory.Core().V1().Pods().Informer()
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerDetailedFuncs{
		AddFunc:    w.record,
		UpdateFunc: func(_, obj any) { w.record(obj, false) },
		DeleteFunc: func(obj any) { w.record(obj, false) },
	}); err != nil {
		return nil, err
	}
	w.factory.Start(w.stopCh)
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		w.stop()
		return nil, fmt.Errorf("list pods: %w", ctx.Err())
	}
	return w, nil
}

// record records a pod the scheduler preempted. A pod already preempted in the
// initial list was preempted before the suite started and is skipped.
func (w *preemptionWatcher) record(obj any, isInInitialList bool) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	i := slices.IndexFunc(pod.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.DisruptionTarget && c.Status == corev1.ConditionTrue &&
			c.Reason == corev1.PodReasonPreemptionByScheduler
	})
	if i < 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen[pod.UID] {
		return
	}
	w.seen[pod.UID] = true
	if isInInitialList {
		return
	}
	w.preemptions = append(w.preemptions, podPreemption{
		pod:           types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name},
		priorityClass: pod.Spec.PriorityClassName,
		node:          pod.Spec.NodeName,
		at:            pod.Status.Conditions[i].LastTransitionTime.Time,
	})
}

// stop ends the watch and returns the preemptions it recorded.
func (w *preemptionWatcher) stop() []podPreemption {
	close(w.stopCh)
	w.factory.Shutdown()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.preemptions
}
