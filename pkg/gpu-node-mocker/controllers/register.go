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

package controllers

import (
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
)

// NewControllers sets up all controllers with the given manager.
func NewControllers(mgr ctrl.Manager, cfg Config) error {
	ncr := &NodeClaimReconciler{
		Client: mgr.GetClient(),
		Config: cfg,
	}
	if err := ncr.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create NodeClaim controller: %w", err)
	}

	if err := (&ShadowPodReconciler{
		Client: mgr.GetClient(),
		Config: cfg,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create ShadowPod controller: %w", err)
	}

	if err := (&FakeNodeGCReconciler{
		Client:           mgr.GetClient(),
		StopLeaseRenewer: ncr.stopLeaseRenewer,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create FakeNodeGC controller: %w", err)
	}

	if err := (&ShadowPodGCReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create ShadowPodGC controller: %w", err)
	}

	return nil
}
