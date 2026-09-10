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
	"sync"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
)

var (
	authHeadersMu    sync.Mutex
	authHeadersCache = map[string][]deploy.AuthHeader{}
)

// NamespaceAuthHeaders returns the headers that each independently authenticate
// a request to the namespace's gateway, from whichever backend installed the
// harness. An empty result means the gateway authenticates nothing.
//
// Specs never build the header themselves: which credential the gateway wants,
// and which header it reads it from, are both properties of the backend. A
// managed gateway may classify requests on one specific header, so a hard-coded
// name silently stops authenticating rather than failing loudly.
//
// Cached per namespace: the readiness loops call this on every attempt, and a
// backend that resolves the credential over its control-plane API would
// otherwise pay a round trip each time.
func NamespaceAuthHeaders(ctx context.Context, namespace string) ([]deploy.AuthHeader, error) {
	authHeadersMu.Lock()
	cached, ok := authHeadersCache[namespace]
	authHeadersMu.Unlock()
	if ok {
		return cached, nil
	}

	d, err := CurrentDeployer()
	if err != nil {
		return nil, err
	}
	headers, err := d.AuthHeaders(ctx, namespace)
	if err != nil {
		return nil, err
	}

	authHeadersMu.Lock()
	authHeadersCache[namespace] = headers
	authHeadersMu.Unlock()
	return headers, nil
}

// ForgetNamespaceAuthHeaders drops the cached headers for a namespace, so a
// torn-down or rotated namespace does not serve a stale credential.
func ForgetNamespaceAuthHeaders(namespace string) {
	authHeadersMu.Lock()
	delete(authHeadersCache, namespace)
	authHeadersMu.Unlock()
}

// NamespaceRequestOptions returns the options a spec must attach so its
// requests reach the namespace's gateway authenticated, or nil when the gateway
// authenticates nothing.
//
// This is for cases that are not *about* authentication: they want a working
// request, and whether that needs a credential is the backend's business. Cases
// that assert on authentication behaviour build their options explicitly so a
// deliberately unauthenticated request stays unauthenticated.
func NamespaceRequestOptions(ctx context.Context, namespace string) ([]RequestOption, error) {
	headers, err := NamespaceAuthHeaders(ctx, namespace)
	if err != nil || len(headers) == 0 {
		return nil, err
	}
	return []RequestOption{WithAuth(headers[0])}, nil
}
