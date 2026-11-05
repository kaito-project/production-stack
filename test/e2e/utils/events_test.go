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
	"testing"
	"time"
)

func TestReporterTimingsFromArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    ReporterTimings
		wantErr bool
	}{
		{
			name: "chart defaults when the flags are absent",
			args: []string{"--leader-elect"},
			want: ReporterTimings{Resync: 60 * time.Second, StartupGrace: 60 * time.Second, EventRefresh: 300 * time.Second},
		},
		{
			name: "E2E install",
			args: []string{"--resync-seconds=60", "--startup-grace-seconds=30", "--event-refresh-seconds=120"},
			want: ReporterTimings{Resync: 60 * time.Second, StartupGrace: 30 * time.Second, EventRefresh: 120 * time.Second},
		},
		{
			name: "zero startup grace is allowed",
			args: []string{"--startup-grace-seconds=0"},
			want: ReporterTimings{Resync: 60 * time.Second, StartupGrace: 0, EventRefresh: 300 * time.Second},
		},
		{name: "zero resync", args: []string{"--resync-seconds=0"}, wantErr: true},
		{name: "negative grace", args: []string{"--startup-grace-seconds=-1"}, wantErr: true},
		{name: "not a number", args: []string{"--event-refresh-seconds=5m"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := reporterTimingsFromArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDebouncedEmitTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timings ReporterTimings
		want    time.Duration
	}{
		// Regression: with the chart's 60s grace, a fixed 3m wait timed out
		// on clusterBBRNotReady.
		{
			name:    "chart defaults",
			timings: ReporterTimings{Resync: 60 * time.Second, StartupGrace: 60 * time.Second},
			want:    270 * time.Second,
		},
		{
			name:    "E2E install rounds the grace up to one resync",
			timings: ReporterTimings{Resync: 60 * time.Second, StartupGrace: 30 * time.Second},
			want:    270 * time.Second,
		},
		{
			name:    "grace longer than one resync",
			timings: ReporterTimings{Resync: 60 * time.Second, StartupGrace: 90 * time.Second},
			want:    330 * time.Second,
		},
		{
			name:    "no grace",
			timings: ReporterTimings{Resync: 30 * time.Second},
			want:    150 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.timings.DebouncedEmitTimeout(); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}
