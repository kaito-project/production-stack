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

package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/config"
	"github.com/kaito-project/production-stack/pkg/productionstack-status-reporter/controllers"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		enableLeaderElection bool
		leaderElectionID     string
		resyncSeconds        int
		startupGraceSeconds  int

		releaseNamespace      string
		istioNamespace        string
		kaitoNamespace        string
		kedaNamespace         string
		gatewayAuthNamespace  string
		bbrNamespace          string
		kedaScalerNamespace   string
		istiodDeployment      string
		kaitoDeployment       string
		bbrDeployment         string
		kedaScalerDeployment  string
		gatewayAuthDeployment string
		nodeProvisionerName   string
		nodeProvisionerNS     string
		weightWindowSeconds   int
		weightMinMBps         float64
		metricName            string
		metricPort            int
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election so only one replica emits events (HA).")
	flag.StringVar(&leaderElectionID, "leader-election-id", "productionstack-status-reporter",
		"The name of the leader election lease.")
	flag.IntVar(&resyncSeconds, "resync-seconds", 30, "How often every reason is re-evaluated.")
	flag.IntVar(&startupGraceSeconds, "startup-grace-seconds", int(config.DefaultConfig().StartupGracePeriod/time.Second),
		"Window during which transient startup states are not surfaced as Warning events (0 disables).")

	cfg := config.DefaultConfig()
	flag.StringVar(&releaseNamespace, "release-namespace", cfg.ReleaseNamespace, "Umbrella chart release namespace (cluster-layer event involvedObject).")
	flag.StringVar(&istioNamespace, "istio-namespace", cfg.IstioNamespace, "Istio control-plane install namespace.")
	flag.StringVar(&kaitoNamespace, "kaito-namespace", cfg.KaitoNamespace, "KAITO controller install namespace.")
	flag.StringVar(&kedaNamespace, "keda-namespace", cfg.KedaNamespace, "KEDA install namespace.")
	flag.StringVar(&gatewayAuthNamespace, "gateway-auth-namespace", cfg.GatewayAuthNamespace, "llm-gateway-auth install namespace.")
	flag.StringVar(&bbrNamespace, "bbr-namespace", cfg.BBRNamespace, "body-based-routing install namespace.")
	flag.StringVar(&kedaScalerNamespace, "keda-scaler-namespace", cfg.KedaScalerNamespace, "keda-kaito-scaler install namespace.")
	flag.StringVar(&istiodDeployment, "istiod-deployment", cfg.IstiodDeployment, "Istio control-plane (istiod) Deployment name.")
	flag.StringVar(&kaitoDeployment, "kaito-deployment", cfg.KaitoDeployment, "KAITO workspace controller Deployment name.")
	flag.StringVar(&bbrDeployment, "bbr-deployment", cfg.BBRDeployment, "body-based-routing Deployment name.")
	flag.StringVar(&kedaScalerDeployment, "keda-scaler-deployment", cfg.KedaScalerDeployment, "keda-kaito-scaler Deployment name.")
	flag.StringVar(&gatewayAuthDeployment, "gateway-auth-deployment", cfg.GatewayAuthDeployment, "llm-gateway-auth ext_authz Deployment name.")
	flag.StringVar(&nodeProvisionerName, "node-provisioner-name", "", "Optional node-provisioner Deployment name; empty disables the check.")
	flag.StringVar(&nodeProvisionerNS, "node-provisioner-namespace", "", "Node-provisioner Deployment namespace.")
	flag.IntVar(&weightWindowSeconds, "weight-download-window-seconds", 60, "Sliding-window length for inferencesetWeightDownloadSlow.")
	flag.Float64Var(&weightMinMBps, "weight-download-min-mbps", 20, "Throughput threshold (MB/s) for inferencesetWeightDownloadSlow.")
	flag.StringVar(&metricName, "weight-download-metric", cfg.MetricName, "Prometheus gauge name for model-weights download throughput.")
	flag.IntVar(&metricPort, "weight-download-metric-port", cfg.MetricPort, "Port exposing the throughput metric on the source pod.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg.ReleaseNamespace = releaseNamespace
	cfg.IstioNamespace = istioNamespace
	cfg.KaitoNamespace = kaitoNamespace
	cfg.KedaNamespace = kedaNamespace
	cfg.GatewayAuthNamespace = gatewayAuthNamespace
	cfg.BBRNamespace = bbrNamespace
	cfg.KedaScalerNamespace = kedaScalerNamespace
	cfg.IstiodDeployment = istiodDeployment
	cfg.KaitoDeployment = kaitoDeployment
	cfg.BBRDeployment = bbrDeployment
	cfg.KedaScalerDeployment = kedaScalerDeployment
	cfg.GatewayAuthDeployment = gatewayAuthDeployment
	cfg.NodeProvisioner = config.NodeProvisionerRef{Name: nodeProvisionerName, Namespace: nodeProvisionerNS}
	cfg.WeightDownload.WindowDuration = time.Duration(weightWindowSeconds) * time.Second
	cfg.WeightDownload.MinMBps = weightMinMBps
	cfg.MetricName = metricName
	cfg.MetricPort = metricPort
	cfg.ResyncInterval = time.Duration(resyncSeconds) * time.Second
	cfg.StartupGracePeriod = time.Duration(startupGraceSeconds) * time.Second

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := controllers.SetupWithManager(mgr, cfg); err != nil {
		setupLog.Error(err, "unable to set up status reporter")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
