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
)

// The AKS App Routing add-on renders one external-dns Deployment per managed
// zone; the default zone's carries the domain in its --domain-filter argument.
const (
	appRoutingNamespace          = "app-routing-system"
	appRoutingDefaultDNSWorkload = "default-domain-dns-external-dns"
	domainFilterArg              = "--domain-filter="
)

var (
	appRoutingDomainOnce sync.Once
	appRoutingDomain     string
	appRoutingDomainErr  error
)

// IsAzureProvider reports whether E2E is using the Azure provider.
func IsAzureProvider() bool {
	provider := os.Getenv("E2E_PROVIDER")
	return provider == "" || strings.EqualFold(provider, "azure")
}

func getAppRoutingDomain() (string, error) {
	if !IsAzureProvider() {
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
		appRoutingDomainErr = fmt.Errorf("resolve AKS App Routing default domain from %s/%s: %w; set APP_ROUTING_DEFAULT_DOMAIN to override",
			appRoutingNamespace, appRoutingDefaultDNSWorkload, err)
	})
	return appRoutingDomain, appRoutingDomainErr
}

// getAppRoutingDomainFromCluster reads the default zone off the App Routing
// external-dns Deployment rather than the approuting.kubernetes.azure.com
// ClusterExternalDNS it was rendered from: a managed control plane such as AI
// Manager grants read access to built-in kinds only, so touching the CRD would
// fail with 403.
func getAppRoutingDomainFromCluster(ctx context.Context) (string, error) {
	clientset, err := GetK8sClientset()
	if err != nil {
		return "", err
	}
	deployment, err := clientset.AppsV1().Deployments(appRoutingNamespace).
		Get(ctx, appRoutingDefaultDNSWorkload, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, arg := range container.Args {
			filter, ok := strings.CutPrefix(arg, domainFilterArg)
			if !ok {
				continue
			}
			return domainFromFilterArg(filter)
		}
	}
	return "", fmt.Errorf("no %s argument on any container of %s/%s",
		domainFilterArg, appRoutingNamespace, appRoutingDefaultDNSWorkload)
}

// domainFromFilterArg takes the first zone of an external-dns --domain-filter
// value, which is a comma-separated list.
func domainFromFilterArg(filter string) (string, error) {
	domain := strings.TrimSpace(strings.Split(filter, ",")[0])
	domain = strings.Trim(domain, ".")
	if !isDNSName(domain) {
		return "", fmt.Errorf("%s%q does not contain a valid domain", domainFilterArg, filter)
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
	if IsAzureProvider() {
		domain, err := getAppRoutingDomain()
		if err != nil {
			return "", err
		}
		return namespace + "." + domain, nil
	}
	return namespace + ".gw.example.com", nil
}
