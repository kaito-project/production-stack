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

func TestValidateCounterSnapshots(t *testing.T) {
	tests := []struct {
		name    string
		before  PodMetricSnapshot
		after   PodMetricSnapshot
		wantErr bool
	}{
		{
			name:   "stable pod set and increasing counters",
			before: PodMetricSnapshot{"pod-a": 10, "pod-b": 20},
			after:  PodMetricSnapshot{"pod-a": 12, "pod-b": 20},
		},
		{
			name:    "pod added",
			before:  PodMetricSnapshot{"pod-a": 10},
			after:   PodMetricSnapshot{"pod-a": 12, "pod-b": 1},
			wantErr: true,
		},
		{
			name:    "pod replaced",
			before:  PodMetricSnapshot{"pod-a": 10},
			after:   PodMetricSnapshot{"pod-b": 12},
			wantErr: true,
		},
		{
			name:    "counter reset",
			before:  PodMetricSnapshot{"pod-a": 10},
			after:   PodMetricSnapshot{"pod-a": 2},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateCounterSnapshots(test.before, test.after)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateCounterSnapshots() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
