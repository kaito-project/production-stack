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
	"errors"
	"strings"
	"testing"
)

func TestNamedAssertion_SuccessReturnsNil(t *testing.T) {
	a := namedAssertion("does nothing", func(context.Context) error { return nil })
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}
	if a.Name != "does nothing" {
		t.Errorf("Name = %q", a.Name)
	}
}

func TestNamedAssertion_FailureIsNamedInError(t *testing.T) {
	underlying := errors.New("boom")
	a := namedAssertion("my check", func(context.Context) error { return underlying })

	err := a.Run(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "my check") {
		t.Errorf("error %q does not name the failing assertion", err.Error())
	}
	if !errors.Is(err, underlying) {
		t.Errorf("error does not wrap the underlying cause: %v", err)
	}
}

func TestRunAll_StopsAtFirstFailure(t *testing.T) {
	var ran []string
	mk := func(name string, fail bool) Assertion {
		return namedAssertion(name, func(context.Context) error {
			ran = append(ran, name)
			if fail {
				return errors.New("fail")
			}
			return nil
		})
	}

	err := RunAll(context.Background(), []Assertion{
		mk("first", false),
		mk("second", true),
		mk("third", false),
	})

	if err == nil {
		t.Fatal("expected RunAll to surface the second assertion's failure")
	}
	if !strings.Contains(err.Error(), "second") {
		t.Errorf("error %q does not name the failing assertion", err.Error())
	}
	if got, want := ran, []string{"first", "second"}; !equalStrings(got, want) {
		t.Errorf("ran assertions = %v, want %v (third should not run)", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
