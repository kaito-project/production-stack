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
	"testing"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
)

type gatewayRecordingDeployer struct {
	deploy.Deployer
	gateways []deploy.GatewayValues
}

func (d *gatewayRecordingDeployer) InstallModelHarness(_ context.Context, values deploy.ModelHarnessValues) error {
	d.gateways = append(d.gateways, values.Gateway)
	return nil
}

func (d *gatewayRecordingDeployer) InstallModelDeployment(_ context.Context, values deploy.ModelDeploymentValues) error {
	d.gateways = append(d.gateways, values.Gateway)
	return nil
}

func (d *gatewayRecordingDeployer) UpgradeModelDeployment(_ context.Context, values deploy.ModelDeploymentValues) error {
	d.gateways = append(d.gateways, values.Gateway)
	return nil
}

func TestLifecycleLeavesGatewayResolutionToBackend(t *testing.T) {
	t.Setenv("E2E_PROVIDER", "azure")
	t.Setenv("APP_ROUTING_DEFAULT_DOMAIN", "not_a_domain")
	deployerMu.Lock()
	previous := currentDeployer
	deployerMu.Unlock()
	t.Cleanup(func() { SetDeployer(previous) })
	backend := &gatewayRecordingDeployer{}
	SetDeployer(backend)
	ctx := context.Background()
	if err := InstallModelHarness(ctx, "e2e-ns", true); err != nil {
		t.Fatal(err)
	}
	values := deploy.ModelDeploymentValues{Gateway: deploy.GatewayValues{DefaultDomain: "explicit.aksapp.io"}}
	if err := InstallModelDeployment(ctx, values); err != nil {
		t.Fatal(err)
	}
	if err := UpgradeModelDeployment(ctx, values); err != nil {
		t.Fatal(err)
	}
	if len(backend.gateways) != 3 {
		t.Fatalf("backend calls = %d, want 3", len(backend.gateways))
	}
	for index, want := range []deploy.GatewayValues{{}, values.Gateway, values.Gateway} {
		if got := backend.gateways[index]; got != want {
			t.Fatalf("gateway for call %d = %+v, want %+v", index, got, want)
		}
	}
}

func TestIsAzureProvider(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		want     bool
	}{
		{name: "default", want: true},
		{name: "Azure", provider: "azure", want: true},
		{name: "Azure case insensitive", provider: "AZURE", want: true},
		{name: "upstream", provider: "upstream", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("E2E_PROVIDER", test.provider)
			if got := IsAzureProvider(); got != test.want {
				t.Fatalf("IsAzureProvider() = %t, want %t for E2E_PROVIDER=%q", got, test.want, test.provider)
			}
		})
	}
}
