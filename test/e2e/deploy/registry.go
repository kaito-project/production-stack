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

package deploy

import (
	"fmt"
	"os"
	"sort"
	"sync"
)

const (
	// EnvBackend selects the Deployer backend by registered name.
	EnvBackend = "E2E_DEPLOYMENT_BACKEND"

	// DefaultBackend is the backend used when EnvBackend is unset, keeping
	// local and non-Azure E2E runs on the Helm CLI.
	DefaultBackend = "helm"
)

// Factory constructs a Deployer for a registered backend.
type Factory func() (Deployer, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register makes a backend available under name. Out-of-tree backends call it
// from an init function so a blank import is enough to enable them.
//
// It panics on an empty name, a nil factory, or a duplicate registration,
// because all three indicate a wiring bug that must fail at startup rather
// than midway through an E2E run.
func Register(name string, factory Factory) {
	if name == "" {
		panic("deploy: Register called with an empty backend name")
	}
	if factory == nil {
		panic(fmt.Sprintf("deploy: Register called with a nil factory for backend %q", name))
	}

	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("deploy: backend %q is already registered", name))
	}
	registry[name] = factory
}

// Backends returns the sorted names of every registered backend.
func Backends() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()

	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// New constructs the Deployer registered under name.
func New(name string) (Deployer, error) {
	registryMu.RLock()
	factory, ok := registry[name]
	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("deploy: unknown backend %q (registered: %v); set %s to one of them",
			name, Backends(), EnvBackend)
	}

	deployer, err := factory()
	if err != nil {
		return nil, fmt.Errorf("deploy: construct backend %q: %w", name, err)
	}
	if deployer == nil {
		return nil, fmt.Errorf("deploy: backend %q factory returned a nil Deployer", name)
	}
	return deployer, nil
}

// NewFromEnv constructs the Deployer named by EnvBackend, falling back to
// DefaultBackend when the variable is unset or empty.
func NewFromEnv() (Deployer, error) {
	name := os.Getenv(EnvBackend)
	if name == "" {
		name = DefaultBackend
	}
	return New(name)
}
