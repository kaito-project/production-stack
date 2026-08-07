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

package scenarios

import (
	"context"
	"errors"
	"testing"
)

func testGroup() GroupResources {
	return GroupResources{
		Namespaces: []string{"e2e-auth"},
		Deployments: []DeploymentSpec{
			{Namespace: "e2e-auth", Name: "auth-gemma"},
		},
	}
}

func TestCleanup_SuccessDeletesDeploymentsBeforeNamespaces(t *testing.T) {
	lc := newFakeLifecycle()
	log := &fakeLogger{}

	err := Cleanup(context.Background(), lc, log, testGroup(), true /* success */, nil, nil)
	if err != nil {
		t.Fatalf("Cleanup() = %v, want nil", err)
	}

	calls := lc.callLog()
	if len(calls) != 2 {
		t.Fatalf("expected exactly 2 lifecycle calls, got %v", calls)
	}
	if calls[0] != "DeleteModelDeployment(e2e-auth/auth-gemma)" {
		t.Errorf("call[0] = %q, want the deployment deleted first", calls[0])
	}
	if calls[1] != "DeleteNamespace(e2e-auth)" {
		t.Errorf("call[1] = %q, want the namespace deleted second", calls[1])
	}
}

func TestCleanup_SuccessLogsButSwallowsDeleteErrors(t *testing.T) {
	lc := newFakeLifecycle()
	lc.deleteDeploymentErr = errors.New("deployment delete failed")
	lc.deleteNamespaceErr = errors.New("namespace delete failed")
	log := &fakeLogger{}

	err := Cleanup(context.Background(), lc, log, testGroup(), true, nil, nil)
	if err != nil {
		t.Fatalf("Cleanup() = %v, want nil (delete errors are logged, not returned)", err)
	}
	if len(log.all()) != 2 {
		t.Errorf("expected 2 warning log lines, got %v", log.all())
	}
}

// fakeDiagnostics records whether Collect was invoked and what group it
// was given.
type fakeDiagnostics struct {
	collected bool
	group     GroupResources
}

func (f *fakeDiagnostics) Collect(_ context.Context, group GroupResources, log Logger) {
	f.collected = true
	f.group = group
	log.Logf("diagnostics collected for namespaces=%v", group.Namespaces)
}

func TestCleanup_FailureDoesNotDeleteChildrenAndCollectsDiagnostics(t *testing.T) {
	lc := newFakeLifecycle()
	log := &fakeLogger{}
	diag := &fakeDiagnostics{}
	primaryFailure := errors.New("assertion X failed")

	group := testGroup()
	err := Cleanup(context.Background(), lc, log, group, false /* success */, primaryFailure, diag)

	if !errors.Is(err, primaryFailure) {
		t.Fatalf("Cleanup() = %v, want the primary failure returned unchanged", err)
	}
	if calls := lc.callLog(); len(calls) != 0 {
		t.Errorf("expected no delete calls on failure, got %v", calls)
	}
	if !diag.collected {
		t.Error("expected Diagnostics.Collect to be invoked on failure")
	}
	if len(diag.group.Namespaces) != 1 || diag.group.Namespaces[0] != "e2e-auth" {
		t.Errorf("diagnostics received unexpected group: %+v", diag.group)
	}
	if len(log.all()) == 0 {
		t.Error("expected diagnostics to log at least one line via the injected Logger")
	}
}

func TestCleanup_FailureWithNilDiagnosticsStillReturnsPrimaryFailure(t *testing.T) {
	lc := newFakeLifecycle()
	log := &fakeLogger{}
	primaryFailure := errors.New("assertion Y failed")

	err := Cleanup(context.Background(), lc, log, testGroup(), false, primaryFailure, nil)

	if !errors.Is(err, primaryFailure) {
		t.Fatalf("Cleanup() = %v, want the primary failure", err)
	}
	if calls := lc.callLog(); len(calls) != 0 {
		t.Errorf("expected no delete calls, got %v", calls)
	}
}
