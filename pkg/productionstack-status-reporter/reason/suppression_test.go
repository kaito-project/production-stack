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

package reason

import (
	"strings"
	"testing"
)

func TestSuppressedBy(t *testing.T) {
	got := SuppressedBy(ClusterIstioControlPlaneNotReady)
	if len(got) != 2 {
		t.Fatalf("expected 2 suppressed reasons, got %d: %v", len(got), got)
	}

	if SuppressedBy(ClusterBBRNotReady) != nil {
		t.Fatalf("clusterBBRNotReady must not suppress anything")
	}
	if SuppressedBy(ClusterKedaNotReady) != nil {
		t.Fatalf("clusterKedaNotReady must not suppress anything")
	}
}

func TestSuppressedByCRD(t *testing.T) {
	got := SuppressedByCRD("inferencesets.kaito.sh")
	if len(got) != 5 {
		t.Fatalf("expected 5 suppressed reasons for inferencesets CRD, got %d: %v", len(got), got)
	}
	if SuppressedByCRD("does.not.exist") != nil {
		t.Fatalf("unknown CRD must suppress nothing")
	}
}

func TestHasSuppressionRow(t *testing.T) {
	if !HasSuppressionRow(ClusterCRDMissing) {
		t.Fatalf("clusterCRDMissing must have a suppression row")
	}
	if !HasSuppressionRow(ClusterGatewayAuthNotReady) {
		t.Fatalf("clusterGatewayAuthNotReady must have a suppression row")
	}
	if HasSuppressionRow(ClusterBBRNotReady) {
		t.Fatalf("clusterBBRNotReady must NOT have a suppression row")
	}
}

func TestTransparencySuffixSortedAndStable(t *testing.T) {
	suppressed := map[Reason]bool{
		ModelharnessGatewayProgrammingFailed: true,
		ModelharnessGatewayClassMissing:      true,
	}
	got := TransparencySuffix(suppressed, 3)
	// Names must be sorted lexicographically and the namespace count present.
	want := " (suppressing downstream reasons: modelharnessGatewayClassMissing, modelharnessGatewayProgrammingFailed in 3 namespace(s))"
	if got != want {
		t.Fatalf("suffix mismatch:\n got: %q\nwant: %q", got, want)
	}
	if !strings.Contains(got, "3 namespace(s)") {
		t.Fatalf("suffix must include namespace count")
	}
}

func TestTransparencySuffixEmpty(t *testing.T) {
	if got := TransparencySuffix(map[Reason]bool{}, 5); got != "" {
		t.Fatalf("expected empty suffix when nothing suppressed, got %q", got)
	}
	if got := TransparencySuffix(map[Reason]bool{InferencesetModelPodsNotReady: true}, 0); got != "" {
		t.Fatalf("expected empty suffix when nsCount==0, got %q", got)
	}
}
