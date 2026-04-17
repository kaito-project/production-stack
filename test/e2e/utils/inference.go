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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// InferenceSetReadyTimeout is the default timeout for waiting for an
	// InferenceSet to become ready (i.e. the InferencePool is created).
	InferenceSetReadyTimeout = 5 * time.Minute
)

// InferenceSetConfig holds configuration for creating an InferenceSet and its
// associated routing resources (DestinationRule, HTTPRoute).
type InferenceSetConfig struct {
	// Name is the InferenceSet name, also used as the model preset name.
	Name string
	// Namespace is the target namespace.
	Namespace string
	// Replicas is the number of InferenceSet replicas.
	Replicas int64
	// NodeCountLimit is the max number of nodes.
	NodeCountLimit int64
	// InstanceType is the VM instance type for the nodes.
	InstanceType string
	// PresetName is the inference preset name.
	PresetName string
	// GatewayName is the name of the Gateway to associate HTTPRoutes with.
	GatewayName string
}

// DefaultInferenceSetConfig returns an InferenceSetConfig with sensible defaults.
func DefaultInferenceSetConfig(name string) InferenceSetConfig {
	return InferenceSetConfig{
		Name:           name,
		Namespace:      "default",
		Replicas:       2,
		NodeCountLimit: 3,
		InstanceType:   "Standard_NV36ads_A10_v5",
		PresetName:     name,
		GatewayName:    "inference-gateway",
	}
}

// InferencePoolName returns the InferencePool name derived from the InferenceSet name.
func InferencePoolName(inferenceSetName string) string {
	return inferenceSetName + "-inferencepool"
}

// EPPServiceName returns the EPP service name derived from the InferenceSet name.
func EPPServiceName(inferenceSetName string) string {
	return InferencePoolName(inferenceSetName) + "-epp"
}

// CreateInferenceSet creates an InferenceSet custom resource.
func CreateInferenceSet(ctx context.Context, cl client.Client, cfg InferenceSetConfig) error {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kaito.sh/v1alpha1",
			"kind":       "InferenceSet",
			"metadata": map[string]interface{}{
				"name":      cfg.Name,
				"namespace": cfg.Namespace,
				"annotations": map[string]interface{}{
					"scaledobject.kaito.sh/auto-provision": "true",
					"scaledobject.kaito.sh/metricName":     "vllm:num_requests_waiting",
					"scaledobject.kaito.sh/threshold":      "10",
				},
			},
			"spec": map[string]interface{}{
				"replicas":       cfg.Replicas,
				"nodeCountLimit": cfg.NodeCountLimit,
				"labelSelector": map[string]interface{}{
					"matchLabels": map[string]interface{}{
						"apps": cfg.Name,
					},
				},
				"template": map[string]interface{}{
					"resource": map[string]interface{}{
						"instanceType": cfg.InstanceType,
					},
					"inference": map[string]interface{}{
						"preset": map[string]interface{}{
							"name": cfg.PresetName,
						},
					},
				},
			},
		},
	}
	return cl.Create(ctx, obj)
}

// WaitForInferenceSetReady waits for the InferenceSet's associated InferencePool
// to be created, indicating KAITO has processed the InferenceSet.
func WaitForInferenceSetReady(ctx context.Context, cl client.Client, name, namespace string, timeout time.Duration) error {
	poolName := InferencePoolName(name)
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		pool := &unstructured.Unstructured{}
		pool.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "inference.networking.k8s.io",
			Version: "v1",
			Kind:    "InferencePool",
		})
		err := cl.Get(ctx, types.NamespacedName{Name: poolName, Namespace: namespace}, pool)
		if err == nil {
			return nil
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("error checking InferencePool %s/%s: %w", namespace, poolName, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(PollInterval):
		}
	}
	return fmt.Errorf("timed out waiting for InferencePool %s/%s to be created", namespace, poolName)
}

// CreateDestinationRuleForInferenceSet creates a DestinationRule for the EPP
// service associated with the InferenceSet. The DestinationRule configures
// SIMPLE TLS with insecureSkipVerify so Istio's sidecar can reach the EPP
// over TLS without requiring valid certificates.
func CreateDestinationRuleForInferenceSet(ctx context.Context, cl client.Client, name, namespace string) error {
	eppName := EPPServiceName(name)
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "networking.istio.io/v1",
			"kind":       "DestinationRule",
			"metadata": map[string]interface{}{
				"name":      eppName,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"host": eppName,
				"trafficPolicy": map[string]interface{}{
					"tls": map[string]interface{}{
						"mode":               "SIMPLE",
						"insecureSkipVerify": true,
					},
				},
			},
		},
	}
	return cl.Create(ctx, obj)
}

