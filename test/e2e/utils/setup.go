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
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // Ginkgo DSL
	. "github.com/onsi/gomega"    //nolint:revive // Gomega DSL
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// EnsureNamespace creates the namespace if it does not exist and, when
// the namespace is non-default, provisions all per-namespace e2e
// resources (Gateway, catch-all HTTPRoute, ReferenceGrant). The
// cluster-wide default Gateway and its catch-all are installed
// out-of-band by hack/e2e/scripts/install-components.sh, so we do not
// re-create them here.
//
// Safe to call repeatedly; existing resources are left untouched.
func EnsureNamespace(ctx context.Context, name, gatewayName string) error {
	if name == DefaultGatewayNamespace {
		// The cluster-wide default Gateway is installed by the e2e
		// install script — do not duplicate it here.
		return nil
	}

	GetClusterClient(TestingCluster)
	cl := TestingCluster.KubeClient
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := cl.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %s: %w", name, err)
	}

	if gatewayName == "" {
		return fmt.Errorf("gatewayName must be set for non-default namespace %q", name)
	}

	return provisionNamespaceResources(ctx, name, gatewayName)
}

// provisionNamespaceResources is the single source of truth for every
// resource that a non-default e2e case namespace owns. Add new
// per-namespace resources here so callers stay unchanged.
//
// Currently provisioned:
//  1. Gateway <gatewayName>: per-case Istio Gateway (HTTP/80) used by
//     this namespace's HTTPRoutes and port-forwarded by the test client.
//  2. HTTPRoute model-not-found-route: catch-all routing every otherwise
//     unmatched path on the per-case Gateway to the shared
//     `default/model-not-found` Service so requests for non-existent
//     models return OpenAI-compatible 404 JSON instead of Envoy's bare
//  404. Uses a cross-namespace backendRef.
//  3. ReferenceGrant allow-model-not-found-from-<ns> (in `default`):
//     authorizes the catch-all HTTPRoute in <ns> to reference the
//     `default/model-not-found` Service.
func provisionNamespaceResources(ctx context.Context, name, gatewayName string) error {
	cl := TestingCluster.KubeClient

	// 1. Gateway in the case namespace.
	gw := &unstructured.Unstructured{}
	gw.SetGroupVersionKind(GatewayGVK)
	gw.SetName(gatewayName)
	gw.SetNamespace(name)
	if err := unstructured.SetNestedField(gw.Object, "istio", "spec", "gatewayClassName"); err != nil {
		return fmt.Errorf("set gatewayClassName: %w", err)
	}
	listeners := []interface{}{
		map[string]interface{}{
			"name":     "http",
			"port":     int64(80),
			"protocol": "HTTP",
		},
	}
	if err := unstructured.SetNestedSlice(gw.Object, listeners, "spec", "listeners"); err != nil {
		return fmt.Errorf("set listeners: %w", err)
	}
	if err := cl.Create(ctx, gw); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create Gateway %s/%s: %w", name, gatewayName, err)
	}

	// 2. ReferenceGrant in the default (Service-owning) namespace, named
	// after the consuming namespace so each case fully owns its grant.
	grantName := fmt.Sprintf("allow-model-not-found-from-%s", name)
	rg := &unstructured.Unstructured{}
	rg.SetGroupVersionKind(ReferenceGrantGVK)
	rg.SetName(grantName)
	rg.SetNamespace(DefaultGatewayNamespace)
	if err := unstructured.SetNestedSlice(rg.Object, []interface{}{
		map[string]interface{}{
			"group":     "gateway.networking.k8s.io",
			"kind":      "HTTPRoute",
			"namespace": name,
		},
	}, "spec", "from"); err != nil {
		return fmt.Errorf("set ReferenceGrant.from: %w", err)
	}
	if err := unstructured.SetNestedSlice(rg.Object, []interface{}{
		map[string]interface{}{
			"group": "",
			"kind":  "Service",
			"name":  ModelNotFoundServiceName,
		},
	}, "spec", "to"); err != nil {
		return fmt.Errorf("set ReferenceGrant.to: %w", err)
	}
	if err := cl.Create(ctx, rg); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create ReferenceGrant %s/%s: %w", DefaultGatewayNamespace, grantName, err)
	}

	// 3. Catch-all HTTPRoute in the case namespace, parented to the
	// per-case Gateway, sending unmatched paths to default/model-not-found.
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(HTTPRouteGVK)
	route.SetName("model-not-found-route")
	route.SetNamespace(name)
	if err := unstructured.SetNestedSlice(route.Object, []interface{}{
		map[string]interface{}{
			"group": "gateway.networking.k8s.io",
			"kind":  "Gateway",
			"name":  gatewayName,
		},
	}, "spec", "parentRefs"); err != nil {
		return fmt.Errorf("set parentRefs: %w", err)
	}
	if err := unstructured.SetNestedSlice(route.Object, []interface{}{
		map[string]interface{}{
			"matches": []interface{}{
				map[string]interface{}{
					"path": map[string]interface{}{
						"type":  "PathPrefix",
						"value": "/",
					},
				},
			},
			"backendRefs": []interface{}{
				map[string]interface{}{
					"group":     "",
					"kind":      "Service",
					"name":      ModelNotFoundServiceName,
					"namespace": DefaultGatewayNamespace,
					"port":      int64(80),
				},
			},
		},
	}, "spec", "rules"); err != nil {
		return fmt.Errorf("set rules: %w", err)
	}
	if err := cl.Create(ctx, route); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create HTTPRoute %s/model-not-found-route: %w", name, err)
	}

	return nil
}

