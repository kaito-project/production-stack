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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive // Ginkgo DSL
	. "github.com/onsi/gomega"    //nolint:revive // Gomega DSL
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// EnsureNamespace creates the namespace if it does not exist and installs
// the modelharness Helm chart into it. modelharness owns every per-namespace
// shared resource: the Istio Gateway (named "<name>-gw" by chart
// default), the catch-all `model-not-found-direct` EnvoyFilter (Envoy
// `direct_response` returning 404 + OpenAI-compatible JSON for any
// request not matched by a deployment-specific HTTPRoute),
// — when authEnabled is true — the AuthorizationPolicy + APIKey CR
// that wire the Gateway into the cluster-wide apikey-ext-authz CUSTOM
// provider, and the default-deny-ingress + allow-inference-traffic
// NetworkPolicies that lock down East-West ingress while keeping the
// per-namespace gateway pod reachable from outside the namespace
// (matched via the standard `gateway.networking.k8s.io/gateway-name`
// label that Istio stamps on every gateway pod). The chart-default
// `allowedIngressNamespaces` (currently keda + kaito-system) covers
// the control-plane scrapers — `keda-kaito-scaler` and the
// gpu-node-mocker / kaito-workspace controllers — that need to reach
// shadow pods directly from outside the workload namespace.
//
// Safe to call repeatedly; the underlying `helm upgrade --install` and
// namespace Create are both idempotent.
func EnsureNamespace(ctx context.Context, name string, authEnabled bool) error {
	GetClusterClient(TestingCluster)
	cl := TestingCluster.KubeClient
	// Stamp the namespace-discovery label the productionstack-status-reporter
	// selects on. modelharness is installed directly into the workload
	// namespace (release namespace == workload namespace), so Helm owns the
	// Namespace's lifecycle and the installer — here, the E2E harness —
	// stamps the discovery label at creation time, mirroring what an
	// operator's onboarding flow does in production. See
	// charts/modelharness/templates/namespace.yaml for the rationale.
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"productionstack.kaito.sh/managed-by": "modelharness",
			},
		},
	}
	if err := cl.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %s: %w", name, err)
	}

	if err := InstallModelHarness(name, authEnabled); err != nil {
		return fmt.Errorf("install modelharness in %s: %w", name, err)
	}

	return nil
}

