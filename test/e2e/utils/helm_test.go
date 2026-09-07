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

import "testing"

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

func TestDomainFromDNSZoneResourceID(t *testing.T) {
	tests := []struct {
		name       string
		resourceID string
		want       string
		wantErr    bool
	}{
		{
			name:       "Azure DNS zone resource ID",
			resourceID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/dnsZones/6a94eb7ca0631900014b8ab3.australiaeast.aksapp.io",
			want:       "6a94eb7ca0631900014b8ab3.australiaeast.aksapp.io",
		},
		{
			name:       "case insensitive DNS name",
			resourceID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/dnsZones/Example.AKSApp.io",
			want:       "Example.AKSApp.io",
		},
		{name: "empty", wantErr: true},
		{name: "trailing slash", resourceID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/dnsZones/", wantErr: true},
		{name: "invalid zone", resourceID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/dnsZones/not_a_domain", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := domainFromDNSZoneResourceID(test.resourceID)
			if test.wantErr {
				if err == nil {
					t.Fatalf("domainFromDNSZoneResourceID(%q) expected an error", test.resourceID)
				}
				return
			}
			if err != nil {
				t.Fatalf("domainFromDNSZoneResourceID(%q): %v", test.resourceID, err)
			}
			if got != test.want {
				t.Fatalf("domainFromDNSZoneResourceID(%q) = %q, want %q", test.resourceID, got, test.want)
			}
		})
	}
}
