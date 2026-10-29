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

package emit

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/config"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/evaluator"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/reason"
)

// An active finding creates its Event once, is refreshed no more often than
// the refresh interval, and is no longer written once it is not passed in
// (recovery) — §1.1.
func TestEmitterRefreshesActiveEventAtInterval(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	e := NewEmitter(cs, clock, 0, 5*time.Minute, "test")
	f := evaluator.Finding{
		Reason:   reason.ClusterBBRNotReady,
		Object:   evaluator.InvolvedObject{Kind: evaluator.KindNamespace, Name: "kaito-system"},
		GroupKey: "cluster/kaito-system",
		Message:  "first",
	}
	get := func() (int32, time.Time, string) {
		t.Helper()
		ev, err := cs.CoreV1().Events(config.ReportingNamespace).Get(ctx, NameForLookup(f).Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get event: %v", err)
		}
		return ev.Count, ev.LastTimestamp.Time, ev.Message
	}

	e.Emit(ctx, []evaluator.Finding{f})
	if count, last, _ := get(); count != 1 || !last.Equal(now) {
		t.Fatalf("after create: count=%d last=%v, want 1 / %v", count, last, now)
	}

	// Within the refresh interval the Event is not rewritten, even with a new
	// message.
	now = now.Add(4 * time.Minute)
	f.Message = "second"
	e.Emit(ctx, []evaluator.Finding{f})
	if count, _, msg := get(); count != 1 || msg != "first" {
		t.Fatalf("within interval: count=%d msg=%q, want 1 / first", count, msg)
	}

	// Once the interval has elapsed it is refreshed.
	now = now.Add(time.Minute)
	e.Emit(ctx, []evaluator.Finding{f})
	if count, last, msg := get(); count != 2 || !last.Equal(now) || msg != "second" {
		t.Fatalf("after interval: count=%d last=%v msg=%q, want 2 / %v / second", count, last, msg, now)
	}

	// Recovery: the finding is gone, so nothing is written any more.
	now = now.Add(time.Hour)
	e.Emit(ctx, nil)
	if count, _, _ := get(); count != 2 {
		t.Fatalf("after recovery: count=%d, want 2", count)
	}
}
