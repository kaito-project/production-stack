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
	"os"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var (
	appRoutingDomainOnce sync.Once
	appRoutingDomain     string
	appRoutingDomainErr  error
)

// UseAppRouting reports whether E2E is targeting AKS managed App Routing.
func UseAppRouting() bool {
	return strings.EqualFold(os.Getenv("E2E_USE_APP_ROUTING"), "true")
}

func getAppRoutingDomain() (string, error) {
	if !UseAppRouting() {
		return "", nil
	}
	appRoutingDomainOnce.Do(func() {
		appRoutingDomain = strings.TrimSpace(os.Getenv("APP_ROUTING_DEFAULT_DOMAIN"))
		if appRoutingDomain != "" {
			if !isDNSName(appRoutingDomain) {
				appRoutingDomainErr = fmt.Errorf("APP_ROUTING_DEFAULT_DOMAIN %q is not a valid DNS name", appRoutingDomain)
			}
			return
		}
		domain, err := getAppRoutingDomainFromCluster(context.Background())
		if err == nil {
			appRoutingDomain = domain
			return
		}
		appRoutingDomainErr = fmt.Errorf("resolve AKS App Routing default domain from clusterexternaldns/default-domain-dns: %w; set APP_ROUTING_DEFAULT_DOMAIN to override", err)
	})
	return appRoutingDomain, appRoutingDomainErr
}

func getAppRoutingDomainFromCluster(ctx context.Context) (string, error) {
	config, err := GetK8sConfig()
	if err != nil {
		return "", err
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return "", err
	}
	resource, err := client.Resource(schema.GroupVersionResource{
		Group: "approuting.kubernetes.azure.com", Version: "v1alpha1", Resource: "clusterexternaldnses",
	}).Get(ctx, "default-domain-dns", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	resourceIDs, found, err := unstructured.NestedStringSlice(resource.Object, "spec", "dnsZoneResourceIDs")
	if err != nil || !found || len(resourceIDs) == 0 {
		return "", fmt.Errorf("default-domain-dns has no DNS zone resource ID")
	}
	return domainFromDNSZoneResourceID(resourceIDs[0])
}

func domainFromDNSZoneResourceID(resourceID string) (string, error) {
	domain := strings.TrimSpace(resourceID[strings.LastIndex(resourceID, "/")+1:])
	if !isDNSName(domain) {
		return "", fmt.Errorf("DNS zone resource ID %q does not contain a valid domain", resourceID)
	}
	return domain, nil
}

func isDNSName(value string) bool {
	if len(value) == 0 || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || !isDNSAlphaNumeric(label[0]) || !isDNSAlphaNumeric(label[len(label)-1]) {
			return false
		}
		for index := 1; index < len(label)-1; index++ {
			if !isDNSAlphaNumeric(label[index]) && label[index] != '-' {
				return false
			}
		}
	}
	return true
}

func isDNSAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

// GatewayHostFor returns the authority used to route requests to a namespace's
// Gateway in the active E2E gateway mode.
func GatewayHostFor(namespace string) (string, error) {
	if UseAppRouting() {
		domain, err := getAppRoutingDomain()
		if err != nil {
			return "", err
		}
		return namespace + "." + domain, nil
	}
	return namespace + ".gw.example.com", nil
}
