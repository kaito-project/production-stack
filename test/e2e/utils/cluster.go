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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

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
func ScaleDeployment(ctx context.Context, namespace, name string, replicas int32) error {
	cs, err := GetK8sClientset()
	if err != nil {
		return err
	}
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
// least `want` ready replicas, or the timeout elapses.
func WaitForDeploymentReplicas(ctx context.Context, namespace, name string, want int32, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, ready, err := GetDeploymentReplicas(ctx, namespace, name)
		if err == nil && ready >= want {
			return nil
		}
		time.Sleep(PollInterval)
	}
	return fmt.Errorf("timed out waiting for %s/%s to have >= %d ready replicas", namespace, name, want)
}
