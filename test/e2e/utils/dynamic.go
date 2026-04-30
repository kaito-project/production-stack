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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var (
	// InferenceSetGVR is the GroupVersionResource for KAITO InferenceSet objects.
	InferenceSetGVR = schema.GroupVersionResource{
		Group:    "kaito.sh",
		Version:  "v1alpha1",
		Resource: "inferencesets",
	}

	// InferencePoolGVR is the GroupVersionResource for Gateway API InferencePool objects.
	InferencePoolGVR = schema.GroupVersionResource{
		Group:    "inference.networking.k8s.io",
		Version:  "v1",
		Resource: "inferencepools",
	}

	// HTTPRouteGVR is the GroupVersionResource for Gateway API HTTPRoute objects.
	HTTPRouteGVR = schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "httproutes",
	}

	// DestinationRuleGVR is the GroupVersionResource for Istio DestinationRule objects.
	DestinationRuleGVR = schema.GroupVersionResource{
		Group:    "networking.istio.io",
		Version:  "v1",
		Resource: "destinationrules",
	}

	// GatewayGVK identifies the Gateway resource provisioned per case.
	GatewayGVK = schema.GroupVersionKind{
		Group:   "gateway.networking.k8s.io",
		Version: "v1",
		Kind:    "Gateway",
	}

	// HTTPRouteGVK identifies HTTPRoute resources (used to create the
	// per-namespace catch-all that returns OpenAI-compatible 404 JSON).
	HTTPRouteGVK = schema.GroupVersionKind{
		Group:   "gateway.networking.k8s.io",
		Version: "v1",
		Kind:    "HTTPRoute",
	}

	// ReferenceGrantGVK identifies the ReferenceGrant used to permit a
	// per-case HTTPRoute to reference the shared model-not-found Service
	// living in the default namespace.
	ReferenceGrantGVK = schema.GroupVersionKind{
		Group:   "gateway.networking.k8s.io",
		Version: "v1beta1",
		Kind:    "ReferenceGrant",
	}

	// AuthorizationPolicyGVK identifies the Istio AuthorizationPolicy used
	// to wire each per-case Gateway into the apikey-ext-authz CUSTOM
	// provider. The upstream llm-gateway-apikey chart only installs an AP
	// targeting the cluster-wide `inference-gateway`, so each per-case
	// Gateway must get its own AP provisioned alongside it.
	AuthorizationPolicyGVK = schema.GroupVersionKind{
		Group:   "security.istio.io",
		Version: "v1",
		Kind:    "AuthorizationPolicy",
	}

	// APIKeyGVK identifies the KAITO APIKey CR that the apikey-operator
	// reconciles into a Secret named APIKeySecretName.
	APIKeyGVK = schema.GroupVersionKind{
		Group:   "kaito.sh",
		Version: "v1alpha1",
		Kind:    "APIKey",
	}
)

// GetDynamicClient returns a dynamic Kubernetes client for working with
// unstructured/CRD resources (InferenceSet, InferencePool, etc.).
func GetDynamicClient() (dynamic.Interface, error) {
	cfg, err := GetK8sConfig()
	if err != nil {
		return nil, err
	}
	return dynamic.NewForConfig(cfg)
}
