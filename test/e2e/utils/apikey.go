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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// APIKeySecretName is the Secret created by the apikey-operator when an APIKey CR exists.
	APIKeySecretName = "llm-api-key"

	// APIKeySecretDataKey is the key inside the Secret that holds the plaintext API key.
	APIKeySecretDataKey = "apiKey"
)

// GetAPIKeyFromSecret reads the plaintext API key from the operator-generated Secret.
func GetAPIKeyFromSecret(ctx context.Context, namespace string) (string, error) {
	clientset, err := GetK8sClientset()
	if err != nil {
		return "", fmt.Errorf("failed to create clientset: %w", err)
	}

	secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, APIKeySecretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get secret %s/%s: %w", namespace, APIKeySecretName, err)
	}

	keyBytes, ok := secret.Data[APIKeySecretDataKey]
	if !ok {
		return "", fmt.Errorf("secret %s/%s does not contain key %q", namespace, APIKeySecretName, APIKeySecretDataKey)
	}

	return string(keyBytes), nil
}
