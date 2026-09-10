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
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
)

type gatewayRecordingDeployer struct {
	deploy.Deployer
	gateways  []deploy.GatewayValues
	harnesses []deploy.ModelHarnessValues
	endpoint  deploy.GatewayEndpoint
	headers   map[string][]deploy.AuthHeader
	authCalls map[string]int
}

type cleanupRecordingDeployer struct {
	deploy.Deployer
	calls  []string
	errors map[string]error
}

func (backend *cleanupRecordingDeployer) UninstallModelDeployment(_ context.Context, name, namespace string) error {
	call := "deployment:" + namespace + "/" + name
	backend.calls = append(backend.calls, call)
	return backend.errors[call]
}

func (backend *cleanupRecordingDeployer) CloseGateway(_ context.Context, namespace string) error {
	call := "gateway:" + namespace
	backend.calls = append(backend.calls, call)
	return backend.errors[call]
}

func (backend *cleanupRecordingDeployer) UninstallModelHarness(_ context.Context, namespace string) error {
	call := "harness:" + namespace
	backend.calls = append(backend.calls, call)
	return backend.errors[call]
}

func TestCleanupContinuesAfterFailures(t *testing.T) {
	const namespace = "cleanup-test"
	deployerMu.Lock()
	previous := currentDeployer
	deployerMu.Unlock()
	t.Cleanup(func() { SetDeployer(previous); ForgetNamespaceAuthHeaders(namespace) })
	deploymentErr := errors.New("deployment failed")
	gatewayErr := errors.New("gateway failed")
	harnessErr := errors.New("harness failed")
	backend := &cleanupRecordingDeployer{errors: map[string]error{
		"deployment:" + namespace + "/first": deploymentErr,
		"gateway:" + namespace:               gatewayErr,
		"harness:" + namespace:               harnessErr,
	}}
	SetDeployer(backend)
	authHeadersMu.Lock()
	authHeadersCache[namespace] = []deploy.AuthHeader{{Name: "API-Key", Value: "stale"}}
	authHeadersMu.Unlock()
	err := CleanupDeploymentsAndNamespace(context.Background(), []deploy.ModelDeploymentValues{
		{Name: "first"}, {Name: "second", Namespace: "explicit"},
	}, namespace)
	for _, expected := range []error{deploymentErr, gatewayErr, harnessErr} {
		if !errors.Is(err, expected) {
			t.Fatalf("cleanup lost %v: %v", expected, err)
		}
	}
	want := []string{"deployment:" + namespace + "/first", "deployment:explicit/second", "gateway:" + namespace, "harness:" + namespace}
	if !reflect.DeepEqual(backend.calls, want) {
		t.Fatalf("cleanup calls=%v want=%v", backend.calls, want)
	}
	authHeadersMu.Lock()
	_, cached := authHeadersCache[namespace]
	authHeadersMu.Unlock()
	if cached {
		t.Fatal("failed gateway cleanup left cached credentials")
	}
}

func (d *gatewayRecordingDeployer) OpenGateway(context.Context, string, string) (deploy.GatewayEndpoint, error) {
	return d.endpoint, nil
}

func (d *gatewayRecordingDeployer) AuthHeaders(_ context.Context, namespace string) ([]deploy.AuthHeader, error) {
	d.authCalls[namespace]++
	return d.headers[namespace], nil
}

func (d *gatewayRecordingDeployer) InstallModelHarness(_ context.Context, values deploy.ModelHarnessValues) error {
	d.gateways = append(d.gateways, values.Gateway)
	d.harnesses = append(d.harnesses, values)
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
	if err := InstallModelHarness(ctx, "e2e-ns"); err != nil {
		t.Fatal(err)
	}
	if len(backend.harnesses) != 1 || !backend.harnesses[0].AuthEnabled {
		t.Fatal("E2E harness must enable authentication")
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

func TestOpenGatewayBindsNamespaceCredentials(t *testing.T) {
	const namespaceA, namespaceB = "gateway-auth-a", "gateway-auth-b"
	deployerMu.Lock()
	previous := currentDeployer
	deployerMu.Unlock()
	for _, namespace := range []string{namespaceA, namespaceB} {
		ForgetNamespaceAuthHeaders(namespace)
	}
	t.Cleanup(func() {
		SetDeployer(previous)
		ForgetNamespaceAuthHeaders(namespaceA)
		ForgetNamespaceAuthHeaders(namespaceB)
	})
	endpoint := &fakeEndpoint{urls: []string{"http://gateway/v1"}, host: "gateway.example.com"}
	backend := &gatewayRecordingDeployer{
		endpoint: endpoint,
		headers: map[string][]deploy.AuthHeader{
			namespaceB: {{Name: "API-Key", Value: "key-b"}},
		},
		authCalls: map[string]int{},
	}
	SetDeployer(backend)
	ctx := context.Background()
	gatewayA, err := OpenGateway(ctx, namespaceA, "gateway-a")
	if err != nil {
		t.Fatal(err)
	}
	gatewayB, err := OpenGateway(ctx, namespaceB, "gateway-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newGatewayRequest(http.MethodGet, []RequestOption{WithoutAuth()}).request(ctx, gatewayA, ModelsPath, nil); err != nil {
		t.Fatal(err)
	}
	if backend.authCalls[namespaceA] != 0 {
		t.Fatal("WithoutAuth must not look up credentials")
	}
	if _, err := newGatewayRequest(http.MethodGet, nil).request(ctx, gatewayA, ModelsPath, nil); err == nil {
		t.Fatal("missing credentials must fail")
	}
	backend.headers[namespaceA] = []deploy.AuthHeader{{Name: "API-Key", Value: "key-a"}}
	for _, test := range []struct {
		gateway deploy.GatewayEndpoint
		want    string
	}{
		{gateway: gatewayA, want: "key-a"},
		{gateway: gatewayB, want: "key-b"},
		{gateway: gatewayA, want: "key-a"},
	} {
		request, err := newGatewayRequest(http.MethodGet, nil).request(ctx, test.gateway, ModelsPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		if request.Header.Get("API-Key") != test.want || request.Host != endpoint.host {
			t.Fatal("gateway must preserve Host and use its own namespace credentials")
		}
	}
	if backend.authCalls[namespaceA] != 2 || backend.authCalls[namespaceB] != 1 {
		t.Fatal("cache must retain credentials but never retain an empty result")
	}
	backend.headers[namespaceA] = []deploy.AuthHeader{{Name: "API-Key", Value: "rotated-key-a"}}
	ForgetNamespaceAuthHeaders(namespaceA)
	request, err := newGatewayRequest(http.MethodGet, nil).request(ctx, gatewayA, ModelsPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("API-Key") != "rotated-key-a" {
		t.Fatal("gateway must resolve refreshed credentials rather than capturing the old key")
	}
	gatewayA.Reset()
	if endpoint.resets != 1 {
		t.Fatal("wrapper must forward Reset to the backend endpoint")
	}
}
