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

package e2e

import (
	"reflect"
	"testing"
)

func TestExtractGatewayHTTPFilterNames(t *testing.T) {
	dump := func(filters ...string) string {
		s := `pilot-agent startup log line
{"configs":[{"@type":"type.googleapis.com/envoy.admin.v3.ListenersConfigDump","dynamic_listeners":[{"active_state":{"listener":{"filter_chains":[{"filters":[{"typed_config":{"@type":"type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager","http_filters":[`
		for i, f := range filters {
			if i > 0 {
				s += ","
			}
			s += `{"name":"` + f + `"}`
		}
		return s + `]}}]}]}}}]}]}`
	}

	cases := map[string][]string{
		"modelharness chart": {"kaito.cors", "envoy.filters.http.ext_authz", "envoy.filters.http.ext_proc.bbr",
			"envoy.filters.http.ext_proc", "envoy.filters.http.router"},
		"AI Manager": {"kaito.cors", "lua-classify-authmode", "ext_authz.apikey", "ext_authz.azure",
			"envoy.filters.http.ext_proc.bbr", "envoy.filters.http.ext_proc", "envoy.filters.http.router"},
	}
	for name, filters := range cases {
		t.Run(name, func(t *testing.T) {
			if got := extractGatewayHTTPFilterNames(dump(filters...)); !reflect.DeepEqual(got, filters) {
				t.Fatalf("extractGatewayHTTPFilterNames() = %v, want %v", got, filters)
			}
		})
	}

	if got := extractGatewayHTTPFilterNames(dump("istio.stats", "envoy.filters.http.router")); got != nil {
		t.Fatalf("listener without ext_authz = %v, want nil", got)
	}
}