// cleanupNamespaceResources removes per-namespace artifacts that live
// outside the case namespace (and therefore are not reaped by the
// namespace cascade). In-namespace resources (Gateway, HTTPRoute) are
// deleted automatically when the namespace is deleted.
func cleanupNamespaceResources(ctx context.Context, name string) error {
	cl := TestingCluster.KubeClient

	grantName := fmt.Sprintf("allow-model-not-found-from-%s", name)
	rg := &unstructured.Unstructured{}
	rg.SetGroupVersionKind(ReferenceGrantGVK)
	rg.SetName(grantName)
	rg.SetNamespace(DefaultGatewayNamespace)
	if err := cl.Delete(ctx, rg); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete ReferenceGrant %s/%s: %w", DefaultGatewayNamespace, grantName, err)
	}
	return nil
}

// DeleteNamespace deletes the given namespace and any out-of-namespace
// per-case artifacts. The cluster-wide default namespace is never
// deleted.
func DeleteNamespace(ctx context.Context, name string) error {
	if name == DefaultGatewayNamespace {
		return nil
	}
	GetClusterClient(TestingCluster)
	cl := TestingCluster.KubeClient
	if err := cleanupNamespaceResources(ctx, name); err != nil {
		return err
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := cl.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete namespace %s: %w", name, err)
	}
	return nil
}

