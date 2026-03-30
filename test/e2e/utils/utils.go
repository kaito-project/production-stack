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
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	// PollInterval defines the interval time for a poll operation.
	PollInterval = 2 * time.Second
	// PollTimeout defines the time after which the poll operation times out.
	PollTimeout = 120 * time.Second
)

const (
	// E2eNamespace is the base namespace prefix for e2e tests.
	E2eNamespace = "production-stack-e2e"
)

// GetEnv returns the value of the given environment variable.
func GetEnv(envVar string) string {
	env := os.Getenv(envVar)
	if env == "" {
		fmt.Printf("%s is not set or is empty\n", envVar)
	}
	return env
}

// GetK8sConfig returns a Kubernetes REST config, supporting both in-cluster
// and local kubeconfig.
func GetK8sConfig() (*rest.Config, error) {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" && os.Getenv("KUBERNETES_SERVICE_PORT") != "" {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
		}
		return cfg, nil
	}

	kubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}
	return cfg, nil
}

// GetK8sClientset returns a typed Kubernetes clientset.
func GetK8sClientset() (*kubernetes.Clientset, error) {
	cfg, err := GetK8sConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// GetPodLogs retrieves the logs of a specific container in a pod.
func GetPodLogs(coreClient *kubernetes.Clientset, namespace, podName, containerName string) (string, error) {
	options := &corev1.PodLogOptions{}
	if containerName != "" {
		options.Container = containerName
	}
	req := coreClient.CoreV1().Pods(namespace).GetLogs(podName, options)
	logs, err := req.Stream(context.Background())
	if err != nil {
		return "", err
	}
	defer logs.Close()

	buf := new(strings.Builder)
	if _, err = io.Copy(buf, logs); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// PrintPodLogsOnFailure prints logs for all pods matching the given label
// selector in the namespace. Useful in test failure reporting.
func PrintPodLogsOnFailure(namespace, labelSelector string) {
	coreClient, err := GetK8sClientset()
	if err != nil {
		log.Printf("Failed to create core client: %v", err)
		return
	}
	pods, err := coreClient.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		log.Printf("Failed to list pods in %s: %v", namespace, err)
		return
	}
	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			logs, err := GetPodLogs(coreClient, namespace, pod.Name, container.Name)
			if err != nil {
				log.Printf("Failed to get logs from pod %s/%s: %v", pod.Name, container.Name, err)
			} else {
				fmt.Printf("=== Logs: %s/%s/%s ===\n%s\n", namespace, pod.Name, container.Name, logs)
			}
		}
	}
}

// WaitForPodReady waits until the given pod name/namespace is in Ready condition.
func WaitForPodReady(ctx context.Context, cl *kubernetes.Clientset, namespace, podName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod, err := cl.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			time.Sleep(PollInterval)
			continue
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return nil
			}
		}
		time.Sleep(PollInterval)
	}
	return fmt.Errorf("timed out waiting for pod %s/%s to be Ready", namespace, podName)
}

// WaitForDeploymentReady waits until a deployment has at least 1 available replica.
func WaitForDeploymentReady(ctx context.Context, cl *kubernetes.Clientset, namespace, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		deploy, err := cl.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			time.Sleep(PollInterval)
			continue
		}
		if deploy.Status.AvailableReplicas > 0 {
			return nil
		}
		time.Sleep(PollInterval)
	}
	return fmt.Errorf("timed out waiting for deployment %s/%s to be available", namespace, name)
}
