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

// Package e2e holds the Production Stack E2E specs.
//
// The specs live in ordinary (non _test.go) files so out-of-tree repositories
// can reuse them: importing this package for its side effects registers every
// spec into the global Ginkgo suite, and the importer supplies its own
// RunSpecs entry point and deploy.Deployer backend.
//
//	import (
//		"testing"
//
//		. "github.com/onsi/ginkgo/v2"
//		. "github.com/onsi/gomega"
//
//		_ "github.com/kaito-project/production-stack/test/e2e" // registers the specs
//		"github.com/kaito-project/production-stack/test/e2e/utils"
//	)
//
//	func TestE2E(t *testing.T) {
//		utils.SetDeployer(myBackend)
//		RegisterFailHandler(Fail)
//		RunSpecs(t, "My E2E Suite")
//	}
//
// Spec selection is by Ginkgo label (--label-filter), not by import, because
// importing the package always registers the full set.
package e2e

import (
	. "github.com/onsi/ginkgo/v2" //nolint:revive // Ginkgo DSL

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Declared here rather than in e2e_test.go so out-of-tree suites that import
// this package inherit the port-forward cleanup.
var _ = AfterSuite(func() {
	utils.CleanupPortForward()
})
