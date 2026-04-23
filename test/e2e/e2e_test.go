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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// gatewayURL is set once in BeforeSuite and reused by all test files.
var gatewayURL string

var _ = BeforeSuite(func() {
	url, err := utils.GetGatewayURL()
	Expect(err).NotTo(HaveOccurred(), "failed to set up gateway port-forward")
	gatewayURL = url

	// Create InferenceSets and wait for the full routing pipeline once
	// for the entire suite. Individual Describes share these resources.
	utils.SetupInferenceSetsWithRouting(modelNames, testNamespace, gatewayURL)
})

var _ = AfterSuite(func() {
	utils.TeardownInferenceSetsWithRouting(modelNames, testNamespace)
	utils.CleanupPortForward()
})

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Production Stack E2E Test Suite")
}
