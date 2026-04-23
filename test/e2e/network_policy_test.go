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
	netpolModelNameA = "netpol-a"           // unique name to avoid KAITO workspace collision with other test suites
	netpolModelNameB = "netpol-b"           // different name for namespace B isolation test
	netpolPreset     = "falcon-7b-instruct"    // underlying model preset shared by both
	probeTimeout     = 10 * time.Second
)

var _ = Describe("Network Policy", utils.GinkgoLabelNetworkPolicy, Ordered, func() {
	var (
		ctx             context.Context
		clientset       *kubernetes.Clientset
		namespace       string
		namespaceB      string
		serverIP        string
		serverPort      int32
		serverIPB       string
		serverPortB     int32
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
		cfg := utils.DefaultInferenceSetConfig(netpolModelNameA)
		cfg.Namespace = namespace
		cfg.PresetName = netpolPreset
		Expect(utils.CreateInferenceSetWithRouting(ctx, cl, cfg)).To(Succeed(),
			"failed to create InferenceSet with routing in %s", namespace)

		// Deploy network policies into the model namespace.
		Expect(utils.CreateNetworkPoliciesForNamespace(ctx, cl, namespace)).To(Succeed(),
			"failed to create network policies in %s", namespace)

		// Wait for a model pod to be ready and get its IP.
		Eventually(func() (string, error) {
			pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", netpolModelNameA),
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
			LabelSelector: fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", netpolModelNameA),
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

		// Deploy a second model namespace (namespace B) to test cross-namespace isolation.
		namespaceB = fmt.Sprintf("e2e-netpol-%d", rand.Intn(900000)+100000)
		nsB := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceB}}
		Expect(cl.Create(ctx, nsB)).To(Succeed(), "failed to create namespace %s", namespaceB)

		cfgB := utils.DefaultInferenceSetConfig(netpolModelNameB)
		cfgB.Namespace = namespaceB
		cfgB.PresetName = netpolPreset // same underlying model preset
		Expect(utils.CreateInferenceSetWithRouting(ctx, cl, cfgB)).To(Succeed(),
			"failed to create InferenceSet with routing in %s", namespaceB)

		Expect(utils.CreateNetworkPoliciesForNamespace(ctx, cl, namespaceB)).To(Succeed(),
			"failed to create network policies in %s", namespaceB)

		// Wait for model pod in namespace B.
		Eventually(func() (string, error) {
			podsB, err := clientset.CoreV1().Pods(namespaceB).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", netpolModelNameB),
			})
			if err != nil {
				return "", err
			}
			for _, pod := range podsB.Items {
				for _, cond := range pod.Status.Conditions {
					if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
						return pod.Status.PodIP, nil
					}
				}
			}
			return "", fmt.Errorf("no ready model pods found")
		}, utils.InferenceSetReadyTimeout, utils.PollInterval).ShouldNot(BeEmpty(),
			"model pod did not become ready in %s", namespaceB)

		podsB, err := clientset.CoreV1().Pods(namespaceB).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", netpolModelNameB),
		})
		Expect(err).NotTo(HaveOccurred())
		for _, pod := range podsB.Items {
			for _, c := range pod.Spec.Containers {
				for _, p := range c.Ports {
					if p.ContainerPort > 0 {
						serverPortB = p.ContainerPort
						break
					}
				}
				if serverPortB > 0 {
					break
				}
			}
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					serverIPB = pod.Status.PodIP
					break
				}
			}
			if serverIPB != "" && serverPortB > 0 {
				break
			}
		}
		Expect(serverIPB).NotTo(BeEmpty(), "could not find a ready model pod IP in namespace B")
		Expect(serverPortB).To(BeNumerically(">", 0), "could not determine model pod serving port in namespace B")
	})

	AfterAll(func() {
		cl := utils.TestingCluster.KubeClient

		// Clean up routing and InferenceSet.
		_ = utils.CleanupInferenceSetWithRouting(ctx, cl, netpolModelNameA, namespace)
		_ = utils.CleanupInferenceSetWithRouting(ctx, cl, netpolModelNameB, namespaceB)

		// Clean up network policies.
		_ = utils.CleanupNetworkPolicies(ctx, cl, namespace)
		_ = utils.CleanupNetworkPolicies(ctx, cl, namespaceB)

		// Clean up probe namespaces.
		for _, ns := range probeNamespaces {
			_ = clientset.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
		}

		// Delete the test namespace.
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		_ = cl.Delete(ctx, nsObj)
		nsBObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceB}}
		_ = cl.Delete(ctx, nsBObj)
	})

	// probeTarget launches a busybox pod in probeNS and execs the given command.
	// It returns the stdout output and any exec error. The caller decides how to
	// interpret the result (e.g. err==nil means connectivity, stdout content for
	// HTTP response validation). Optional labels can be applied to the probe pod.
	probeTarget := func(probeNS string, cmd []string, timeout time.Duration, labels map[string]string) (string, error) {
		// Track probe namespaces for cleanup (skip pre-existing ones).
		if probeNS != namespace && probeNS != "istio-system" && probeNS != "kube-system" && probeNS != "default" {
			probeNamespaces = append(probeNamespaces, probeNS)
		}

		// Ensure namespace exists.
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: probeNS}}
		_, _ = clientset.CoreV1().Namespaces().Create(ctx, nsObj, metav1.CreateOptions{})

		probePodName := fmt.Sprintf("netpol-probe-%d", rand.Intn(900000)+100000)
		probePod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      probePodName,
				Namespace: probeNS,
				Labels:    labels,
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
		execCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		err = exec.StreamWithContext(execCtx, remotecommand.StreamOptions{
			Stdout: &stdout,
			Stderr: &stderr,
		})

		return stdout.String(), err
	}

	// ncCmd builds a netcat TCP connectivity check command.
	ncCmd := func(targetIP string, targetPort int32) []string {
		return []string{"sh", "-c", fmt.Sprintf("echo test | nc -w 3 %s %d", targetIP, targetPort)}
	}

	// wgetCmd builds a wget chat completion POST command. Exit code 0 means HTTP 2xx.
	wgetCmd := func(targetIP string, targetPort int32, model string) []string {
		return []string{"sh", "-c", fmt.Sprintf(
			`wget -q -O /dev/null --header='Content-Type: application/json' `+
				`--post-data='{"model":"%s","messages":[{"role":"user","content":"hello"}],"max_tokens":5}' `+
				`http://%s:%d/v1/chat/completions`,
			model, targetIP, targetPort,
		)}
	}

	// probe is a convenience wrapper that checks TCP connectivity to the model pod.
	probe := func(probeNS string) bool {
		_, err := probeTarget(probeNS, ncCmd(serverIP, serverPort), probeTimeout, nil)
		return err == nil
	}

	It("should DENY ingress from an external namespace", func() {
		Expect(probe("external-ns")).To(BeFalse(),
			"traffic from external-ns should be blocked by default-deny-ingress")
	})

	It("should ALLOW ingress from within the model namespace via a real inference request", func() {
		// Send a real chat completion request via wget from a busybox pod in the
		// model namespace directly to the model pod IP.
		_, err := probeTarget(namespace, wgetCmd(serverIP, serverPort, netpolPreset), 30*time.Second, nil)
		Expect(err).NotTo(HaveOccurred(),
			"intra-namespace inference request should succeed with HTTP 200")
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

	It("should DENY ingress from kube-system namespace", func() {
		Expect(probe("kube-system")).To(BeFalse(),
			"traffic from kube-system should be blocked by default-deny-ingress")
	})

	It("should ALLOW ingress from a pod with the gateway label in default namespace", func() {
		// The network policy allows ingress from pods in the default namespace
		// with the gateway label. This simulates the real gateway pod's connectivity
		// to the model pods without requiring a full inference request through the
		// routing stack.
		gatewayLabels := map[string]string{
			"gateway.networking.k8s.io/gateway-name": "inference-gateway",
		}
		_, err := probeTarget("default", ncCmd(serverIP, serverPort), probeTimeout, gatewayLabels)
		Expect(err).NotTo(HaveOccurred(),
			"a pod with the gateway label in default namespace should be able to reach model pods")
	})

	It("should DENY ingress from workload namespace A to workload namespace B", func() {
		// Each model namespace has its own network policies with intra-namespace
		// allow only. A pod in namespace A should NOT be able to reach model
		// pods in namespace B — this validates tenant isolation between workloads.
		_, err := probeTarget(namespace, ncCmd(serverIPB, serverPortB), probeTimeout, nil)
		Expect(err).To(HaveOccurred(),
			"workload namespace A should not be able to reach model pods in workload namespace B")
	})

	It("should DENY ingress from workload namespace B to workload namespace A", func() {
		// Verify isolation in the reverse direction as well.
		_, err := probeTarget(namespaceB, ncCmd(serverIP, serverPort), probeTimeout, nil)
		Expect(err).To(HaveOccurred(),
			"workload namespace B should not be able to reach model pods in workload namespace A")
	})
})
