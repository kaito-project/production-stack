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
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Long deployment name tests.
//
// The deployment name becomes the InferenceSet name and, through the
// modeldeployment chart, the InferencePool name, the EPP Service name and the
// EPP `inferencepool` selector label value, all capped at 63 characters.
// This suite deploys a model under a 63-character name and verifies it serves
// inference end to end, then verifies that a 64-character name is rejected
// without creating an InferenceSet.
var _ = Describe("Long deployment name", Ordered, utils.GinkgoLabelInferenceSet, func() {
	var (
		ctx     context.Context
		gateway deploy.GatewayEndpoint
		values  deploy.ModelDeploymentValues
	)

	BeforeAll(func() {
		ctx = context.Background()
		values = CaseDeployments[CaseLongName][0]
		Expect(values.Name).To(HaveLen(63))

		DeferCleanup(UninstallCase, CaseLongName)
		gateway = InstallCase(CaseLongName)
	})

	It("serves inference for a 63-character deployment name", func() {
		Eventually(func() error {
			return utils.CheckChatSuccess(ctx, gateway, values.Name)
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("rejects a 64-character deployment name", func() {
		tooLong := values
		tooLong.Name = values.Name + "x"
		DeferCleanup(func(ctx SpecContext) {
			Expect(utils.UninstallModelDeployment(ctx, tooLong.Name, tooLong.Namespace)).To(Succeed())
		})

		err := utils.InstallModelDeployment(ctx, tooLong)
		Expect(err).To(HaveOccurred(), "installing a 64-character deployment name should fail")
		GinkgoWriter.Printf("64-character deployment name rejected: %v\n", err)

		By("verifying no InferenceSet was created")
		dynClient, err := utils.GetDynamicClient()
		Expect(err).NotTo(HaveOccurred())
		_, err = dynClient.Resource(utils.InferenceSetGVR).Namespace(tooLong.Namespace).
			Get(ctx, tooLong.Name, metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected no InferenceSet %s, got %v", tooLong.Name, err)
	})
})
