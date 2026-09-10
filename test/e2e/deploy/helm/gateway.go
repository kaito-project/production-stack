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

package helm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/kaito-project/production-stack/test/e2e/deploy"
)

const (
	appRoutingNamespace          = "app-routing-system"
	appRoutingDefaultDNSWorkload = "default-domain-dns-external-dns"
	domainFilterArg              = "--domain-filter="
)

func (d *Deployer) deploymentGatewayValues(ctx context.Context, values deploy.GatewayValues) (deploy.GatewayValues, error) {
	provider := firstNonEmpty(values.CloudProvider, os.Getenv("E2E_PROVIDER"), "azure")
	if !strings.EqualFold(provider, "azure") {
		return values, nil
	}
	values.CloudProvider = "azure"
	if values.GatewayClassName == "" {
		values.GatewayClassName = "approuting-istio"
	}
	if values.DefaultDomain == "" {
		domain, err := d.getAppRoutingDomain(ctx)
		if err != nil {
			return deploy.GatewayValues{}, err
		}
		values.DefaultDomain = domain
	}
	return values, nil
}

func (d *Deployer) getAppRoutingDomain(ctx context.Context) (string, error) {
	d.domainMu.Lock()
	defer d.domainMu.Unlock()
	if d.appRoutingDomain != "" {
		return d.appRoutingDomain, nil
	}
	if domain := strings.TrimSpace(os.Getenv("APP_ROUTING_DEFAULT_DOMAIN")); domain != "" {
		if !isDNSName(domain) {
			return "", fmt.Errorf("APP_ROUTING_DEFAULT_DOMAIN %q is not a valid DNS name", domain)
		}
		d.appRoutingDomain = domain
		return domain, nil
	}
	out, err := d.kubectl(ctx, "get", "deployment", appRoutingDefaultDNSWorkload, "--namespace", appRoutingNamespace, "-o", "json")
	if err != nil {
		return "", fmt.Errorf("resolve AKS App Routing default domain from %s/%s: %w; set APP_ROUTING_DEFAULT_DOMAIN to override: %s",
			appRoutingNamespace, appRoutingDefaultDNSWorkload, err, out)
	}
	var deployment appsv1.Deployment
	if err := json.Unmarshal(out, &deployment); err != nil {
		return "", fmt.Errorf("parse App Routing DNS deployment: %w", err)
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, arg := range container.Args {
			if filter, ok := strings.CutPrefix(arg, domainFilterArg); ok {
				domain, err := domainFromFilterArg(filter)
				if err != nil {
					return "", err
				}
				d.appRoutingDomain = domain
				return domain, nil
			}
		}
	}
	return "", fmt.Errorf("no %s argument on any container of %s/%s", domainFilterArg, appRoutingNamespace, appRoutingDefaultDNSWorkload)
}

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

const (
	// gatewayPort is the HTTP listener port on the Istio Gateway Service.
	gatewayPort = 80

	// apiRoot is the OpenAI API prefix a gateway serves under. Endpoints
	// expose the root, so callers append "/chat/completions" or "/models".
	apiRoot = "/v1"

	// localGatewayDomain is the authority the chart's HTTPRoute matches on
	// when the stack is not fronted by a real DNS zone.
	localGatewayDomain = "gw.example.com"

	// portForwardReadyTimeout is generous because the wait is dominated by
	// the API server setting up the SPDY channel, which takes 20-30s on a
	// busy control plane.
	portForwardReadyTimeout = 90 * time.Second
)

// OpenGateway implements deploy.Deployer.
//
// A stack fronted by a real DNS zone (AKS App Routing) is reachable directly,
// so no tunnel is allocated. Otherwise the Gateway is cluster-internal and the
// only way in is a kubectl port-forward to the Service Istio derives from it.
func (d *Deployer) OpenGateway(ctx context.Context, namespace, gatewayName string) (deploy.GatewayEndpoint, error) {
	if namespace == "" {
		return nil, fmt.Errorf("helm: gateway: namespace is required")
	}
	if gatewayName == "" {
		return nil, fmt.Errorf("helm: gateway: gateway name is required")
	}

	// The harness install already told us how this namespace's Gateway is
	// exposed, so the domain is not resolved a second time here.
	if gateway, ok := d.harnessGateway(namespace); ok && gateway.DefaultDomain != "" {
		return &routedEndpoint{
			base: "https://" + namespace + "." + gateway.DefaultDomain + apiRoot,
		}, nil
	}

	service := istioGatewayServiceName(gatewayName)
	if out, err := d.kubectl(ctx, "get", "service", service, "--namespace", namespace); err != nil {
		return nil, fmt.Errorf("helm: gateway service %s/%s: %w\n%s", namespace, service, err, string(out))
	}

	d.gatewayMu.Lock()
	defer d.gatewayMu.Unlock()
	if existing, ok := d.gateways[namespace]; ok {
		return existing, nil
	}
	ep := &tunnelledEndpoint{
		host:      namespace + "." + localGatewayDomain,
		namespace: namespace,
		service:   service,
	}
	if _, err := ep.BaseURL(); err != nil {
		return nil, err
	}
	d.gateways[namespace] = ep
	return ep, nil
}

