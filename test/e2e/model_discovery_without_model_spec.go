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
	"fmt"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Model discovery without a model.
//
// What this verifies (and why):
//
//	charts/modelharness renders the `models-api` HTTPRoute that sends
//	GET /v1/models and GET /v1/models/{id} to the cluster-wide
//	productionstack-status-reporter, so model discovery answers as soon as
//	the harness is installed, before any modeldeployment exists.
//
//	On an App Routing cluster that route is also what publishes the
//	namespace hostname: the default-domain external-dns derives DNS records
//	only from the Routes attached to a Gateway listener, never from the
//	Gateway itself. While the modeldeployments' HTTPRoutes were the only
//	Routes, a namespace without a model had no resolvable hostname, and
//	deleting the last model withdrew it. The Azure provider reaches the
//	Gateway through that hostname, so the first assertion waits for it to be
//	published; the upstream provider uses a port-forward.
//
// Not Serial: it only touches its own namespace and reads from the status
// reporter.
var _ = Describe("Model discovery without a model", Ordered, utils.GinkgoLabelModelDiscovery, utils.GinkgoLabelNoGPU, func() {
	var (
		ctx     context.Context
		ns      string
		gateway deploy.GatewayEndpoint
	)

	BeforeAll(func() {
		ctx = context.Background()
		ns = utils.UniqueName("e2e-discovery-no-model")
		DeferCleanup(func() {
			Expect(utils.DeleteNamespace(context.Background(), ns)).To(Succeed(),
				"failed to delete namespace %s", ns)
		})
		Expect(utils.EnsureNamespace(ctx, ns)).To(Succeed(), "failed to install the modelharness in %s", ns)

		var err error
		gateway, err = utils.OpenGateway(ctx, ns, ns)
		Expect(err).NotTo(HaveOccurred(), "failed to open the gateway in %s", ns)
	})

	It("lists no models", func() {
		var list *utils.ModelList
		// The budget SetupInferenceSetsWithRouting gives a new deployment's
		// route: on an App Routing cluster it also covers publishing the
		// hostname and the expiry of a negatively cached lookup.
		Eventually(func() error {
			resp, err := utils.SendModels(gateway, utils.ModelsPath)
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusOK {
				body, _ := utils.ReadResponseBody(resp)
				return fmt.Errorf("expected 200, got %d: %s", resp.StatusCode, string(body))
			}
			list, err = utils.ParseModelList(resp)
			return err
		}, utils.InferenceSetReadyTimeout, 10*time.Second).Should(Succeed(),
			"GET /v1/models should answer in a namespace without a modeldeployment")

		Expect(list.Object).To(Equal("list"))
		Expect(list.Data).To(BeEmpty(), "a namespace without a modeldeployment has no models to list")
	})

	It("answers an unknown model id with 404 model_not_found", func() {
		// The catch-all would answer this bodyless GET with 400
		// invalid_request_body, so a 404 shows the status reporter answered.
		resp, err := utils.SendModels(gateway, utils.ModelRetrievePath("e2e-no-such-model"))
		Expect(err).NotTo(HaveOccurred(), "request to gateway failed")
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))

		errResp, err := utils.ParseErrorResponse(resp)
		Expect(err).NotTo(HaveOccurred(), "response body should be a JSON error envelope")
		Expect(errResp.ErrorCode()).To(Equal("model_not_found"))
		Expect(errResp.Error.Type).To(Equal("invalid_request_error"))
	})
})
