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
	"net/http"
	"testing"
)

const sampleConfigDump = `{
  "configs": [
    {
      "@type": "type.googleapis.com/envoy.admin.v3.ListenersConfigDump",
      "dynamic_listeners": [
        {
          "active_state": {
            "listener": {
              "filter_chains": [
                {
                  "filters": [
                    {
                      "typed_config": {
                        "@type": "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
                        "http_filters": [
                          {"name": "envoy.filters.http.ext_authz"},
                          {"name": "envoy.filters.http.ext_proc.bbr"},
                          {"name": "envoy.filters.http.ext_proc"},
                          {"name": "envoy.filters.http.router"}
                        ]
                      }
                    }
                  ]
                }
              ]
            }
          }
        }
      ]
    }
  ]
}`

func TestExtractGatewayHTTPFilterNames_ParsesOrderedFilters(t *testing.T) {
	names := extractGatewayHTTPFilterNames(sampleConfigDump)
	want := []string{
		"envoy.filters.http.ext_authz",
		"envoy.filters.http.ext_proc.bbr",
		"envoy.filters.http.ext_proc",
		"envoy.filters.http.router",
	}
	if !equalStrings(names, want) {
		t.Fatalf("got %v, want %v", names, want)
	}
}

func TestExtractGatewayHTTPFilterNames_InvalidJSONReturnsNil(t *testing.T) {
	if got := extractGatewayHTTPFilterNames("not json"); got != nil {
		t.Errorf("expected nil for invalid JSON, got %v", got)
	}
}

func TestHasInferenceFilters(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		want  bool
	}{
		{"both present", []string{"envoy.filters.http.ext_authz", "envoy.filters.http.router"}, true},
		{"missing router", []string{"envoy.filters.http.ext_authz"}, false},
		{"missing authz", []string{"envoy.filters.http.router"}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		if got := hasInferenceFilters(c.names); got != c.want {
			t.Errorf("%s: hasInferenceFilters(%v) = %v, want %v", c.name, c.names, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("truncate under limit changed the string: %q", got)
	}
	if got := truncate("hello world", 5); got != "hello…" {
		t.Errorf("truncate(...) = %q, want %q", got, "hello…")
	}
}

func TestAssertEnvoyFilterOrder_PassesOnCorrectOrder(t *testing.T) {
	kube := newFakeKubeOps()
	kube.firstRunningPodName = "gw-pod-abc"
	kube.execOutput = sampleConfigDump

	state := FilterOrderState{cfg: FilterOrderConfig{Kube: kube}}
	if err := AssertEnvoyFilterOrder(context.Background(), state); err != nil {
		t.Fatalf("expected pass, got %v", err)
	}
}

func TestAssertEnvoyFilterOrder_FailsOnWrongOrder(t *testing.T) {
	kube := newFakeKubeOps()
	kube.firstRunningPodName = "gw-pod-abc"
	// BBR after EPP: wrong order.
	kube.execOutput = `{
      "configs": [{"dynamic_listeners": [{"active_state": {"listener": {"filter_chains": [{"filters": [{"typed_config": {
        "@type": "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
        "http_filters": [
          {"name": "envoy.filters.http.ext_authz"},
          {"name": "envoy.filters.http.ext_proc"},
          {"name": "envoy.filters.http.ext_proc.bbr"},
          {"name": "envoy.filters.http.router"}
        ]}}]}]}}}]}]}`

	state := FilterOrderState{cfg: FilterOrderConfig{Kube: kube}}
	if err := AssertEnvoyFilterOrder(context.Background(), state); err == nil {
		t.Fatal("expected failure when BBR runs after the InferencePool ext_proc")
	}
}

func TestAssertMissingModelFieldReturns400_ParsesErrorEnvelope(t *testing.T) {
	chat := &fakeChatClient{rules: []fakeChatRule{{
		status: http.StatusBadRequest,
		body:   `{"error":{"message":"model is required","type":"invalid_request_error","code":"invalid_request_body"}}`,
		header: map[string]string{"x-kaito-error-source": "bbr"},
	}}}
	state := FilterOrderState{cfg: FilterOrderConfig{Chat: chat}, access: NamespaceAccess{GatewayURL: "http://fake"}}

	if err := AssertMissingModelFieldReturns400(context.Background(), state); err != nil {
		t.Fatalf("expected pass, got %v", err)
	}
}

func TestAssertMissingModelFieldReturns400_FailsOnWrongErrorCode(t *testing.T) {
	chat := &fakeChatClient{rules: []fakeChatRule{{
		status: http.StatusBadRequest,
		body:   `{"error":{"message":"bad","type":"invalid_request_error","code":"something_else"}}`,
		header: map[string]string{"x-kaito-error-source": "bbr"},
	}}}
	state := FilterOrderState{cfg: FilterOrderConfig{Chat: chat}, access: NamespaceAccess{GatewayURL: "http://fake"}}

	if err := AssertMissingModelFieldReturns400(context.Background(), state); err == nil {
		t.Fatal("expected failure for an unexpected error code")
	}
}

func TestAssertNonJSONContentTypeNo5xx(t *testing.T) {
	pass := &fakeChatClient{def: fakeChatRule{status: 200}}
	fail := &fakeChatClient{def: fakeChatRule{status: 502}}

	passState := FilterOrderState{cfg: FilterOrderConfig{Chat: pass}, access: NamespaceAccess{GatewayURL: "http://fake"}, modelName: "m"}
	if err := AssertNonJSONContentTypeNo5xx(context.Background(), passState); err != nil {
		t.Errorf("expected pass on 200, got %v", err)
	}

	failState := FilterOrderState{cfg: FilterOrderConfig{Chat: fail}, access: NamespaceAccess{GatewayURL: "http://fake"}, modelName: "m"}
	if err := AssertNonJSONContentTypeNo5xx(context.Background(), failState); err == nil {
		t.Error("expected failure on 502")
	}
}