// CloseGateway implements deploy.Deployer.
func (d *Deployer) CloseGateway(_ context.Context, namespace string) error {
	d.gatewayMu.Lock()
	ep, ok := d.gateways[namespace]
	delete(d.gateways, namespace)
	d.gatewayMu.Unlock()
	if ok {
		ep.Reset()
	}
	return nil
}

// istioGatewayServiceName returns the Service Istio creates for a Gateway.
func istioGatewayServiceName(gatewayName string) string {
	return gatewayName + "-istio"
}

func (d *Deployer) harnessGateway(namespace string) (deploy.GatewayValues, bool) {
	d.gatewayMu.Lock()
	defer d.gatewayMu.Unlock()
	gateway, ok := d.harnessGateways[namespace]
	return gateway, ok
}

// routedEndpoint reaches the gateway over its published address. The URL
// carries the authority, so no Host override is needed and there is no
// transport to reset.
type routedEndpoint struct {
	base string
}

func (e *routedEndpoint) BaseURL() (string, error) { return e.base, nil }
func (e *routedEndpoint) Host() string             { return "" }
func (e *routedEndpoint) Reset()                   {}

// tunnelledEndpoint reaches the gateway through a kubectl port-forward it
// owns, rebuilding it whenever it is found dead.
type tunnelledEndpoint struct {
	host      string
	namespace string
	service   string

	mu        sync.Mutex
	cmd       *exec.Cmd
	exited    chan struct{}
	exitErr   error
	localPort int
}

func (e *tunnelledEndpoint) Host() string { return e.host }

func (e *tunnelledEndpoint) BaseURL() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.alive() {
		return fmt.Sprintf("http://localhost:%d%s", e.localPort, apiRoot), nil
	}
	if err := e.start(); err != nil {
		return "", err
	}
	return fmt.Sprintf("http://localhost:%d%s", e.localPort, apiRoot), nil
}

func (e *tunnelledEndpoint) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stop()
}

func (e *tunnelledEndpoint) alive() bool {
	if e.cmd == nil {
		return false
	}
	select {
	case <-e.exited:
		return false
	default:
		return true
	}
}

func (e *tunnelledEndpoint) stop() {
	if e.cmd == nil {
		return
	}
	if e.cmd.Process != nil {
		_ = e.cmd.Process.Kill()
		<-e.exited
	}
	e.cmd = nil
}

// start spawns kubectl port-forward and blocks until the local listener
// accepts. e.mu must be held.
func (e *tunnelledEndpoint) start() error {
	e.stop()

	// Always bind a fresh port. On a long run the kernel usually still holds
	// the previous socket in TIME_WAIT, and rebinding it fails for the whole
	// readiness window — which looks identical to a gateway that never came up.
	port, err := freePort()
	if err != nil {
		return fmt.Errorf("helm: gateway %s/%s: allocate local port: %w", e.namespace, e.service, err)
	}

	cmd := exec.Command("kubectl", "port-forward",
		"svc/"+e.service,
		fmt.Sprintf("%d:%d", port, gatewayPort),
		"-n", e.namespace)

	// Capture kubectl's output so a readiness timeout reports its actual
	// error instead of being indistinguishable from "took too long".
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("helm: gateway %s/%s: start kubectl port-forward: %w", e.namespace, e.service, err)
	}

	exited := make(chan struct{})
	e.cmd, e.exited, e.exitErr, e.localPort = cmd, exited, nil, port
	go func() {
		err := cmd.Wait()
		e.exitErr = fmt.Errorf("kubectl port-forward exited: %w\n%s", err, output.String())
		close(exited)
	}()

	deadline := time.Now().Add(portForwardReadyTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return fmt.Errorf("helm: gateway %s/%s: %w", e.namespace, e.service, e.exitErr)
		default:
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	e.stop()
	return fmt.Errorf("helm: gateway %s/%s: port-forward not ready within %s\n%s",
		e.namespace, e.service, portForwardReadyTimeout, output.String())
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
