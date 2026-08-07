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

// Suffix is applied consistently to every physical Namespace and
// ModelDeployment name a scenario group creates, so that two concurrent
// runs of the same group (parallel Ginkgo workers, repeated AIManager
// invocations, ...) never collide on cluster-wide resource names.
//
// An empty Suffix leaves names unchanged, which matches today's Helm
// suite behavior (e2e-auth, auth-gemma, ...) exactly — existing call
// sites that don't pass a suffix see no change in resolved names.
type Suffix string

// apply appends "-<suffix>" to base, or returns base unchanged when the
// suffix is empty.
func (s Suffix) apply(base string) string {
	if s == "" {
		return base
	}
	return base + "-" + string(s)
}

// Namespace resolves the physical namespace name for a logical base name.
func (s Suffix) Namespace(base string) string { return s.apply(base) }

// Deployment resolves the physical ModelDeployment (Helm release / KAITO
// InferenceSet) name for a logical base name.
func (s Suffix) Deployment(base string) string { return s.apply(base) }

// Gateway resolves the per-namespace Gateway name, mirroring the
// charts/modelharness convention "<namespace>-gw". Gateway names derive
// from the (already-suffixed) namespace, not from Suffix directly, so
// callers should pass a namespace already resolved via Suffix.Namespace.
func (Suffix) Gateway(namespace string) string { return namespace + "-gw" }

// HostHeader resolves the Host header value the apikey-authz service
// maps to a namespace (subdomain = namespace).
func (Suffix) HostHeader(namespace string) string { return namespace + ".gw.example.com" }
