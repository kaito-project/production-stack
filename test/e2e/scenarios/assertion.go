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
	"fmt"
)

// Assertion is one independently-nameable, reusable check within a
// scenario group. Name identifies the check in error messages and feeds
// Ginkgo `It` descriptions in the Ginkgo adapters; Run performs the check
// — including any internal polling — and returns nil on success.
//
// Any polling/retry an assertion needs MUST live inside Run itself.
// Scenario groups deliberately do not wrap a whole case (or a whole
// assertion list) in a retry loop: a flaky assertion should fail with a
// clear, single error rather than being silently re-run from scratch.
type Assertion struct {
	Name string
	Run  func(ctx context.Context) error
}

// namedAssertion builds an Assertion whose Run wraps fn's error (if any)
// with the assertion's Name, so the failure is traceable even when a
// caller aggregates several assertions' errors together (e.g. a
// non-Ginkgo caller running the whole list and only checking the final
// error, rather than reporting each Assertion individually).
func namedAssertion(name string, fn func(ctx context.Context) error) Assertion {
	return Assertion{
		Name: name,
		Run: func(ctx context.Context) error {
			if err := fn(ctx); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			return nil
		},
	}
}

// RunAll runs every assertion in order and returns the first failure,
// naming which assertion failed. Intended for non-Ginkgo callers (e.g. a
// downstream harness that wants a single pass/fail result); Ginkgo
// adapters should instead invoke each Assertion.Run individually from its
// own `It` so failures are reported per-check.
func RunAll(ctx context.Context, assertions []Assertion) error {
	for _, a := range assertions {
		if err := a.Run(ctx); err != nil {
			return err
		}
	}
	return nil
}
