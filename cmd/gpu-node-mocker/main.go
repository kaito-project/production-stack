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
		metricsAddr           string
		probeAddr             string
		shadowPodImage        string
		udsTokenizerImage     string
		leaseDurationSec      int
		leaseRenewIntervalSec int
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&shadowPodImage, "shadow-pod-image", controllers.DefaultInferenceSimImage,
		"Container image for the inference simulator running in shadow pods.")
	flag.StringVar(&udsTokenizerImage, "uds-tokenizer-image", controllers.DefaultUDSTokenizerImage,
		"Container image for the UDS tokenizer sidecar in shadow pods.")
	flag.IntVar(&leaseDurationSec, "lease-duration-seconds", 40,
		"Duration in seconds for fake node lease.")
	flag.IntVar(&leaseRenewIntervalSec, "lease-renew-interval-seconds", 10,
		"Interval in seconds at which the fake node lease is renewed.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	restCfg := ctrl.GetConfigOrDie()

	// Fail fast if required CRDs are not yet installed in the cluster. The
	// gpu-node-mocker controllers watch karpenter.sh/v1 NodeClaim objects;
	// without the CRD, controller-runtime's informers loop on "no kind is
	// registered" errors instead of failing. Exiting with a non-zero status
	// here lets the Deployment's restart policy back off and retry until
	// the KAITO operator (which ships the karpenter CRDs) finishes
	// installing them. This unblocks parallel install ordering at the
	// shell level (no need to gate on KAITO CRDs before deploying us).
	if err := checkRequiredCRDs(restCfg); err != nil {
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

	cfg := controllers.Config{
		ShadowPodImage:        shadowPodImage,
		UDSTokenizerImage:     udsTokenizerImage,
		LeaseDurationSec:      int32(leaseDurationSec),
		LeaseRenewIntervalSec: leaseRenewIntervalSec,
	}

	// Register all controllers.
	if err := controllers.NewControllers(mgr, cfg); err != nil {
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
func checkRequiredCRDs(cfg *rest.Config) error {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}

	required := []struct {
		groupVersion string
		resource     string
	}{
		// Karpenter NodeClaim CRD is installed by the KAITO workspace
		// operator's chart; the NodeClaimReconciler watches it.
		{groupVersion: "karpenter.sh/v1", resource: "nodeclaims"},
	}

	for _, r := range required {
		list, err := dc.ServerResourcesForGroupVersion(r.groupVersion)
		if err != nil {
			return fmt.Errorf("discovering resources for %s: %w", r.groupVersion, err)
		}
		found := false
		for _, api := range list.APIResources {
			if api.Name == r.resource {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("required resource %s.%s is not yet registered with the apiserver", r.resource, r.groupVersion)
		}
		setupLog.Info("required CRD is ready", "groupVersion", r.groupVersion, "resource", r.resource)
	}
	return nil
}
