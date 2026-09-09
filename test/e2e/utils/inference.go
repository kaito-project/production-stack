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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
)

const (
	// InferenceSetReadyTimeout is the default timeout for waiting for a
	// modeldeployment to become servable. Extended to 20 minutes to absorb
	// cold-start variance from GPU node bring-up, image reconciliation, and
	// multi-node model warmup.
	InferenceSetReadyTimeout = 20 * time.Minute
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

// WaitForInferenceSetReady waits until the modeldeployment can actually serve:
// the EPP Deployment has a ready replica and every model-serving pod is
// Running, Ready, and has a PodIP. Both are prerequisites for the gateway to
// route a request — the EPP picks the endpoint, the pod holds the model.
//
// Readiness is read off built-in kinds rather than the InferencePool or
// InferenceSet status: a managed control plane such as AI Manager grants the
// caller read access to built-in kinds only, so waiting on a CRD would fail
// with 403 before any spec gets to run.
func WaitForInferenceSetReady(ctx context.Context, values deploy.ModelDeploymentValues, timeout time.Duration) error {
	clientset, err := GetK8sClientset()
	if err != nil {
		return fmt.Errorf("init clientset: %w", err)
	}

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = inferenceSetReady(ctx, clientset, values); lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(PollInterval):
		}
	}
	return fmt.Errorf("timed out after %s waiting for modeldeployment %s/%s to become ready: %w",
		timeout, values.Namespace, values.Name, lastErr)
}

func inferenceSetReady(ctx context.Context, clientset kubernetes.Interface, values deploy.ModelDeploymentValues) error {
	namespace, eppName := values.Namespace, EPPServiceName(values.Name)

	epp, err := clientset.AppsV1().Deployments(namespace).Get(ctx, eppName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("EPP Deployment %s/%s not created yet", namespace, eppName)
		}
		return fmt.Errorf("get EPP Deployment %s/%s: %w", namespace, eppName, err)
	}
	if epp.Status.ReadyReplicas < 1 {
		return fmt.Errorf("EPP Deployment %s/%s has no ready replica yet", namespace, eppName)
	}

	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: values.InferencePodSelector(),
	})
	if err != nil {
		return fmt.Errorf("list inference pods for %s: %w", values.Name, err)
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("no inference pods found for %s", values.Name)
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning {
			return fmt.Errorf("inference pod %s is %s, not Running", pod.Name, pod.Status.Phase)
		}
		if len(pod.Status.ContainerStatuses) == 0 || !pod.Status.ContainerStatuses[0].Ready {
			return fmt.Errorf("inference pod %s container is not Ready yet", pod.Name)
		}
		if pod.Status.PodIP == "" {
			return fmt.Errorf("inference pod %s has no PodIP yet", pod.Name)
		}
	}
	return nil
}

// CreateInferenceSetWithRouting installs the modeldeployment Helm chart with
// the supplied values. The chart provisions the InferenceSet, InferencePool,
// EPP (Deployment + Service + RBAC + ConfigMap), and HTTPRoute in a single
// step. The EPP runs with `--secure-serving=false`, so no DestinationRule is
// required for the Istio Gateway to reach it.
//
// The call returns once the release is applied; callers that need the
// deployment to actually serve traffic must follow up with
// WaitForInferenceSetReady.
func CreateInferenceSetWithRouting(ctx context.Context, values deploy.ModelDeploymentValues) error {
	if err := InstallModelDeployment(ctx, values); err != nil {
		return fmt.Errorf("failed to install modeldeployment for %s: %w", values.Name, err)
	}
	return nil
}

// CleanupInferenceSetWithRouting uninstalls the modeldeployment Helm release,
// which removes the InferenceSet, InferencePool, EPP artifacts, and HTTPRoute.
func CleanupInferenceSetWithRouting(ctx context.Context, name, namespace string) error {
	if err := UninstallModelDeployment(ctx, name, namespace); err != nil {
		return fmt.Errorf("failed to uninstall modeldeployment %s: %w", name, err)
	}
	return nil
}
