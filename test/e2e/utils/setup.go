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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SetupInferenceSetsWithRouting idempotently creates InferenceSets, waits for
// InferencePools, creates DestinationRules + HTTPRoutes, waits for EPP and
// inference pods to be Running, and optionally verifies the gateway routing
// pipeline is returning HTTP 200 for each model.
//
// Parameters:
//   - modelNames: list of model names to set up
//   - namespace: target namespace (typically "default")
//   - gatewayURL: if non-empty, performs a warm-up request loop per model
//     to wait for the BBR → EPP ext_proc pipeline to be fully ready
func SetupInferenceSetsWithRouting(modelNames []string, namespace, gatewayURL string) {
	ctx := context.Background()
	GetClusterClient(TestingCluster)

	cl := TestingCluster.KubeClient
	for _, model := range modelNames {
		cfg := DefaultInferenceSetConfig(model)
		cfg.Namespace = namespace

		By(fmt.Sprintf("Creating InferenceSet for %s", model))
		err := CreateInferenceSet(ctx, cl, cfg)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred(), "failed to create InferenceSet for %s", model)
		}

		By(fmt.Sprintf("Waiting for InferencePool for %s", model))
		err = WaitForInferenceSetReady(ctx, cl, cfg.Name, cfg.Namespace, InferenceSetReadyTimeout)
		Expect(err).NotTo(HaveOccurred(), "InferenceSet %s not ready", model)

		By(fmt.Sprintf("Creating DestinationRule for %s", model))
		Eventually(func() error {
			err := CreateDestinationRuleForInferenceSet(ctx, cl, cfg.Name, cfg.Namespace)
			if apierrors.IsAlreadyExists(err) {
				return nil
			}
			return err
		}, 1*time.Minute, 5*time.Second).Should(Succeed(),
			"failed to create DestinationRule for %s", model)

		By(fmt.Sprintf("Creating HTTPRoute for %s", model))
		Eventually(func() error {
			err := CreateHTTPRouteForInferenceSet(ctx, cl, cfg.Name, cfg.Namespace, cfg.GatewayName)
			if apierrors.IsAlreadyExists(err) {
				return nil
			}
			return err
		}, 1*time.Minute, 5*time.Second).Should(Succeed(),
			"failed to create HTTPRoute for %s", model)
	}

	// Wait for KAITO to fully reconcile: EPP pods (deployed via
	// HelmRelease), fake nodes, shadow pods, and original pod status
	// patching must all complete before the gateway can route traffic.
	clientset, err := GetK8sClientset()
	Expect(err).NotTo(HaveOccurred())

	for _, model := range modelNames {
		eppName := EPPServiceName(model)
		By(fmt.Sprintf("Waiting for EPP pods for %s to be Running", model))
		Eventually(func() error {
			pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
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
			"EPP pods for %s should be Running", model)
	}

	for _, model := range modelNames {
		By(fmt.Sprintf("Waiting for inference pods for %s to be Running", model))
		Eventually(func() error {
			pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", model),
			})
			if err != nil {
				return fmt.Errorf("failed to list pods: %w", err)
			}
			if len(pods.Items) == 0 {
				return fmt.Errorf("no inference pods found for %s", model)
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
			"inference pods for %s should be Running with PodIPs", model)
	}

	// Wait for the full BBR → EPP ext_proc pipeline to be ready.
	// Pods being Running does not guarantee ext_proc gRPC connections
	// are established; requests may 500 during the warm-up window.
	if gatewayURL != "" {
		for _, model := range modelNames {
			model := model
			By(fmt.Sprintf("Waiting for gateway routing to be ready for %s", model))
			Eventually(func() error {
				resp, err := SendChatCompletion(gatewayURL, model)
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
				"gateway should route to %s successfully", model)
		}
	}
}

// TeardownInferenceSetsWithRouting cleans up InferenceSets and their associated
// routing resources (HTTPRoute, DestinationRule).
func TeardownInferenceSetsWithRouting(modelNames []string, namespace string) {
	ctx := context.Background()
	for _, model := range modelNames {
		By(fmt.Sprintf("Cleaning up InferenceSet with routing for %s", model))
		err := CleanupInferenceSetWithRouting(ctx, TestingCluster.KubeClient, model, namespace)
		if err != nil {
			GinkgoWriter.Printf("Cleanup warning for %s: %v\n", model, err)
		}
	}
}
