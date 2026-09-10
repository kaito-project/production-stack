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
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // Ginkgo DSL
	. "github.com/onsi/gomega"    //nolint:revive // Gomega DSL
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
)

// EnsureNamespace provisions the workload namespace and everything the
// modelharness owns in it: the Istio Gateway (named "<name>-gw" by chart
// default), the catch-all `model-not-found-direct` EnvoyFilter (Envoy
// `direct_response` returning 404 + OpenAI-compatible JSON for any request not
// matched by a deployment-specific HTTPRoute, plus the model-discovery routes),
// the AuthorizationPolicy + APIKey CR that wire
// the Gateway into the cluster-wide apikey-ext-authz CUSTOM provider, and the
// CiliumNetworkPolicy that locks down East-West ingress while keeping the
// per-namespace gateway pod reachable from outside the namespace (matched via
// the standard `gateway.networking.k8s.io/gateway-name` label that Istio
// stamps on every gateway pod). The chart-default `allowedIngressNamespaces`
// (currently keda + kaito-system + kube-system + monitoring) covers the
// control-plane scrapers — `keda-kaito-scaler` and the gpu-node-mocker /
// kaito-workspace controllers — that need to reach shadow pods directly from
// outside the workload namespace.
//
// The namespace is created by the Deployer rather than here, so a non-Helm
// backend can provision it through its own API instead of needing cluster
// credentials of its own.
//
// Safe to call repeatedly; the underlying deployer operations are idempotent.
func EnsureNamespace(ctx context.Context, name string) error {
	if err := InstallModelHarness(ctx, name); err != nil {
		return fmt.Errorf("install modelharness in %s: %w", name, err)
	}
	ForgetNamespaceAuthHeaders(name)
	if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		headers, err := NamespaceAuthHeaders(ctx, name)
		return len(headers) > 0, err
	}); err != nil {
		return fmt.Errorf("wait for authentication credentials in namespace %s: %w", name, err)
	}
	return nil
}

// DeleteNamespace removes the modelharness from the namespace, which also
// deletes the namespace itself and cascades everything left in it.
func DeleteNamespace(ctx context.Context, name string) error {
	var cleanupErrors []error
	// Release the gateway transport before the namespace is gone, so the
	// backend does not keep trying to reach one that no longer exists.
	if err := CloseGateway(ctx, name); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("close gateway for %s: %w", name, err))
	}
	// Case namespaces are named deterministically, so a cached credential
	// would otherwise be served to the next harness installed under this name.
	ForgetNamespaceAuthHeaders(name)
	if err := UninstallModelHarness(ctx, name); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("uninstall modelharness from %s: %w", name, err))
	}
	return errors.Join(cleanupErrors...)
}

// WaitForGatewayService blocks until the Istio Service backing the named
// Gateway exists AND the gateway Pod has at least one Ready replica, so
// port-forwards started immediately afterwards do not race the
// gateway-controller. Istio creates the Service synchronously when it
// observes the Gateway resource, but the underlying envoy Pod takes
// longer to schedule + become Ready; `kubectl port-forward` to a Service
// with no Ready endpoints hangs until those endpoints appear, which
// causes the 30s port-forward readiness probe to time out.
//
// Readiness is read off the Service and its Pods rather than the Gateway's
// Programmed condition: a managed control plane such as AI Manager grants the
// caller read access to built-in kinds only, so touching the Gateway CRD would
// fail with 403. The Service and Pod are what actually have to be up anyway —
// Programmed only says the controller accepted the Gateway.
//
// The Service is found by the gateway-name label rather than by name. Istio
// names it "<gateway>-<gatewayClassName>", so guessing the name breaks the
// moment the class is not plain "istio" — on AKS App Routing it is
// "<gateway>-approuting-istio".
func WaitForGatewayService(ctx context.Context, namespace, gatewayName string, timeout time.Duration) error {
	clientset, err := GetK8sClientset()
	if err != nil {
		return fmt.Errorf("init clientset: %w", err)
	}

	selector := fmt.Sprintf("gateway.networking.k8s.io/gateway-name=%s", gatewayName)

	return pollUntilReady(ctx, timeout, fmt.Sprintf("gateway %s/%s to be ready (selector=%q)", namespace, gatewayName, selector), func(ctx context.Context) error {
		svcs, err := clientset.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			return err
		}
		if len(svcs.Items) == 0 {
			return fmt.Errorf("gateway service not found")
		}
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err == nil {
			for _, pod := range pods.Items {
				if pod.Status.Phase != corev1.PodRunning {
					continue
				}
				for _, c := range pod.Status.Conditions {
					if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
						return nil
					}
				}
			}
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("gateway has no Ready pod")
	})
}

