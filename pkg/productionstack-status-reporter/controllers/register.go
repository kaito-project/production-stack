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

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/config"
)

// RBAC markers — the reporter has READ-ONLY access to every resource it
// evaluates, plus permission to create/aggregate Events in kube-system. No
// write access to any watched resource is requested (§3 "read-only API
// access"). The set is scoped to resources the reporter actually reads as
// objects: KAITO NodeClaim health is read from Workspace.status (not NodeClaim
// objects), so it is not listed here.
//
// +kubebuilder:rbac:groups="",resources=namespaces;pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/proxy,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=kaito.sh,resources=inferencesets;workspaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// SetupWithManager builds the read-only clients the reporter needs and
// registers it as a leader-elected Runnable on the manager.
func SetupWithManager(mgr ctrl.Manager, cfg config.Config) error {
	restCfg := mgr.GetConfig()

	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("build clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("build dynamic client: %w", err)
	}

	reporter := NewStatusReporter(cs, dyn, cfg)
	if err := mgr.Add(reporter); err != nil {
		return fmt.Errorf("add status reporter to manager: %w", err)
	}
	return nil
}
