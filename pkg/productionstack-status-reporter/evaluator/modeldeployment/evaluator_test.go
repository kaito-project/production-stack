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

package modeldeployment

import "testing"

func TestIsTerminalLaunchFailure(t *testing.T) {
	cases := []struct {
		name    string
		reason  string
		message string
		want    bool
	}{
		{"sku not available in region", "SKUNotAvailable", "the requested SKU is not available", true},
		{"all instance types exhausted", "InsufficientCapacity", "all requested instance types were unavailable during launch", true},
		{"subscription quota retryable", "SubscriptionQuotaReached", "subscription level ondemand vCPU quota has been reached", false},
		{"zonal allocation retryable", "ZonalAllocationFailure", "allocation failed in zone 1", false},
		{"generic create failure retryable", "CreateInstanceFailed", "creating instance failed", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		if got := isTerminalLaunchFailure(tc.reason, tc.message); got != tc.want {
			t.Errorf("%s: isTerminalLaunchFailure(%q, %q)=%v, want %v", tc.name, tc.reason, tc.message, got, tc.want)
		}
	}
}
