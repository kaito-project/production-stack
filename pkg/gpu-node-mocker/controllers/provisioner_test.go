// Copyright (c) KAITO authors.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"testing"
)

func TestNewProvisionerMocker_AzureGPU(t *testing.T) {
	m, err := NewProvisionerMocker(ProvisionerAzureGPU, testConfig())
	if err != nil {
		t.Fatalf("NewProvisionerMocker(azure): %v", err)
	}
	if _, ok := m.(*GPUProvisionerMocker); !ok {
		t.Fatalf("expected *GPUProvisionerMocker, got %T", m)
	}
	if m.Type() != ProvisionerAzureGPU {
		t.Errorf("Type() = %q, want %q", m.Type(), ProvisionerAzureGPU)
	}
	crds := m.RequiredCRDs()
	if len(crds) != 1 || crds[0].Resource != "nodeclaims" {
		t.Errorf("RequiredCRDs = %+v, want only nodeclaims", crds)
	}
}

func TestNewProvisionerMocker_Karpenter(t *testing.T) {
	m, err := NewProvisionerMocker(ProvisionerKarpenter, testConfig())
	if err != nil {
		t.Fatalf("NewProvisionerMocker(karpenter): %v", err)
	}
	if _, ok := m.(*KarpenterMocker); !ok {
		t.Fatalf("expected *KarpenterMocker, got %T", m)
	}
	if m.Type() != ProvisionerKarpenter {
		t.Errorf("Type() = %q, want %q", m.Type(), ProvisionerKarpenter)
	}
	crds := m.RequiredCRDs()
	gotResources := map[string]bool{}
	for _, c := range crds {
		gotResources[c.Resource] = true
	}
	if !gotResources["nodeclaims"] || !gotResources["nodepools"] {
		t.Errorf("RequiredCRDs = %+v, want nodeclaims and nodepools", crds)
	}
}

func TestNewProvisionerMocker_Invalid(t *testing.T) {
	if _, err := NewProvisionerMocker("bogus", testConfig()); err == nil {
		t.Fatal("expected error for unsupported provisioner, got nil")
	}
}
