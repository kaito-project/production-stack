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

// Package scenarios is an ordinary-Go (no Ginkgo/Gomega) library of
// reusable end-to-end scenario logic for four Smoke-labeled test groups:
//
//   - GPU / framework smoke  (gpu_smoke.go)
//   - API-key authentication (apikey_auth.go)
//   - Filter execution order (filter_order.go)
//   - BBR cluster-filter HA  (cluster_filter_ha.go)
//
// Every file in this package is plain Go: it depends only on the standard
// library and Kubernetes API types, never on github.com/onsi/ginkgo or
// github.com/onsi/gomega. That makes the package importable from any Go
// test harness — Ginkgo `It`s in test/e2e (this repo, Helm-backed), or a
// downstream repo's own suite (e.g. an AIManager-backed harness) — without
// pulling in a BDD framework.
//
// Each scenario group follows the same three-phase shape:
//
//  1. Setup   — provisions (or reuses) the namespace(s) and
//     ModelDeployment(s) the group needs, through the injected Lifecycle,
//     and resolves per-namespace dataplane access (gateway URL + optional
//     API key) via NamespaceAccess.
//  2. Assertions — a list of independently-named checks (see Assertion),
//     each owning its own internal polling (Eventually-style retries
//     live *inside* one assertion; there is deliberately no whole-case
//     retry wrapper around the group). Callers that want per-check
//     reporting (e.g. individual Ginkgo `It`s) can invoke each Assertion's
//     Run function separately; callers that just want a single result can
//     run them in sequence and stop at the first failure.
//  3. Cleanup — deletes the group's ModelDeployments before its
//     Namespaces on success. On failure, Cleanup deliberately leaves the
//     children in place, invokes the injected (bounded, best-effort)
//     Diagnostics hook, and returns the original failure unchanged so a
//     human (or the top-level caller, which later removes the whole
//     cluster) can inspect what went wrong.
//
// Every physical Namespace and ModelDeployment name this package creates
// is resolved through Suffix, so a caller-provided suffix is applied
// consistently across an entire scenario run (see names.go).
package scenarios
