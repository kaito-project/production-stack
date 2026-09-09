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
)

// NamespaceAPIKey returns the API key that authenticates requests to the
// namespace's gateway, from whichever backend installed the harness.
//
// Specs never read the Secret directly: a managed backend mints the key
// through its own API and does not let the caller read Secrets at all.
func NamespaceAPIKey(ctx context.Context, namespace string) (string, error) {
	d, err := CurrentDeployer()
	if err != nil {
		return "", err
	}
	return d.NamespaceAPIKey(ctx, namespace)
}