// SetupInferenceSetsWithRouting idempotently installs the modeldeployment
// Helm chart for each entry in deployments, waits for the EPP and inference
// (shadow) pods to be Ready, and optionally verifies that the Gateway routing
// pipeline is returning HTTP 200 for each deployment.
//
// The modeldeployment chart inlines all of the per-deployment GAIE artifacts
// (InferenceSet, InferencePool, EPP Deployment/Service/ConfigMap/RBAC, and
// HTTPRoute), so no separate DestinationRule creation step is required —
// the EPP runs with `--secure-serving=false` and is reached over plaintext
// gRPC by the Istio Gateway.
//
// Parameters:
//   - deployments: list of ModelDeploymentValues to install. If an entry's
//     Namespace is empty, the namespace argument is used as the default.
//   - namespace: target namespace for entries whose Namespace is unset.
//   - gateway: if non-nil, performs a warm-up request loop per
//     deployment to wait for the BBR → EPP ext_proc pipeline to be ready.
func SetupInferenceSetsWithRouting(deployments []deploy.ModelDeploymentValues, namespace string, gateway deploy.GatewayEndpoint) {
	ctx := context.Background()
	GetClusterClient(TestingCluster)

	// Apply namespace default eagerly so subsequent waits use the correct ns.
	resolved := make([]deploy.ModelDeploymentValues, len(deployments))
	for i, d := range deployments {
		if d.Namespace == "" {
			d.Namespace = namespace
		}
		resolved[i] = d
	}

	for _, d := range resolved {
		By(fmt.Sprintf("Installing modeldeployment %s (model=%s) in %s", d.Name, d.Model, d.Namespace))
		Expect(InstallModelDeployment(ctx, d)).To(Succeed(),
			"failed to install modeldeployment for %s", d.Name)
	}

	for _, d := range resolved {
		By(fmt.Sprintf("Waiting for modeldeployment %s to become ready", d.Name))
		Expect(WaitForInferenceSetReady(ctx, d, InferenceSetReadyTimeout)).
			To(Succeed(), "modeldeployment %s not ready", d.Name)
	}

	// Wait for the full BBR → EPP ext_proc pipeline to be ready.
	// Pods being Running does not guarantee ext_proc gRPC connections
	// are established; requests may 500 during the warm-up window.
	//
	// The HTTPRoute matches X-Gateway-Model-Name against the deployment
	// name (.Values.name in the chart), so the gateway is exercised by
	// sending requests with `"model": "<deploymentName>"`.
	if gateway != nil {
		for _, d := range resolved {
			d := d

			By(fmt.Sprintf("Waiting for gateway routing to be ready for deployment %s (preset %s)", d.Name, d.Model))
			Eventually(func() error {
				ForgetNamespaceAuthHeaders(d.Namespace)
				return CheckChatSuccess(ctx, gateway, d.Name)
			}, InferenceSetReadyTimeout, 10*time.Second).Should(Succeed(),
				"gateway should route to deployment %s successfully", d.Name)
		}
	}
}

// TeardownInferenceSetsWithRouting uninstalls the modeldeployment Helm
// releases for every deployment, removing the InferenceSets, InferencePools,
// EPP artifacts, and HTTPRoutes. Entries with an empty Namespace fall back
// to the supplied namespace argument.
func TeardownInferenceSetsWithRouting(deployments []deploy.ModelDeploymentValues, namespace string) {
	if err := uninstallDeployments(context.Background(), deployments, namespace); err != nil {
		GinkgoWriter.Printf("Cleanup warning: %v\n", err)
	}
}

// CleanupDeploymentsAndNamespace attempts every deployment uninstall and then
// tears down the supplied namespace, collecting errors without skipping steps.
func CleanupDeploymentsAndNamespace(ctx context.Context, deployments []deploy.ModelDeploymentValues, namespace string) error {
	deploymentErr := uninstallDeployments(ctx, deployments, namespace)
	namespaceErr := DeleteNamespace(ctx, namespace)
	return errors.Join(deploymentErr, namespaceErr)
}

func uninstallDeployments(ctx context.Context, deployments []deploy.ModelDeploymentValues, namespace string) error {
	var cleanupErrors []error
	for _, d := range deployments {
		ns := d.Namespace
		if ns == "" {
			ns = namespace
		}
		if err := UninstallModelDeployment(ctx, d.Name, ns); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("uninstall modeldeployment %s/%s: %w", ns, d.Name, err))
		}
	}
	return errors.Join(cleanupErrors...)
}
