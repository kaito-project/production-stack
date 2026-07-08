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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

// Event message hygiene tests (issue #87, proposal §1.5). Reporter Event
// messages are operator-facing and must never embed TSG / runbook URLs — the
// message states the cause and the remediation in prose, and any link belongs
// in documentation, not in the Event stream. This asserts the invariant across
// every Event the reporter has published so far in the suite.
var _ = Describe("Reporter Event Message Hygiene",
	utils.GinkgoLabelStatusReporter, func() {

		var ctx context.Context

		BeforeEach(func() {
			ctx = context.Background()
		})

		It("never embeds http:// or https:// URLs in any reporter Event message", func() {
			events, err := utils.ListReporterEvents(ctx)
			Expect(err).NotTo(HaveOccurred())

			for i := range events {
				ev := events[i]
				msg := ev.Message
				Expect(strings.Contains(msg, "http://")).To(BeFalse(),
					"reporter event %q must not embed an http:// URL: %q", ev.Reason, msg)
				Expect(strings.Contains(msg, "https://")).To(BeFalse(),
					"reporter event %q must not embed an https:// URL: %q", ev.Reason, msg)
			}
		})
	})
