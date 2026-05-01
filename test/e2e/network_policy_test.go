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
	probeTimeout = 10 * time.Second
)

var _ = Describe("Network Policy", utils.GinkgoLabelNetworkPolicy, Ordered, func() {
	var (
		ctx             context.Context
		clientset       *kubernetes.Clientset
		namespace       string
		namespaceB      string
		netpolModelA    string
		netpolModelB    string
		serverIP        string
		serverPort      int32
		serverIPB       string
		serverPortB     int32
		probeNamespaces []string
	)

	BeforeAll(func() {
		ctx = context.Background()
		utils.GetClusterClient(utils.TestingCluster)

		var err error
		clientset, err = utils.GetK8sClientset()
		Expect(err).NotTo(HaveOccurred(), "failed to create k8s clientset")

		// Install both workload namespaces (each with default-deny + allow-inference
		// NetworkPolicy pair) via the shared case framework. InstallCase handles
		// the per-namespace Gateway, modeldeployment Helm release, EPP / shadow
		// pod readiness, and gateway routing warmup.
		InstallCase(CaseNetworkPolicyA)
		InstallCase(CaseNetworkPolicyB)

		namespace = CaseNamespace(CaseNetworkPolicyA)
		namespaceB = CaseNamespace(CaseNetworkPolicyB)
		netpolModelA = CaseDeployments[CaseNetworkPolicyA][0].Name
		netpolModelB = CaseDeployments[CaseNetworkPolicyB][0].Name

		// Resolve the model pod IP + port for namespace A.
		serverIP, serverPort = readyModelPodEndpoint(ctx, clientset, namespace, netpolModelA)
		Expect(serverIP).NotTo(BeEmpty(), "could not find a ready model pod IP in %s", namespace)
		Expect(serverPort).To(BeNumerically(">", 0), "could not determine model pod serving port in %s", namespace)

		// Resolve the model pod IP + port for namespace B.
		serverIPB, serverPortB = readyModelPodEndpoint(ctx, clientset, namespaceB, netpolModelB)
		Expect(serverIPB).NotTo(BeEmpty(), "could not find a ready model pod IP in %s", namespaceB)
		Expect(serverPortB).To(BeNumerically(">", 0), "could not determine model pod serving port in %s", namespaceB)

		// Wait for NetworkPolicy enforcement to actually take effect on this
		// cluster. On freshly created Cilium clusters the policy maps may take
		// a few seconds to load even after pods report Ready. Use a single
		// long-lived canary pod in an external namespace and probe with wget,
		// which (unlike `nc -w` on busybox 1.36) sets a meaningful non-zero
		// exit code when the connection is refused/blocked.
		canaryNS := fmt.Sprintf("e2e-netpol-canary-%d", rand.Intn(900000)+100000)
		probeNamespaces = append(probeNamespaces, canaryNS)
		_, _ = clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: canaryNS},
		}, metav1.CreateOptions{})

		canaryPodName := fmt.Sprintf("canary-probe-%d", rand.Intn(900000)+100000)
		_, err = clientset.CoreV1().Pods(canaryNS).Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: canaryPodName, Namespace: canaryNS},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:    "probe",
					Image:   "busybox:1.36",
					Command: []string{"sh", "-c", "sleep 3600"},
				}},
			},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "failed to create canary probe pod")
		Expect(utils.WaitForPodReady(ctx, clientset, canaryNS, canaryPodName, utils.PollTimeout)).
			To(Succeed(), "canary probe pod did not become ready")

		restCfg, err := utils.GetK8sConfig()
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool {
			// `nc -z -w 3` does a pure TCP probe and returns 0 on success,
			// non-zero on refused/blocked/timeout. Echo the exit code to
			// stdout because client-go's StreamWithContext returns nil even
			// when the remote command exits non-zero, so we cannot rely on
			// `err != nil` to mean "blocked".
			cmd := []string{"sh", "-c", fmt.Sprintf(
				"nc -z -w 3 %s %d 2>&1; echo EXIT=$?",
				serverIP, serverPort,
			)}
			req := clientset.CoreV1().RESTClient().Post().
				Resource("pods").Name(canaryPodName).Namespace(canaryNS).
				SubResource("exec").
				VersionedParams(&corev1.PodExecOptions{
					Command: cmd, Stdout: true, Stderr: true,
				}, scheme.ParameterCodec)

			exec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", req.URL())
			if err != nil {
				return false
			}

			var stdout, stderr bytes.Buffer
			execCtx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			_ = exec.StreamWithContext(execCtx, remotecommand.StreamOptions{
				Stdout: &stdout, Stderr: &stderr,
			})
			out := stdout.String() + stderr.String()
			// Enforcement is active when nc cannot establish the TCP handshake
			// from the external canary namespace (any non-zero exit).
			return !bytes.Contains([]byte(out), []byte("EXIT=0"))
		}, 3*time.Minute, 5*time.Second).Should(BeTrue(),
			"timed out waiting for NetworkPolicy enforcement to become active — "+
				"Cilium may not be enforcing policies on this cluster, or the "+
				"allow-inference-traffic rule is too permissive")
	})

	AfterAll(func() {
		// Clean up probe namespaces (canary + any It-block-created probe ns).
		for _, ns := range probeNamespaces {
			_ = clientset.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
		}
		UninstallCase(CaseNetworkPolicyA)
		UninstallCase(CaseNetworkPolicyB)
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

	// ncCmd builds a TCP connectivity check using `nc -z` (zero I/O mode),
	// which is the canonical probe form busybox 1.36's `nc` actually
	// accepts (the bare `nc -w 3 HOST PORT` form is rejected with a usage
	// banner). Exit code 0 ⇒ TCP handshake completed, non-zero ⇒ blocked,
	// refused, or timed out — exactly what NetworkPolicy controls at L3/L4.
	ncCmd := func(targetIP string, targetPort int32) []string {
		return []string{"sh", "-c", fmt.Sprintf("nc -z -w 3 %s %d", targetIP, targetPort)}
	}

	// probe is a convenience wrapper that checks TCP connectivity to the model pod.
	probe := func(probeNS string) bool {
		_, err := probeTarget(probeNS, ncCmd(serverIP, serverPort), probeTimeout, nil)
		return err == nil
	}

	// ── Enforcement baseline ──────────────────────────────────────────────
	// These two tests run first (suite is Ordered) and together prove that
	// NetworkPolicy enforcement is active. If intra-namespace connectivity
	// works but an external namespace is NOT blocked, enforcement is off and
	// every subsequent deny assertion would be a false positive.
	It("baseline: should ALLOW ingress from within the model namespace", func() {
		// NetworkPolicy is L3/L4, so a TCP-level reachability check is the
		// correct signal. Hitting the model pod directly with an
		// HTTP/`/v1/chat/completions` POST would test the EPP+Gateway
		// pipeline (which only the Gateway pod can reach), not the policy.
		_, err := probeTarget(namespace, ncCmd(serverIP, serverPort), probeTimeout, nil)
		Expect(err).NotTo(HaveOccurred(),
			"intra-namespace TCP reach to model pod should succeed — if this fails, NetworkPolicy is over-blocking")
	})

	It("baseline: should DENY ingress from an external namespace (proves enforcement is active)", func() {
		allowed := probe("external-ns")
		Expect(allowed).To(BeFalse(),
			"external namespace reached model pod — NetworkPolicy enforcement is NOT active; "+
				"remaining deny tests are unreliable. Check that the CNI plugin supports "+
				"NetworkPolicy and that policies were applied correctly.")
	})

	// ── Deny tests ────────────────────────────────────────────────────────
	It("should DENY ingress from a non-gateway pod in default namespace", func() {
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

	// ── Allow tests ───────────────────────────────────────────────────────
	It("should ALLOW ingress via gateway-labeled pod in default namespace", func() {
		// The allow-inference-traffic NetworkPolicy permits ingress from
		// pods in `default` carrying the inference-gateway label. Verify at
		// L4 — the policy decision is independent of what the model pod
		// chooses to serve on that port.
		gatewayLabels := map[string]string{
			"gateway.networking.k8s.io/gateway-name": "inference-gateway",
		}
		_, err := probeTarget("default", ncCmd(serverIP, serverPort), probeTimeout, gatewayLabels)
		Expect(err).NotTo(HaveOccurred(),
			"gateway-labeled pod in default should be allowed to TCP-connect to the model pod")
	})

	// ── Cross-namespace isolation ─────────────────────────────────────────
	It("should DENY ingress from workload namespace A to workload namespace B", func() {
		_, err := probeTarget(namespace, ncCmd(serverIPB, serverPortB), probeTimeout, nil)
		Expect(err).To(HaveOccurred(),
			"workload namespace A should not be able to reach model pods in workload namespace B")
	})

	It("should DENY ingress from workload namespace B to workload namespace A", func() {
		_, err := probeTarget(namespaceB, ncCmd(serverIP, serverPort), probeTimeout, nil)
		Expect(err).To(HaveOccurred(),
			"workload namespace B should not be able to reach model pods in workload namespace A")
	})
})

// readyModelPodEndpoint returns the PodIP and first containerPort of a Ready
// pod owned by the given InferenceSet name in the given namespace. Fails the
// current Ginkgo spec if no Ready pod has appeared within InferenceSetReadyTimeout.
func readyModelPodEndpoint(ctx context.Context, clientset *kubernetes.Clientset, ns, deploymentName string) (string, int32) {
	selector := fmt.Sprintf("inferenceset.kaito.sh/created-by=%s", deploymentName)

	Eventually(func() (string, error) {
		pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
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
		return "", fmt.Errorf("no ready model pods found for %s in %s", deploymentName, ns)
	}, utils.InferenceSetReadyTimeout, utils.PollInterval).ShouldNot(BeEmpty(),
		"model pod did not become ready in %s", ns)

	pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	Expect(err).NotTo(HaveOccurred())

	var (
		ip   string
		port int32
	)
	for _, pod := range pods.Items {
		ready := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			continue
		}
		ip = pod.Status.PodIP
		for _, c := range pod.Spec.Containers {
			for _, p := range c.Ports {
				if p.ContainerPort > 0 {
					port = p.ContainerPort
					break
				}
			}
			if port > 0 {
				break
			}
		}
		if ip != "" && port > 0 {
			break
		}
	}
	return ip, port
}
