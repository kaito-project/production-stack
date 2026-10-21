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
	"errors"
	"strings"
	"testing"
	"time"
)

// A check interrupted by the deadline must not hide what the previous checks
// reported as not ready.
func TestPollUntilReadyKeepsLastDiagnosticOnTimeout(t *testing.T) {
	interval := PollInterval
	PollInterval = 20 * time.Millisecond
	t.Cleanup(func() { PollInterval = interval })

	calls := 0
	err := pollUntilReady(context.Background(), 100*time.Millisecond, "thing", func(ctx context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("inference pod is Pending")
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "last check: inference pod is Pending") {
		t.Fatalf("timeout error lost the last diagnostic: %v", err)
	}
}

func TestPollUntilReadyReportsDeadlineWithoutEarlierDiagnostic(t *testing.T) {
	interval := PollInterval
	PollInterval = 20 * time.Millisecond
	t.Cleanup(func() { PollInterval = interval })

	err := pollUntilReady(context.Background(), 50*time.Millisecond, "thing", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if err == nil || !strings.Contains(err.Error(), "last check: context deadline exceeded") {
		t.Fatalf("expected the deadline as the last check, got %v", err)
	}
}
