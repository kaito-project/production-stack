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

// defaultGatewayURL is the URL of the cluster-wide Inference Gateway in
// the `default` namespace (installed by hack/e2e/scripts/install-components.sh).
// It is used by edge tests that depend on cluster-wide artifacts which only
// parent the default Gateway: the catch-all model-not-found HTTPRoute and
// the inference-debug-filter EnvoyFilter.
//
// Per-case inference traffic flows through case-owned Gateways resolved
// inside each Ordered Describe's BeforeAll (see InstallCase in cases.go).
var defaultGatewayURL string

var _ = BeforeSuite(func() {
	url, err := utils.GetGatewayURL()
	Expect(err).NotTo(HaveOccurred(), "failed to set up default gateway port-forward")
	defaultGatewayURL = url
})

var _ = AfterSuite(func() {
	utils.CleanupPortForward()
})

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Production Stack E2E Test Suite")
}
