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

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/onsi/ginkgo/v2/types"
)

// Spec status values used throughout the report. They mirror the lowercase
// strings produced by Ginkgo's types.SpecState.String().
const (
	StatusPassed  = "passed"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
	StatusPending = "pending"
)

// statusRank orders statuses from best to worst so that, when two specs share
// the same leaf text, the worst outcome wins (a failure is never hidden by a
// passing namesake).
var statusRank = map[string]int{
	StatusPending: 0,
	StatusSkipped: 1,
	StatusPassed:  2,
	StatusFailed:  3,
}

// parseResults reads one or more Ginkgo JSON reports (as written by
// `ginkgo --json-report`) and returns a map from spec leaf text (the It title)
// to its outcome. When the same leaf text appears more than once, the worst
// outcome is kept. A missing path returns an empty map with no error so the
// report can still be generated as a source-only coverage report.
func parseResults(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read results: %w", err)
	}

	var reports []types.Report
	if err := json.Unmarshal(raw, &reports); err != nil {
		return nil, fmt.Errorf("parse results JSON: %w", err)
	}

	results := make(map[string]string)
	for _, r := range reports {
		for _, spec := range r.SpecReports {
			// Ignore setup/teardown nodes; only report on actual It specs.
			if spec.LeafNodeText == "" {
				continue
			}
			status := normalizeStatus(spec.State)
			if status == "" {
				continue
			}
			key := spec.LeafNodeText
			if prev, ok := results[key]; !ok || statusRank[status] > statusRank[prev] {
				results[key] = status
			}
		}
	}
	return results, nil
}

// normalizeStatus collapses Ginkgo's spec states into the four buckets the
// report displays. Timed-out/aborted/panicked/interrupted specs are treated as
// failures.
func normalizeStatus(state types.SpecState) string {
	switch {
	case state.Is(types.SpecStatePassed):
		return StatusPassed
	case state.Is(types.SpecStatePending):
		return StatusPending
	case state.Is(types.SpecStateSkipped):
		return StatusSkipped
	case state.Is(types.SpecStateFailureStates):
		return StatusFailed
	default:
		return ""
	}
}