// WaitForGatewayService blocks until the Istio Service backing the named
// Gateway exists AND the gateway Pod has at least one Ready replica, so
// port-forwards started immediately afterwards do not race the
// gateway-controller. Istio creates the Service synchronously when it
// observes the Gateway resource, but the underlying envoy Pod takes
// longer to schedule + become Ready; `kubectl port-forward` to a Service
// with no Ready endpoints hangs until those endpoints appear, which
// causes the 30s port-forward readiness probe to time out.
func WaitForGatewayService(ctx context.Context, namespace, gatewayName string, timeout time.Duration) error {
	if namespace == DefaultGatewayNamespace {
		return nil
	}
	GetClusterClient(TestingCluster)
	cl := TestingCluster.KubeClient
	clientset, err := GetK8sClientset()
	if err != nil {
		return fmt.Errorf("init clientset: %w", err)
	}

	deadline := time.Now().Add(timeout)
	svc := &corev1.Service{}
	svcKey := types.NamespacedName{Namespace: namespace, Name: IstioGatewayServiceName(gatewayName)}
	podSelector := fmt.Sprintf("gateway.networking.k8s.io/gateway-name=%s", gatewayName)

	for time.Now().Before(deadline) {
		if err := cl.Get(ctx, svcKey, svc); err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: podSelector,
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
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("gateway %s/%s did not become ready within %s (service=%s, pod selector=%q)",
		namespace, gatewayName, timeout, svcKey.Name, podSelector)
}

// SetupInferenceSetsWithRouting idempotently installs the modeldeployment
// Helm chart for each entry in deployments, waits for the InferencePool, EPP,
// and inference (shadow) pods to be Running, and optionally verifies that
// the Gateway routing pipeline is returning HTTP 200 for each deployment.
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
//   - gatewayURL: if non-empty, performs a warm-up request loop per
//     deployment to wait for the BBR → EPP ext_proc pipeline to be ready.
func SetupInferenceSetsWithRouting(deployments []ModelDeploymentValues, namespace, gatewayURL string) {
	ctx := context.Background()
	GetClusterClient(TestingCluster)

	cl := TestingCluster.KubeClient

	// Apply namespace default eagerly so subsequent waits use the correct ns.
	resolved := make([]ModelDeploymentValues, len(deployments))
	for i, d := range deployments {
		if d.Namespace == "" {
			d.Namespace = namespace
		}
		resolved[i] = d
	}

	for _, d := range resolved {
		By(fmt.Sprintf("Installing modeldeployment chart %s (model=%s) in %s", d.Name, d.Model, d.Namespace))
		Expect(InstallModelDeployment(d)).To(Succeed(),
			"failed to install modeldeployment chart for %s", d.Name)

		By(fmt.Sprintf("Waiting for InferencePool for %s", d.Name))
		Expect(WaitForInferenceSetReady(ctx, cl, d.Name, d.Namespace, InferenceSetReadyTimeout)).
			To(Succeed(), "InferenceSet %s not ready", d.Name)
	}

	// Wait for KAITO + the chart-rendered EPP Deployment to fully reconcile:
	// EPP pods, fake nodes, shadow pods, and original pod status patching
	// must all complete before the gateway can route traffic.
	clientset, err := GetK8sClientset()
	Expect(err).NotTo(HaveOccurred())

	for _, d := range resolved {
		eppName := EPPServiceName(d.Name)
		By(fmt.Sprintf("Waiting for EPP pods for %s to be Running", d.Name))
		Eventually(func() error {
			pods, err := clientset.CoreV1().Pods(d.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("inferencepool=%s", eppName),
			})
			if err != nil {
				return fmt.Errorf("failed to list EPP pods: %w", err)
			}
			var running int
			for _, pod := range pods.Items {
				if pod.Status.Phase == "Running" {
					running++
				}
			}
			if running < 1 {
				return fmt.Errorf("no running EPP pods for %q (total: %d)", eppName, len(pods.Items))
			}
			return nil
		}, 5*time.Minute, 10*time.Second).Should(Succeed(),
			"EPP pods for %s should be Running", d.Name)
	}

	for _, d := range resolved {
		By(fmt.Sprintf("Waiting for inference pods for %s to be Running", d.Name))
		Eventually(func() error {
			pods, err := clientset.CoreV1().Pods(d.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", d.Name),
			})
			if err != nil {
				return fmt.Errorf("failed to list pods: %w", err)
			}
			if len(pods.Items) == 0 {
				return fmt.Errorf("no inference pods found for %s", d.Name)
			}
			for _, pod := range pods.Items {
				if pod.Status.Phase != "Running" {
					return fmt.Errorf("pod %s is %s, not Running", pod.Name, pod.Status.Phase)
				}
				if pod.Status.PodIP == "" {
					return fmt.Errorf("pod %s has no PodIP yet", pod.Name)
				}
			}
			return nil
		}, 5*time.Minute, 10*time.Second).Should(Succeed(),
			"inference pods for %s should be Running with PodIPs", d.Name)
	}

	// Wait for the full BBR → EPP ext_proc pipeline to be ready.
	// Pods being Running does not guarantee ext_proc gRPC connections
	// are established; requests may 500 during the warm-up window.
	//
	// The HTTPRoute matches X-Gateway-Model-Name against the deployment
	// name (.Values.name in the chart), so the gateway is exercised by
	// sending requests with `"model": "<deploymentName>"`.
	if gatewayURL != "" {
		for _, d := range resolved {
			d := d
			By(fmt.Sprintf("Waiting for gateway routing to be ready for deployment %s (preset %s)", d.Name, d.Model))
			Eventually(func() error {
				resp, err := SendChatCompletion(gatewayURL, d.Name)
				if err != nil {
					return fmt.Errorf("request failed: %w", err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					body, _ := ReadResponseBody(resp)
					return fmt.Errorf("expected 200, got %d: %s", resp.StatusCode, string(body))
				}
				return nil
			}, 5*time.Minute, 10*time.Second).Should(Succeed(),
				"gateway should route to deployment %s successfully", d.Name)
		}
	}
}

// TeardownInferenceSetsWithRouting uninstalls the modeldeployment Helm
// releases for every deployment, removing the InferenceSets, InferencePools,
// EPP artifacts, and HTTPRoutes. Entries with an empty Namespace fall back
// to the supplied namespace argument.
func TeardownInferenceSetsWithRouting(deployments []ModelDeploymentValues, namespace string) {
	for _, d := range deployments {
		ns := d.Namespace
		if ns == "" {
			ns = namespace
		}
		By(fmt.Sprintf("Uninstalling modeldeployment chart for %s in %s", d.Name, ns))
		if err := UninstallModelDeployment(d.Name, ns); err != nil {
			GinkgoWriter.Printf("Cleanup warning for %s: %v\n", d.Name, err)
		}
	}
}
