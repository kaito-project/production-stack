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
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

const (
	netpolModelName = "falcon-7b-instruct"
	probeTimeout    = 10 * time.Second
)

var _ = Describe("Network Policy", utils.GinkgoLabelNetworkPolicy, Ordered, func() {
	var (
		ctx             context.Context
		clientset       *kubernetes.Clientset
		namespace       string
		serverIP        string
		serverPort      int32
		probeNamespaces []string
	)

	BeforeAll(func() {
		ctx = context.Background()
		utils.GetClusterClient(utils.TestingCluster)
		cl := utils.TestingCluster.KubeClient

		var err error
		clientset, err = utils.GetK8sClientset()
		Expect(err).NotTo(HaveOccurred(), "failed to create k8s clientset")

		// Create a dynamic namespace for this test run.
		namespace = fmt.Sprintf("e2e-netpol-%d", rand.Intn(900000)+100000)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(cl.Create(ctx, ns)).To(Succeed(), "failed to create namespace %s", namespace)

		// Deploy InferenceSet with routing.
		cfg := utils.DefaultInferenceSetConfig(netpolModelName)
		cfg.Namespace = namespace
		Expect(utils.CreateInferenceSetWithRouting(ctx, cl, cfg)).To(Succeed(),
			"failed to create InferenceSet with routing in %s", namespace)

		// Deploy network policies into the model namespace.
		Expect(utils.CreateNetworkPoliciesForNamespace(ctx, cl, namespace)).To(Succeed(),
			"failed to create network policies in %s", namespace)

		// Wait for a model pod to be ready and get its IP.
		Eventually(func() (string, error) {
			pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", netpolModelName),
			})
			if err != nil {
				return "", err
			}
			for _, pod := range pods.Items {
				for _, cond := range pod.Status.Conditions {
					if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
						return pod.Status.PodIP, nil
					}
				}
			}
			return "", fmt.Errorf("no ready model pods found")
		}, utils.InferenceSetReadyTimeout, utils.PollInterval).ShouldNot(BeEmpty(),
			"model pod did not become ready in %s", namespace)

		// Capture the model pod IP.
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", netpolModelName),
		})
		Expect(err).NotTo(HaveOccurred())
		for _, pod := range pods.Items {
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					serverIP = pod.Status.PodIP
					break
				}
			}
			if serverIP != "" {
				break
			}
		}
		Expect(serverIP).NotTo(BeEmpty(), "could not find a ready model pod IP")

		// Get the serving port from the pod spec.
		for _, pod := range pods.Items {
			for _, c := range pod.Spec.Containers {
				for _, p := range c.Ports {
					if p.ContainerPort > 0 {
						serverPort = p.ContainerPort
						break
					}
				}
				if serverPort > 0 {
					break
				}
			}
			if serverPort > 0 {
				break
			}
		}
		Expect(serverPort).To(BeNumerically(">", 0), "could not determine model pod serving port")
	})

	AfterAll(func() {
		cl := utils.TestingCluster.KubeClient

		// Clean up routing and InferenceSet.
		_ = utils.CleanupInferenceSetWithRouting(ctx, cl, netpolModelName, namespace)

		// Clean up network policies.
		_ = utils.CleanupNetworkPolicies(ctx, cl, namespace)

		// Clean up probe namespaces.
		for _, ns := range probeNamespaces {
			_ = clientset.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
		}

		// Delete the test namespace.
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		_ = cl.Delete(ctx, nsObj)
	})

	// probeTarget launches a busybox pod in probeNS and tries to nc to targetIP:targetPort.
	probeTarget := func(probeNS string, targetIP string, targetPort int32) bool {
		// Track probe namespaces for cleanup (skip pre-existing ones).
		if probeNS != namespace && probeNS != "istio-system" && probeNS != "kube-system" && probeNS != "default" {
			probeNamespaces = append(probeNamespaces, probeNS)
		}

		// Ensure namespace exists.
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: probeNS}}
		_, _ = clientset.CoreV1().Namespaces().Create(ctx, nsObj, metav1.CreateOptions{})

		probePodName := "netpol-probe"
		probePod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      probePodName,
				Namespace: probeNS,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:    "probe",
					Image:   "busybox:1.36",
					Command: []string{"sh", "-c", "sleep 3600"},
				}},
			},
		}
		_, err := clientset.CoreV1().Pods(probeNS).Create(ctx, probePod, metav1.CreateOptions{})
		if err != nil {
			GinkgoLogr.Info("probe pod create", "err", err)
		}
		Expect(utils.WaitForPodReady(ctx, clientset, probeNS, probePodName, utils.PollTimeout)).
			To(Succeed(), "probe pod in %s did not become ready", probeNS)

		defer func() {
			_ = clientset.CoreV1().Pods(probeNS).Delete(ctx, probePodName, metav1.DeleteOptions{})
		}()

		// Exec into the probe pod and try to connect to the target.
		cmd := []string{"sh", "-c", fmt.Sprintf("echo test | nc -w 3 %s %d", targetIP, targetPort)}

		restCfg, err := utils.GetK8sConfig()
		Expect(err).NotTo(HaveOccurred())

		req := clientset.CoreV1().RESTClient().Post().
			Resource("pods").
			Name(probePodName).
			Namespace(probeNS).
			SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Command: cmd,
				Stdout:  true,
				Stderr:  true,
			}, scheme.ParameterCodec)

		exec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", req.URL())
		Expect(err).NotTo(HaveOccurred())

		var stdout, stderr bytes.Buffer
		execCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()

		err = exec.StreamWithContext(execCtx, remotecommand.StreamOptions{
			Stdout: &stdout,
			Stderr: &stderr,
		})

		return err == nil
	}

	// probe is a convenience wrapper that probes the model pod.
	probe := func(probeNS string) bool {
		return probeTarget(probeNS, serverIP, serverPort)
	}

	It("should DENY ingress from an external namespace", func() {
		Expect(probe("external-ns")).To(BeFalse(),
			"traffic from external-ns should be blocked by default-deny-ingress")
	})

	It("should ALLOW ingress from within the model namespace", func() {
		Expect(probe(namespace)).To(BeTrue(),
			"intra-namespace traffic should be allowed by allow-inference-traffic")
	})

	It("should DENY ingress from a non-gateway pod in default namespace", func() {
		// The policy only allows pods in default with the gateway label,
		// not arbitrary pods.
		Expect(probe("default")).To(BeFalse(),
			"traffic from a non-gateway pod in default should be blocked")
	})

	It("should DENY ingress from istio-system namespace", func() {
		Expect(probe("istio-system")).To(BeFalse(),
			"traffic from istio-system should be blocked — only the gateway pod in default is allowed")
	})

	It("should DENY ingress from a random namespace", func() {
		Expect(probe("random-ns")).To(BeFalse(),
			"traffic from random-ns should be blocked by default-deny-ingress")
	})

	It("should ALLOW ingress from kube-system namespace", func() {
		Expect(probe("kube-system")).To(BeTrue(),
			"traffic from kube-system should be allowed for health checks and DNS")
	})

	It("should ALLOW ingress from the inference gateway via a real request", func() {
		// Send an actual chat completion request through the gateway to prove
		// the gateway pod (default ns, with the gateway label) can reach the
		// model pods through the network policy.
		gatewayURL, err := utils.GetGatewayURL()
		Expect(err).NotTo(HaveOccurred(), "failed to get gateway URL")

		resp, err := utils.SendChatCompletion(gatewayURL, netpolModelName)
		Expect(err).NotTo(HaveOccurred(), "failed to send chat completion through gateway")
		defer resp.Body.Close()

		// A 200 proves the full path worked: gateway pod → BBR → EPP → model pod.
		// If authN/authZ is added to the gateway in the future, this test will
		// need to include valid credentials.
		Expect(resp.StatusCode).To(Equal(200),
			"gateway should be able to reach model pods — got HTTP %d", resp.StatusCode)
	})
})
