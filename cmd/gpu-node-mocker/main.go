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
	"fmt"
	"os"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	karpenterv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/kaito-project/production-stack/pkg/gpu-node-mocker/controllers"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(coordinationv1.AddToScheme(scheme))
	// Karpenter v1 doesn't export a SchemeBuilder; register types manually.
	gv := schema.GroupVersion{Group: "karpenter.sh", Version: "v1"}
	scheme.AddKnownTypes(gv,
		&karpenterv1.NodePool{},
		&karpenterv1.NodePoolList{},
		&karpenterv1.NodeClaim{},
		&karpenterv1.NodeClaimList{},
	)
	metav1.AddToGroupVersion(scheme, gv)
}

func main() {
	var (
		metricsAddr            string
		probeAddr              string
		shadowPodImage         string
		udsTokenizerImage      string
		timeToFirstToken       string
		interTokenLatency      string
		ttftStdDev             string
		itlStdDev              string
		kvCacheTransfer        string
		kvCacheTransferStdDev  string
		timeFactorUnderLoad    string
		latencyCalculator      string
		prefillOverhead        string
		prefillTimePerToken    string
		prefillTimeStdDev      string
		kvTransferTimePerToken string
		kvTransferTimeStdDev   string
		leaseDurationSec       int
		leaseRenewIntervalSec  int
		nodeProvisioner        string
		nodeClassGroup         string
		nodeClassVersion       string
		nodeClassKind          string
		nodeClassResource      string
	)

	defaultNodeClass := controllers.DefaultNodeClassRef()

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&shadowPodImage, "shadow-pod-image", controllers.DefaultInferenceSimImage,
		"Container image for the inference simulator running in shadow pods.")
	flag.StringVar(&udsTokenizerImage, "uds-tokenizer-image", controllers.DefaultUDSTokenizerImage,
		"Container image for the UDS tokenizer sidecar in shadow pods.")
	flag.StringVar(&timeToFirstToken, "time-to-first-token", "",
		"Override the selected latency profile's time-to-first-token (e.g. 100ms). Empty ⇒ use the profile value. See llm-d-inference-sim latency-profiles.md.")
	flag.StringVar(&interTokenLatency, "inter-token-latency", "",
		"Override the selected latency profile's inter-token latency (e.g. 30ms). Empty ⇒ use the profile value. See llm-d-inference-sim latency-profiles.md.")
	flag.StringVar(&ttftStdDev, "time-to-first-token-std-dev", "",
		"Override the selected latency profile's std-dev jitter for time-to-first-token (e.g. 20ms). Empty ⇒ use the profile value.")
	flag.StringVar(&itlStdDev, "inter-token-latency-std-dev", "",
		"Override the selected latency profile's std-dev jitter for inter-token latency (e.g. 2ms). Empty ⇒ use the profile value.")
	flag.StringVar(&kvCacheTransfer, "kv-cache-transfer-latency", "",
		"Override the selected latency profile's constant KV-cache transfer overhead (e.g. 2ms). Empty ⇒ use the profile value.")
	flag.StringVar(&kvCacheTransferStdDev, "kv-cache-transfer-latency-std-dev", "",
		"Override the selected latency profile's std-dev jitter for KV-cache transfer latency (e.g. 400us). Empty ⇒ use the profile value.")
	flag.StringVar(&timeFactorUnderLoad, "time-factor-under-load", "",
		"Override the selected latency profile's latency multiplier as concurrency approaches max-num-seqs (e.g. 2.0). Empty ⇒ use the profile value.")
	flag.StringVar(&latencyCalculator, "latency-calculator", "",
		fmt.Sprintf("Operator-wide default latency model (%q or %q) when a pod has no %q annotation. Empty ⇒ %q.",
			controllers.LatencyCalculatorConstant, controllers.LatencyCalculatorPerToken,
			controllers.AnnotationLatencyCalculator, controllers.DefaultLatencyCalculator))
	flag.StringVar(&prefillOverhead, "prefill-overhead", "",
		"per-token calculator: override the profile's fixed prefill overhead (e.g. 30ms). Empty ⇒ use the profile value.")
	flag.StringVar(&prefillTimePerToken, "prefill-time-per-token", "",
		"per-token calculator: override the profile's prefill cost per prompt token (e.g. 250us). Empty ⇒ use the profile value.")
	flag.StringVar(&prefillTimeStdDev, "prefill-time-std-dev", "",
		"per-token calculator: override the profile's std-dev jitter for prefill time (e.g. 5ms). Empty ⇒ use the profile value.")
	flag.StringVar(&kvTransferTimePerToken, "kv-cache-transfer-time-per-token", "",
		"per-token calculator: override the profile's KV-cache transfer cost per token (e.g. 3us). Empty ⇒ use the profile value.")
	flag.StringVar(&kvTransferTimeStdDev, "kv-cache-transfer-time-std-dev", "",
		"per-token calculator: override the profile's std-dev jitter for KV-cache transfer time (e.g. 200us). Empty ⇒ use the profile value.")
	flag.IntVar(&leaseDurationSec, "lease-duration-seconds", 40,
		"Duration in seconds for fake node lease.")
	flag.IntVar(&leaseRenewIntervalSec, "lease-renew-interval-seconds", 10,
		"Interval in seconds at which the fake node lease is renewed.")
	flag.StringVar(&nodeProvisioner, "node-provisioner", controllers.ProvisionerAzureGPU,
		fmt.Sprintf("The KAITO node provisioner to mock (%q or %q).",
			controllers.ProvisionerAzureGPU, controllers.ProvisionerKarpenter))
	flag.StringVar(&nodeClassGroup, "node-class-group", defaultNodeClass.Group,
		"API group of the karpenter NodeClass to reconcile in karpenter mode.")
	flag.StringVar(&nodeClassVersion, "node-class-version", defaultNodeClass.Version,
		"API version of the karpenter NodeClass to reconcile in karpenter mode.")
	flag.StringVar(&nodeClassKind, "node-class-kind", defaultNodeClass.Kind,
		"Kind of the karpenter NodeClass to reconcile in karpenter mode.")
	flag.StringVar(&nodeClassResource, "node-class-resource", defaultNodeClass.Resource,
		"Plural resource name of the karpenter NodeClass (used for the startup CRD discovery check).")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if leaseRenewIntervalSec <= 0 {
		setupLog.Error(nil, "--lease-renew-interval-seconds must be > 0")
		os.Exit(1)
	}
	if leaseDurationSec <= 0 {
		setupLog.Error(nil, "--lease-duration-seconds must be > 0")
		os.Exit(1)
	}
	if latencyCalculator != "" &&
		latencyCalculator != controllers.LatencyCalculatorConstant &&
		latencyCalculator != controllers.LatencyCalculatorPerToken {
		setupLog.Error(nil, "--latency-calculator must be \"constant\", \"per-token\", or empty",
			"value", latencyCalculator)
		os.Exit(1)
	}

	cfg := controllers.Config{
		ShadowPodImage:              shadowPodImage,
		UDSTokenizerImage:           udsTokenizerImage,
		TimeToFirstToken:            timeToFirstToken,
		InterTokenLatency:           interTokenLatency,
		TimeToFirstTokenStdDev:      ttftStdDev,
		InterTokenLatencyStdDev:     itlStdDev,
		KVCacheTransferLatency:      kvCacheTransfer,
		KVCacheTransferStdDev:       kvCacheTransferStdDev,
		TimeFactorUnderLoad:         timeFactorUnderLoad,
		LatencyCalculator:           latencyCalculator,
		PrefillOverhead:             prefillOverhead,
		PrefillTimePerToken:         prefillTimePerToken,
		PrefillTimeStdDev:           prefillTimeStdDev,
		KVCacheTransferTimePerToken: kvTransferTimePerToken,
		KVCacheTransferTimeStdDev:   kvTransferTimeStdDev,
		LeaseDurationSec:            int32(leaseDurationSec),
		LeaseRenewIntervalSec:       leaseRenewIntervalSec,
		NodeClass: controllers.NodeClassRef{
			Group:    nodeClassGroup,
			Version:  nodeClassVersion,
			Kind:     nodeClassKind,
			Resource: nodeClassResource,
		},
	}

	restCfg := ctrl.GetConfigOrDie()

	// Build the provisioner-specific mocker first so we can validate exactly
	// the CRDs it depends on (and fail fast on an invalid --node-provisioner).
	mocker, err := controllers.NewProvisionerMocker(nodeProvisioner, cfg)
	if err != nil {
		setupLog.Error(err, "invalid --node-provisioner")
		os.Exit(1)
	}
	setupLog.Info("mocking node provisioner", "provisioner", mocker.Type())

	// Fail fast if required CRDs are not yet installed in the cluster. The
	// gpu-node-mocker controllers watch karpenter.sh/v1 NodeClaim (and, in
	// karpenter mode, NodePool) objects; without the CRDs, controller-runtime's
	// informers loop on "no kind is registered" errors instead of failing.
	// Exiting with a non-zero status here lets the Deployment's restart policy
	// back off and retry until the KAITO operator (which ships the karpenter
	// CRDs) finishes installing them. This unblocks parallel install ordering
	// at the shell level (no need to gate on KAITO CRDs before deploying us).
	if err := checkRequiredCRDs(restCfg, mocker.RequiredCRDs()); err != nil {
		setupLog.Error(err, "required CRDs are not ready; exiting so the pod is restarted")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Register all controllers.
	if err := controllers.NewControllers(mgr, cfg, mocker); err != nil {
		setupLog.Error(err, "unable to register controllers")
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

// checkRequiredCRDs verifies that every API resource the gpu-node-mocker
// controllers depend on is already registered with the API server. The
// check is done via discovery so it does not require the CRD types to be
// served — only that the apiserver advertises the resource. A single
// missing resource returns an error; the caller is expected to exit so
// the kubelet restarts the pod (the simplest "wait for CRDs" strategy).
func checkRequiredCRDs(cfg *rest.Config, required []controllers.RequiredCRD) error {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}

	for _, r := range required {
		list, err := dc.ServerResourcesForGroupVersion(r.GroupVersion)
		if err != nil {
			return fmt.Errorf("discovering resources for %s: %w", r.GroupVersion, err)
		}
		found := false
		for _, api := range list.APIResources {
			if api.Name == r.Resource {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("required resource %s.%s is not yet registered with the apiserver", r.Resource, r.GroupVersion)
		}
		setupLog.Info("required CRD is ready", "groupVersion", r.GroupVersion, "resource", r.Resource)
	}
	return nil
}
