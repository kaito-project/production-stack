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
//
// The Phase-1 controllers are provisioner-specific and provided via the
// already-constructed ProvisionerMocker. The Phase-2 ShadowPodReconciler is
// provisioner-agnostic and always registered.
func NewControllers(mgr ctrl.Manager, cfg Config, mocker ProvisionerMocker) error {
	if err := mocker.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to set up %s mocker: %w", mocker.Type(), err)
	}

	if err := (&ShadowPodReconciler{
		Client: mgr.GetClient(),
		Config: cfg,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("unable to create ShadowPod controller: %w", err)
	}

	return nil
}