// DeleteNamespace uninstalls the modelharness Helm release in the namespace
// and then deletes the namespace itself. The release is uninstalled before
// the namespace cascade so helm release metadata stays consistent.
func DeleteNamespace(ctx context.Context, name string) error {
	// Kill any cached kubectl port-forwards targeting this namespace
	// before the namespace is gone, so subsequent EnsurePortForwards()
	// healthchecks don't try to restart a forward against a vanished
	// namespace (which surfaces as a 90s readiness timeout).
	RemovePortForwardsForNamespace(name)
	GetClusterClient(TestingCluster)
	cl := TestingCluster.KubeClient
	if err := UninstallModelHarness(name); err != nil {
		return fmt.Errorf("uninstall modelharness from %s: %w", name, err)
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

	// Single dynamic client shared across all deployments in this batch.
	// Created once here rather than inside the per-deployment loop so that
	// cases with multiple deployments (e.g. CaseModelRouting) don't pay the
	// connection overhead on every iteration.
	dynClient, dynErr := GetDynamicClient()
	Expect(dynErr).NotTo(HaveOccurred())

	for _, d := range resolved {
		podTimeout := InferencePodReadyTimeout
		if d.PodReadyTimeout > 0 {
			podTimeout = d.PodReadyTimeout
		}

		// Fast-fail helper for known cloud-side provisioning failures so
		// we don't burn the full pod timeout on unrecoverable conditions.
		checkNodeClaimFailure := func() error {
			ncList, err := dynClient.Resource(NodeClaimGVR).List(ctx, metav1.ListOptions{})
			if err != nil || ncList == nil {
				return nil
			}
			for _, nc := range ncList.Items {
				name := nc.GetName()
				if !strings.HasPrefix(name, d.Namespace+"-") {
					continue
				}
				if !strings.Contains(name, "-"+d.Name+"-") {
					continue
				}

				conds, found, _ := unstructured.NestedSlice(nc.Object, "status", "conditions")
				if !found {
					continue
				}
				for _, c := range conds {
					m, ok := c.(map[string]interface{})
					if !ok {
						continue
					}
					typ := fmt.Sprint(m["type"])
					status := fmt.Sprint(m["status"])
					reason := fmt.Sprint(m["reason"])
					message := fmt.Sprint(m["message"])

					if status == "False" && (typ == "Launched" || typ == "Initialized" || typ == "Registered" || typ == "Ready") {
						return fmt.Errorf("NodeClaim %s condition %s=False (reason=%s): %s", name, typ, reason, message)
					}

					lower := strings.ToLower(reason + " " + message)
					if strings.Contains(lower, "quota") ||
						strings.Contains(lower, "insufficient") ||
						strings.Contains(lower, "notavailableforsubscription") ||
						strings.Contains(lower, "overconstrained") ||
						strings.Contains(lower, "capacity") {
						return fmt.Errorf("NodeClaim %s provisioning blocked (reason=%s): %s", name, reason, message)
					}
				}
			}
			return nil
		}

		// Fast-fail: KAITO must either (a) create an inference pod directly
		// (GPU-mocker mode — pods appear immediately) or (b) create a new
		// Karpenter NodeClaim (karpenter mode — KAITO requests a GPU node
		// before creating the pod; NodeClaim appears within ~1 min).
		// Comparing names rather than count avoids false negatives when
		// unrelated NodeClaims from previous tests are being concurrently
		// garbage-collected.
		By(fmt.Sprintf("Waiting for KAITO to start provisioning for %s", d.Name))
		ncListBefore, _ := dynClient.Resource(NodeClaimGVR).List(ctx, metav1.ListOptions{})
		beforeNCNames := map[string]struct{}{}
		if ncListBefore != nil {
			for _, nc := range ncListBefore.Items {
				beforeNCNames[nc.GetName()] = struct{}{}
			}
		}
		Eventually(func() error {
			// Path (a): pod already exists (GPU-mocker, fake node was ready).
			pods, _ := clientset.CoreV1().Pods(d.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", d.Name),
			})
			if pods != nil && len(pods.Items) > 0 {
				return nil
			}
			// Path (a.1): workspace exists for this deployment. KAITO may have
			// reconciled the InferenceSet and created the Workspace before a new
			// NodeClaim or inference pod is visible.
			wsList, _ := dynClient.Resource(WorkspaceGVR).Namespace(d.Namespace).List(ctx, metav1.ListOptions{})
			if wsList != nil {
				for _, ws := range wsList.Items {
					labels := ws.GetLabels()
					if labels != nil && labels["inferenceset.kaito.sh/created-by"] == d.Name {
						return nil
					}
					if strings.HasPrefix(ws.GetName(), d.Name+"-") {
						return nil
					}
				}
			}
			// Path (b): new NodeClaim appeared (Karpenter mode).
			ncList, _ := dynClient.Resource(NodeClaimGVR).List(ctx, metav1.ListOptions{})
			if ncList != nil {
				for _, nc := range ncList.Items {
					if _, existed := beforeNCNames[nc.GetName()]; !existed {
						// Only count NodeClaims in our namespace (KAITO names
						// them "<namespace>-...") to avoid false triggers from
						// other parallel cases creating their own NodeClaims.
						if !strings.HasPrefix(nc.GetName(), d.Namespace+"-") {
							continue
						}
						GinkgoWriter.Printf("[%s] new Karpenter NodeClaim %q; waiting for node + pod (timeout=%s)\n",
							d.Name, nc.GetName(), podTimeout)
						return nil
					}
				}
			}
			allPods, _ := clientset.CoreV1().Pods(d.Namespace).List(ctx, metav1.ListOptions{})
			if allPods != nil && len(allPods.Items) > 0 {
				GinkgoWriter.Printf("[diag] pods in %s (any label): ", d.Namespace)
				for _, p := range allPods.Items {
					GinkgoWriter.Printf("%s(%s) ", p.Name, p.Status.Phase)
				}
				GinkgoWriter.Printf("\n")
			}
			isObj, _ := dynClient.Resource(InferenceSetGVR).Namespace(d.Namespace).Get(ctx, d.Name, metav1.GetOptions{})
			if isObj != nil {
				if conds, found, _ := unstructured.NestedSlice(isObj.Object, "status", "conditions"); found {
					parts := make([]string, 0, len(conds))
					for _, c := range conds {
						m, ok := c.(map[string]interface{})
						if !ok {
							continue
						}
						parts = append(parts, fmt.Sprintf("%v=%v(%v)", m["type"], m["status"], m["reason"]))
					}
					if len(parts) > 0 {
						return fmt.Errorf("no new NodeClaim/pod/workspace yet; InferenceSet conditions: %s", strings.Join(parts, ", "))
					}
				}
			}
			return fmt.Errorf("no new NodeClaim/pod/workspace yet (KAITO may still be reconciling InferenceSet)")
		}, 10*time.Minute, 15*time.Second).Should(Succeed(),
			"KAITO must start provisioning for %s within 10 minutes.\n"+
				"On a fresh CI cluster the KAITO workspace controller may need\n"+
				"several minutes to initialize. Causes: KAITO not installed with\n"+
				"nodeProvisioner=karpenter, preset %q unsupported, HuggingFace\n"+
				"token missing in %s, or NodePool lacks %q",
			d.Name, d.Model, d.Namespace, d.InstanceType)

		By(fmt.Sprintf("Waiting for inference pods for %s to be Running (timeout=%s)", d.Name, podTimeout))
		Eventually(func() error {
			if err := checkNodeClaimFailure(); err != nil {
				return err
			}

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
					// Emit pod events to help diagnose why it is stuck Pending.
					events, _ := clientset.CoreV1().Events(d.Namespace).List(ctx, metav1.ListOptions{
						FieldSelector: fmt.Sprintf("involvedObject.name=%s", pod.Name),
					})
					if events != nil && len(events.Items) > 0 {
						latest := events.Items[len(events.Items)-1]
						GinkgoWriter.Printf("[diag] pod %s (%s) last event: %s — %s\n",
							pod.Name, pod.Status.Phase, latest.Reason, latest.Message)
					}
					return fmt.Errorf("pod %s is %s, not Running", pod.Name, pod.Status.Phase)
				}
				if pod.Status.PodIP == "" {
					return fmt.Errorf("pod %s has no PodIP yet", pod.Name)
				}
			}
			return nil
		}, podTimeout, 10*time.Second).Should(Succeed(),
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
			gatewayTimeout := GatewayReadyTimeout
			if d.GatewayWarmupTimeout > 0 {
				gatewayTimeout = d.GatewayWarmupTimeout
			}
			// When the deployment opts in to API key auth, the chart
			// renders an APIKey CR; the apikey-operator generates a
			// Secret named APIKeySecretName in the same namespace. The
			// authz service resolves the namespace from the Host
			// header subdomain (<ns>.gw.example.com).
			var (
				bearerToken string
				hostHeader  string
			)
			if d.AuthAPIKeyEnabled {
				By(fmt.Sprintf("Waiting for API key Secret in %s for deployment %s", d.Namespace, d.Name))
				Eventually(func() (string, error) {
					return GetAPIKeyFromSecret(ctx, d.Namespace)
				}, 60*time.Second, 2*time.Second).ShouldNot(BeEmpty(),
					"API key Secret should be created in %s", d.Namespace)
				key, err := GetAPIKeyFromSecret(ctx, d.Namespace)
				Expect(err).NotTo(HaveOccurred())
				bearerToken = key
				hostHeader = d.Namespace + ".gw.example.com"
				// Give Envoy a moment to pick up the AuthorizationPolicy.
				time.Sleep(5 * time.Second)
				// DEBUG: surface the bearer prefix + host so failed-warmup
				// triage can correlate the request with what apikey-authz
				// indexed for this namespace.
				keyPrefix := bearerToken
				if len(keyPrefix) > 8 {
					keyPrefix = keyPrefix[:8]
				}
				By(fmt.Sprintf("DEBUG warmup auth ns=%s deployment=%s host=%s bearerPrefix=%s bearerLen=%d",
					d.Namespace, d.Name, hostHeader, keyPrefix, len(bearerToken)))
			}
			By(fmt.Sprintf("Waiting for gateway routing to be ready for deployment %s (preset %s)", d.Name, d.Model))
			Eventually(func() error {
				var (
					resp *http.Response
					err  error
				)
				if d.AuthAPIKeyEnabled {
					// Re-read the Secret on each retry so we don't cache
					// a stale bearer that the apikey-operator has since
					// rotated (the operator regenerates the Secret if its
					// KEYID drifts from the APIKey CR — see operator
					// "Secret not found, will regenerate" reconciles).
					freshKey, kerr := GetAPIKeyFromSecret(ctx, d.Namespace)
					if kerr != nil {
						return fmt.Errorf("re-read API key for %s: %w", d.Namespace, kerr)
					}
					bearerToken = freshKey
					resp, err = SendChatCompletionWithAuth(gatewayURL, d.Name, "hello", bearerToken, hostHeader)
				} else {
					resp, err = SendChatCompletion(gatewayURL, d.Name)
				}
				if err != nil {
					return fmt.Errorf("request failed: %w", err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					body, _ := ReadResponseBody(resp)
					return fmt.Errorf("expected 200, got %d (ns=%s deployment=%s host=%q authEnabled=%v): %s",
						resp.StatusCode, d.Namespace, d.Name, hostHeader, d.AuthAPIKeyEnabled, string(body))
				}
				return nil
			}, gatewayTimeout, 10*time.Second).Should(Succeed(),
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
