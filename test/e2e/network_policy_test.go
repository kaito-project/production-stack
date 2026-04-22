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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/kaito-project/production-stack/test/e2e/utils"
)

const (
	netpolTestNamespace = "default"
	serverPodName       = "netpol-server"
	serverPort          = 8080
	probeTimeout        = 10 * time.Second
)

var _ = Describe("Network Policy", utils.GinkgoLabelNetworkPolicy, func() {
	var (
		ctx       context.Context
		clientset *kubernetes.Clientset
		serverIP  string
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		clientset, err = utils.GetK8sClientset()
		Expect(err).NotTo(HaveOccurred(), "failed to create k8s clientset")

		// Clean up any leftover server pod from a previous run.
		_ = clientset.CoreV1().Pods(netpolTestNamespace).Delete(ctx, serverPodName, metav1.DeleteOptions{})
		// Wait for the old pod to be fully removed before creating a new one.
		Eventually(func() bool {
			_, err := clientset.CoreV1().Pods(netpolTestNamespace).Get(ctx, serverPodName, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, utils.PollTimeout, utils.PollInterval).Should(BeTrue(), "old server pod did not terminate")

		// Deploy a simple TCP server pod in the default namespace.
		serverPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serverPodName,
				Namespace: netpolTestNamespace,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:    "server",
					Image:   "busybox:1.36",
					Command: []string{"sh", "-c", fmt.Sprintf("nc -lk -p %d -e echo pong", serverPort)},
				}},
			},
		}
		_, err = clientset.CoreV1().Pods(netpolTestNamespace).Create(ctx, serverPod, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred(), "failed to create server pod")

		// Wait for the server pod to be ready.
		err = utils.WaitForPodReady(ctx, clientset, netpolTestNamespace, serverPodName, utils.PollTimeout)
		Expect(err).NotTo(HaveOccurred(), "server pod did not become ready")

		// Get the server pod IP.
		pod, err := clientset.CoreV1().Pods(netpolTestNamespace).Get(ctx, serverPodName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		serverIP = pod.Status.PodIP
		Expect(serverIP).NotTo(BeEmpty(), "server pod has no IP")
	})

	AfterEach(func() {
		// Clean up: server pod and any probe namespaces.
		_ = clientset.CoreV1().Pods(netpolTestNamespace).Delete(ctx, serverPodName, metav1.DeleteOptions{})
		for _, ns := range []string{"external-ns", "random-ns"} {
			_ = clientset.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
		}
	})

	probe := func(namespace string) bool {
		// Ensure namespace exists.
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		_, _ = clientset.CoreV1().Namespaces().Create(ctx, nsObj, metav1.CreateOptions{})

		probePodName := "netpol-probe"
		probePod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      probePodName,
				Namespace: namespace,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:    "probe",
					Image:   "busybox:1.36",
					Command: []string{"sh", "-c", "sleep 3600"},
				}},
			},
		}
		_, err := clientset.CoreV1().Pods(namespace).Create(ctx, probePod, metav1.CreateOptions{})
		if err != nil {
			// Pod might already exist from a previous run; continue.
			GinkgoLogr.Info("probe pod create", "err", err)
		}
		Expect(utils.WaitForPodReady(ctx, clientset, namespace, probePodName, utils.PollTimeout)).
			To(Succeed(), "probe pod in %s did not become ready", namespace)

		defer func() {
			_ = clientset.CoreV1().Pods(namespace).Delete(ctx, probePodName, metav1.DeleteOptions{})
		}()

		// Exec into the probe pod and try to connect to the server.
		cmd := []string{"sh", "-c", fmt.Sprintf("echo test | nc -w 3 %s %d", serverIP, serverPort)}

		restCfg, err := utils.GetK8sConfig()
		Expect(err).NotTo(HaveOccurred())

		req := clientset.CoreV1().RESTClient().Post().
			Resource("pods").
			Name(probePodName).
			Namespace(namespace).
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

	It("should DENY ingress from an external namespace", func() {
		Expect(probe("external-ns")).To(BeFalse(),
			"traffic from external-ns should be blocked by default-deny-ingress")
	})

	It("should ALLOW ingress from within the default namespace", func() {
		Expect(probe("default")).To(BeTrue(),
			"intra-namespace traffic should be allowed by allow-inference-traffic")
	})

	It("should ALLOW ingress from istio-system namespace", func() {
		Expect(probe("istio-system")).To(BeTrue(),
			"traffic from istio-system should be allowed by allow-inference-traffic")
	})

	It("should DENY ingress from a random namespace", func() {
		Expect(probe("random-ns")).To(BeFalse(),
			"traffic from random-ns should be blocked by default-deny-ingress")
	})
})
