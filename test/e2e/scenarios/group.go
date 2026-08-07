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

import "context"

// GroupResources names the physical (already-suffixed) resources one
// scenario group owns. Cleanup uses it to decide what to delete (and in
// what order); Diagnostics uses it to decide what to inspect on failure.
type GroupResources struct {
	// Namespaces are every namespace the group provisioned.
	Namespaces []string
	// Deployments are every ModelDeployment the group provisioned.
	// Namespace/Name must already be physical (suffixed) names.
	Deployments []DeploymentSpec
}

// Diagnostics performs bounded, best-effort structured collection (pod
// state, recent logs, ...) after a failed assertion, to help postmortem
// debugging. Implementations MUST NOT panic, MUST bound how much data
// they collect/log (a runaway dump is worse than none), and MUST treat
// their own collection failures as non-fatal (log and move on) — a
// diagnostics failure must never mask or replace the assertion failure
// that triggered it.
type Diagnostics interface {
	Collect(ctx context.Context, group GroupResources, log Logger)
}

// NoopDiagnostics collects nothing. Useful as a safe default for callers
// (and most unit tests) that don't need failure diagnostics.
type NoopDiagnostics struct{}

// Collect implements Diagnostics.
func (NoopDiagnostics) Collect(context.Context, GroupResources, Logger) {}

// Cleanup tears down a scenario group's resources once every assertion
// has run.
//
//   - success == true: ModelDeployments are deleted before their
//     Namespaces (mirrors production teardown ordering — a namespace
//     delete cascades the Gateway/HTTPRoute anyway, but deleting the
//     workload first keeps Helm release state consistent). Deletion
//     errors are logged (best-effort) rather than returned, matching
//     existing UninstallCase behavior — a cleanup hiccup must not turn a
//     green run red.
//   - success == false: children are deliberately left in place for
//     postmortem inspection. Cleanup instead invokes diag.Collect
//     (bounded, best-effort) and returns primaryFailure unchanged, so the
//     original assertion failure is never masked by a cleanup or
//     diagnostics error. The top-level caller is expected to remove the
//     whole cluster later, which reclaims these leftovers.
//
// diag may be nil, in which case no diagnostics are collected on failure.
func Cleanup(
	ctx context.Context,
	lc Lifecycle,
	log Logger,
	group GroupResources,
	success bool,
	primaryFailure error,
	diag Diagnostics,
) error {
	if !success {
		if diag != nil {
			diag.Collect(ctx, group, log)
		}
		return primaryFailure
	}

	for _, d := range group.Deployments {
		if err := lc.DeleteModelDeployment(ctx, d.Namespace, d.Name); err != nil {
			log.Logf("cleanup: delete deployment %s/%s: %v", d.Namespace, d.Name, err)
		}
	}
	for _, ns := range group.Namespaces {
		if err := lc.DeleteNamespace(ctx, ns); err != nil {
			log.Logf("cleanup: delete namespace %s: %v", ns, err)
		}
	}
	return primaryFailure
}
