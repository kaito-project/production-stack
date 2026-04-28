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

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// InferenceSetReadyTimeout is the default timeout for waiting for an
	// InferenceSet to be reconciled (i.e. the InferencePool has been
	// rendered by the modeldeployment Helm chart and is observable on the
	// cluster).
	InferenceSetReadyTimeout = 5 * time.Minute
)

// InferencePoolName returns the InferencePool name derived from the
// deployment (InferenceSet) name. Matches the modeldeployment chart's
// naming convention.
func InferencePoolName(deploymentName string) string {
	return deploymentName + "-inferencepool"
}

// EPPServiceName returns the EPP service name derived from the deployment
// name. Matches the modeldeployment chart's naming convention.
func EPPServiceName(deploymentName string) string {
	return InferencePoolName(deploymentName) + "-epp"
}

// WaitForInferenceSetReady waits for the InferenceSet's associated
// InferencePool to be present on the cluster, indicating that the Helm
// release has been applied and KAITO has begun reconciling the InferenceSet.
func WaitForInferenceSetReady(ctx context.Context, cl client.Client, name, namespace string, timeout time.Duration) error {
	poolName := InferencePoolName(name)
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		pool := &unstructured.Unstructured{}
		pool.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "inference.networking.k8s.io",
			Version: "v1",
			Kind:    "InferencePool",
		})
		err := cl.Get(ctx, types.NamespacedName{Name: poolName, Namespace: namespace}, pool)
		if err == nil {
			return nil
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("error checking InferencePool %s/%s: %w", namespace, poolName, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(PollInterval):
		}
	}
	return fmt.Errorf("timed out waiting for InferencePool %s/%s to be created", namespace, poolName)
}

// CreateInferenceSetWithRouting installs the modeldeployment Helm chart with
// the supplied values. The chart provisions the InferenceSet, InferencePool,
// EPP (Deployment + Service + RBAC + ConfigMap), and HTTPRoute in a single
// step. The EPP runs with `--secure-serving=false`, so no DestinationRule is
// required for the Istio Gateway to reach it.
//
// After the chart is installed, the call blocks until the InferencePool is
// observable on the cluster.
func CreateInferenceSetWithRouting(ctx context.Context, cl client.Client, values ModelDeploymentValues) error {
	if err := InstallModelDeployment(values); err != nil {
		return fmt.Errorf("failed to install modeldeployment chart for %s: %w", values.Name, err)
	}

	if err := WaitForInferenceSetReady(ctx, cl, values.Name, values.Namespace, InferenceSetReadyTimeout); err != nil {
		return fmt.Errorf("InferenceSet %s not ready: %w", values.Name, err)
	}
	return nil
}

// CleanupInferenceSetWithRouting uninstalls the modeldeployment Helm release,
// which removes the InferenceSet, InferencePool, EPP artifacts, and HTTPRoute.
func CleanupInferenceSetWithRouting(ctx context.Context, _ client.Client, name, namespace string) error {
	_ = ctx
	if err := UninstallModelDeployment(name, namespace); err != nil {
		return fmt.Errorf("failed to uninstall modeldeployment %s: %w", name, err)
	}
	return nil
}
