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

// Package config holds the productionstack-status-reporter tunables (wired from
// command-line flags / the Helm chart values) plus the fixed reporter-identity
// constants, shared by the evaluators and the controllers (reporter) package.
package config

import (
	"time"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/scraper"
	"github.com/kaito-project/production-stack/pkg/util/window"
)

const (
	// ReporterComponent is the Event source.component / reportingController
	// value. Operators select on it:
	//   kubectl get events -n kube-system \
	//     --field-selector source=productionstack-status-reporter
	ReporterComponent = "productionstack-status-reporter"

	// ReportingNamespace is where every control-plane Event is published
	// (§1.1: always kube-system).
	ReportingNamespace = "kube-system"

	// defaultReleaseNamespace is the umbrella chart's default release Namespace,
	// also where every control-plane Event is published (§1.1: kube-system).
	defaultReleaseNamespace = "kube-system"
)

// NodeProvisionerRef registers the node-provisioner Deployment the cluster
// evaluator probes for clusterNodeProvisionerNotReady. When Name is empty the
// check is skipped (treated as Ready) so clusters that pre-provision GPU nodes
// are not penalised (§1.2).
type NodeProvisionerRef struct {
	Name      string
	Namespace string
}

// Config holds the evaluator tunables, wired from command-line flags / the
// Helm chart values.
type Config struct {
	// ReleaseNamespace is the umbrella chart's release Namespace, used as the
	// involvedObject of the positive clusterReady event (§1.2).
	ReleaseNamespace string

	// Component install namespaces probed for the cluster-layer reasons.
	IstioNamespace       string
	KaitoNamespace       string
	KedaNamespace        string
	GatewayAuthNamespace string
	BBRNamespace         string
	KedaScalerNamespace  string

	// Deployment names probed for cluster-layer readiness.
	IstiodDeployment      string
	KaitoDeployment       string
	BBRDeployment         string
	KedaScalerDeployment  string
	GatewayAuthDeployment string

	// NodeProvisioner is the optional node-provisioner Deployment to probe.
	NodeProvisioner NodeProvisionerRef

	// WeightDownload configures the inferencesetWeightDownloadSlow window.
	WeightDownload window.Config
	// MetricName / MetricPort configure the throughput scrape.
	MetricName string
	MetricPort int

	// ResyncInterval is how often every reason is re-evaluated.
	ResyncInterval time.Duration

	// StartupGracePeriod is the window during which transient startup states
	// are not surfaced as Warning events. Cluster/harness/EPP/route findings
	// are withheld while the backing resource is younger than this window
	// (object-age gating) or, for findings without a backing object, until the
	// problem has persisted this long (debounce). Workspace/model-pod findings
	// bypass it and instead discriminate terminal failures from in-progress
	// states (GPU provisioning / weight download legitimately take a while).
	// Set to 0 to disable.
	StartupGracePeriod time.Duration
}

// DefaultConfig returns a Config populated with the proposal defaults.
func DefaultConfig() Config {
	return Config{
		ReleaseNamespace:      defaultReleaseNamespace,
		IstioNamespace:        "istio-system",
		KaitoNamespace:        "kaito-system",
		KedaNamespace:         "keda",
		GatewayAuthNamespace:  "llm-gateway-auth",
		BBRNamespace:          "kaito-system",
		KedaScalerNamespace:   "keda",
		IstiodDeployment:      "istiod",
		KaitoDeployment:       "kaito-workspace",
		BBRDeployment:         "body-based-router",
		KedaScalerDeployment:  "keda-kaito-scaler",
		GatewayAuthDeployment: "apikey-authz",
		WeightDownload: window.Config{
			WindowDuration: 60 * time.Second,
			MinMBps:        20,
		},
		MetricName:         scraper.DefaultMetricName,
		MetricPort:         5000,
		ResyncInterval:     1 * time.Minute,
		StartupGracePeriod: 60 * time.Second,
	}
}
