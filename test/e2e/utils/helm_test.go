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

func TestParseDefaultDomainIgnoresAzureCLIWarning(t *testing.T) {
	output := "WARNING: The behavior of this command has been altered by the following extension: aks-preview\n" +
		"6a7ea485cb0cb500011ffe59.australiaeast.aksapp.io\n"

	got, err := parseDefaultDomain(output)
	if err != nil {
		t.Fatal(err)
	}
	if want := "6a7ea485cb0cb500011ffe59.australiaeast.aksapp.io"; got != want {
		t.Fatalf("parseDefaultDomain() = %q, want %q", got, want)
	}
}
