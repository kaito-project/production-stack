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

	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

// BBRPodSelector matches the cluster-wide body-based-router pods.
const BBRPodSelector = "app.kubernetes.io/name=body-based-routing"

// BBRNamespace discovers the namespace the cluster-wide body-based-router runs
// in, rather than assuming the umbrella chart's release namespace: a managed
// control plane installs the same workload into its own add-on namespace
// (kube-system on AI Manager) instead of kaito-system.
func BBRNamespace(ctx context.Context) (string, error) {
	clientset, err := GetK8sClientset()
	if err != nil {
		return "", err
	}
	pods, err := clientset.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: BBRPodSelector,
	})
	if err != nil {
		return "", fmt.Errorf("list pods matching %q: %w", BBRPodSelector, err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods match %q in any namespace", BBRPodSelector)
	}
	return pods.Items[0].Namespace, nil
}

var (
	scheme         = runtime.NewScheme()
	TestingCluster = NewCluster(scheme)
)

// Cluster holds the Kubernetes clients needed for e2e tests.
type Cluster struct {
	Scheme     *runtime.Scheme
	KubeClient client.Client
}

// NewCluster creates a new Cluster with the given scheme.
func NewCluster(s *runtime.Scheme) *Cluster {
	return &Cluster{
		Scheme: s,
	}
}

// GetClusterClient initialises the cluster KubeClient from the current
// kubeconfig (in-cluster or ~/.kube/config).
func GetClusterClient(cluster *Cluster) {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))

	restConfig := config.GetConfigOrDie()

	k8sClient, err := client.New(restConfig, client.Options{Scheme: cluster.Scheme})
	gomega.Expect(err).Should(gomega.Succeed(), "Failed to set up Kube Client")

	cluster.KubeClient = k8sClient
}

// ScaleDeployment sets the named Deployment's replica count via the scale
// subresource. It updates spec only and does NOT wait for the rollout to
// converge — use WaitForDeploymentReplicas for that.
//
// Callers MUST carry GinkgoLabelStandardK8sOnly: a managed cluster such as AKS
// Automatic does not allow the suite to reshape running workloads this way.
func ScaleDeployment(ctx context.Context, namespace, name string, replicas int32) error {
	cs, err := GetK8sClientset()
	if err != nil {
		return err
	}
	return scaleDeployment(ctx, cs, namespace, name, replicas)
}

func scaleDeployment(ctx context.Context, cs kubernetes.Interface, namespace, name string, replicas int32) error {
	scale, err := cs.AppsV1().Deployments(namespace).GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get scale %s/%s: %w", namespace, name, err)
	}
	scale.Spec.Replicas = replicas
	if _, err := cs.AppsV1().Deployments(namespace).UpdateScale(ctx, name, scale, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update scale %s/%s to %d: %w", namespace, name, replicas, err)
	}
	return nil
}

// GetDeploymentReplicas returns the desired (spec) and ready replica counts
// for the named Deployment.
func GetDeploymentReplicas(ctx context.Context, namespace, name string) (desired, ready int32, err error) {
	cs, err := GetK8sClientset()
	if err != nil {
		return 0, 0, err
	}
	d, err := cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, 0, err
	}
	desired = int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	return desired, d.Status.ReadyReplicas, nil
}

// WaitForDeploymentReplicas blocks until the named Deployment reports at
// least `want` ready replicas. For zero, it waits for scale-down to complete.
func WaitForDeploymentReplicas(ctx context.Context, namespace, name string, want int32, timeout time.Duration) error {
	clientset, err := GetK8sClientset()
	if err != nil {
		return err
	}
	return waitForDeploymentReplicas(ctx, clientset, namespace, name, want, timeout, want == 0)
}

func waitForDeploymentReplicas(ctx context.Context, clientset kubernetes.Interface, namespace, name string, want int32, timeout time.Duration, exact bool) error {
	return pollUntilReady(ctx, timeout, fmt.Sprintf("deployment %s/%s replicas=%d (exact=%t)", namespace, name, want, exact), func(ctx context.Context) error {
		deployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		return deploymentReplicasReady(deployment, want, exact)
	})
}

func deploymentReplicasReady(deployment *appsv1.Deployment, want int32, exact bool) error {
	if exact {
		if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != want || deployment.Status.ObservedGeneration < deployment.Generation {
			return fmt.Errorf("target replica count has not been observed")
		}
		if deployment.Status.Replicas != want || deployment.Status.ReadyReplicas != want {
			return fmt.Errorf("replicas=%d ready=%d, want exactly %d", deployment.Status.Replicas, deployment.Status.ReadyReplicas, want)
		}
		if deployment.Status.TerminatingReplicas != nil && *deployment.Status.TerminatingReplicas > 0 {
			return fmt.Errorf("%d replicas are still terminating", *deployment.Status.TerminatingReplicas)
		}
	} else if deployment.Status.ReadyReplicas < want {
		return fmt.Errorf("ready replicas=%d, want at least %d", deployment.Status.ReadyReplicas, want)
	}
	return nil
}

// DeploymentReplicaGuard preserves the replica count across fault injection.
// Like ScaleDeployment, callers must carry GinkgoLabelStandardK8sOnly.
type DeploymentReplicaGuard struct {
	clientset        kubernetes.Interface
	namespace        string
	name             string
	originalReplicas int32
}

// NewDeploymentReplicaGuard captures the current desired replica count without mutating it.
func NewDeploymentReplicaGuard(ctx context.Context, namespace, name string) (*DeploymentReplicaGuard, error) {
	clientset, err := GetK8sClientset()
	if err != nil {
		return nil, err
	}
	return newDeploymentReplicaGuard(ctx, clientset, namespace, name)
}

func newDeploymentReplicaGuard(ctx context.Context, clientset kubernetes.Interface, namespace, name string) (*DeploymentReplicaGuard, error) {
	deployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("capture replicas for %s/%s: %w", namespace, name, err)
	}
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}
	return &DeploymentReplicaGuard{clientset: clientset, namespace: namespace, name: name, originalReplicas: replicas}, nil
}

// OriginalReplicas returns the desired count captured before fault injection.
func (guard *DeploymentReplicaGuard) OriginalReplicas() int32 {
	return guard.originalReplicas
}

// ScaleAndWait waits for the exact replica count, including scale-down convergence.
func (guard *DeploymentReplicaGuard) ScaleAndWait(ctx context.Context, replicas int32, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := scaleDeployment(ctx, guard.clientset, guard.namespace, guard.name, replicas); err != nil {
		return err
	}
	return waitForDeploymentReplicas(ctx, guard.clientset, guard.namespace, guard.name, replicas, timeout, true)
}

// Restore reapplies the original count and is safe to retry after any failure.
func (guard *DeploymentReplicaGuard) Restore(ctx context.Context, timeout time.Duration) error {
	return guard.ScaleAndWait(ctx, guard.originalReplicas, timeout)
}