// CreateHTTPRouteForInferenceSet creates an HTTPRoute that routes requests
// with the matching X-Gateway-Model-Name header to the InferenceSet's
// InferencePool via the specified Gateway.
func CreateHTTPRouteForInferenceSet(ctx context.Context, cl client.Client, name, namespace, gatewayName string) error {
	poolName := InferencePoolName(name)
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata": map[string]interface{}{
				"name":      name + "-route",
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"parentRefs": []interface{}{
					map[string]interface{}{
						"group": "gateway.networking.k8s.io",
						"kind":  "Gateway",
						"name":  gatewayName,
					},
				},
				"rules": []interface{}{
					map[string]interface{}{
						"matches": []interface{}{
							map[string]interface{}{
								"headers": []interface{}{
									map[string]interface{}{
										"type":  "Exact",
										"name":  "X-Gateway-Model-Name",
										"value": name,
									},
								},
								"path": map[string]interface{}{
									"type":  "PathPrefix",
									"value": "/",
								},
							},
						},
						"backendRefs": []interface{}{
							map[string]interface{}{
								"name":  poolName,
								"group": "inference.networking.k8s.io",
								"kind":  "InferencePool",
							},
						},
					},
				},
			},
		},
	}
	return cl.Create(ctx, obj)
}

// CreateInferenceSetWithRouting creates an InferenceSet and waits for it to be
// ready (InferencePool created by KAITO), then creates the associated
// DestinationRule and HTTPRoute resources.
func CreateInferenceSetWithRouting(ctx context.Context, cl client.Client, cfg InferenceSetConfig) error {
	if err := CreateInferenceSet(ctx, cl, cfg); err != nil {
		return fmt.Errorf("failed to create InferenceSet %s: %w", cfg.Name, err)
	}

	if err := WaitForInferenceSetReady(ctx, cl, cfg.Name, cfg.Namespace, InferenceSetReadyTimeout); err != nil {
		return fmt.Errorf("InferenceSet %s not ready: %w", cfg.Name, err)
	}

	if err := CreateDestinationRuleForInferenceSet(ctx, cl, cfg.Name, cfg.Namespace); err != nil {
		return fmt.Errorf("failed to create DestinationRule for %s: %w", cfg.Name, err)
	}

	if err := CreateHTTPRouteForInferenceSet(ctx, cl, cfg.Name, cfg.Namespace, cfg.GatewayName); err != nil {
		return fmt.Errorf("failed to create HTTPRoute for %s: %w", cfg.Name, err)
	}

	return nil
}

// CleanupInferenceSetWithRouting deletes the InferenceSet, DestinationRule,
// and HTTPRoute resources created by CreateInferenceSetWithRouting.
func CleanupInferenceSetWithRouting(ctx context.Context, cl client.Client, name, namespace string) error {
	var errs []error

	// Delete HTTPRoute.
	httpRoute := &unstructured.Unstructured{}
	httpRoute.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "gateway.networking.k8s.io",
		Version: "v1",
		Kind:    "HTTPRoute",
	})
	httpRoute.SetName(name + "-route")
	httpRoute.SetNamespace(namespace)
	if err := cl.Delete(ctx, httpRoute); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete HTTPRoute %s-route: %w", name, err))
	}

	// Delete DestinationRule.
	dr := &unstructured.Unstructured{}
	dr.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "networking.istio.io",
		Version: "v1",
		Kind:    "DestinationRule",
	})
	dr.SetName(EPPServiceName(name))
	dr.SetNamespace(namespace)
	if err := cl.Delete(ctx, dr); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete DestinationRule %s: %w", EPPServiceName(name), err))
	}

	// Delete InferenceSet.
	is := &unstructured.Unstructured{}
	is.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "kaito.sh",
		Version: "v1alpha1",
		Kind:    "InferenceSet",
	})
	is.SetName(name)
	is.SetNamespace(namespace)
	if err := cl.Delete(ctx, is); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("delete InferenceSet %s: %w", name, err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("cleanup errors: %v", errs)
	}
	return nil
}
