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
		Version:  "v1alpha2",
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
