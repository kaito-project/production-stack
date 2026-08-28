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

package controllers

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// ── Detection ───────────────────────────────────────────────────────────────

func podWithModelArg(arg string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "model", Args: strings.Fields(arg)}},
		},
	}
}

func TestDetectStreamingMode(t *testing.T) {
	sasPod := podWithModelArg("--model $STREAM_MODEL_URI")
	sasPod.Spec.InitContainers = []corev1.Container{{Name: KaitoSASFetchInitContainerName}}

	multiContainer := podWithModelArg("--model az://weights/phi-3")
	multiContainer.Spec.Containers = append([]corev1.Container{{Name: "sidecar"}}, multiContainer.Spec.Containers...)

	tests := []struct {
		name string
		pod  *corev1.Pod
		want streamingMode
	}{
		{"sas via fetch-sas init container", sasPod, streamingModeSAS},
		{"direct azure", podWithModelArg("--model az://weights/phi-3"), streamingModeDirectAzure},
		{"direct azure equals form", podWithModelArg("--model=az://weights/phi-3"), streamingModeDirectAzure},
		{"s3 unsupported", podWithModelArg("--model s3://bucket/phi-3"), streamingModeUnsupported},
		{"gcs unsupported", podWithModelArg("--model gs://bucket/phi-3"), streamingModeUnsupported},
		{"huggingface repo", podWithModelArg("--model tiiuae/falcon-7b-instruct"), streamingModeNone},
		{"no model arg", podWithModelArg("--port 8000"), streamingModeNone},
		{"sidecar before model container", multiContainer, streamingModeDirectAzure},
		{
			// KAITO sets --load-format=runai_streamer here too, but the weights
			// are on local disk. Keying on that flag instead of the URI scheme
			// would send the probe after a path the shadow pod does not have.
			"local weights path is not streaming",
			podWithModelArg("--model /opt/kaito/models/deepseekv4flash --load-format runai_streamer"),
			streamingModeNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectStreamingMode(tt.pod); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectStreamingModeShellWrapped(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:    "model",
			Command: []string{"/bin/sh", "-c", "python3 api.py --model=az://weights/phi-3 --port=8000"},
		}}},
	}
	if got := detectStreamingMode(pod); got != streamingModeDirectAzure {
		t.Errorf("got %q, want %q", got, streamingModeDirectAzure)
	}
}

// ── Identity cloning ────────────────────────────────────────────────────────

// newSASOriginalPod mimics a KAITO SAS-path pod AFTER the azure-workload-identity
// webhook has mutated it, which is the only form the mocker ever observes.
func newSASOriginalPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-0",
			Namespace: "models",
			Labels: map[string]string{
				InferenceSetCreatedByLabelKey: "phi-3",
				AzureWorkloadIdentityUseLabel: "true",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:           "fake-ws1",
			ServiceAccountName: "kaito-model-streamer",
			InitContainers: []corev1.Container{
				{
					Name:  "cuda-toolkit-provisioner",
					Image: "cuda:latest",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "cuda-toolkit", MountPath: "/usr/local/nvidia"},
					},
				},
				{
					Name:    KaitoSASFetchInitContainerName,
					Image:   "python:3.12-slim",
					Command: []string{"/bin/sh", "-c", "pip install azure-identity && python3 /scripts/fetch_sas.py"},
					Env: []corev1.EnvVar{
						{Name: "STREAM_DATAREFS_URL", Value: "https://example.com/datarefs/phi-3/versions/1"},
						{Name: "STREAM_IDENTITY_CLIENT_ID", Value: "00000000-0000-0000-0000-000000000000"},
						{Name: "STREAM_SOURCE_TYPE", Value: "public"},
						{Name: KaitoSASEnvFileEnvVar, Value: "/mnt/streaming/env"},
						// Injected by the workload-identity webhook.
						{Name: "AZURE_CLIENT_ID", Value: "injected"},
						{Name: "AZURE_TENANT_ID", Value: "injected"},
						{Name: "AZURE_FEDERATED_TOKEN_FILE", Value: "/var/run/secrets/azure/tokens/azure-identity-token"},
						{Name: "AZURE_AUTHORITY_HOST", Value: "https://login.microsoftonline.com/"},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: KaitoSASSharedVolumeName, MountPath: "/mnt/streaming"},
						{Name: "fetch-sas-script", MountPath: "/scripts", ReadOnly: true},
						{Name: AzureIdentityTokenVolumeName, MountPath: "/var/run/secrets/azure/tokens"},
					},
				},
			},
			Containers: []corev1.Container{{
				Name:  "ws",
				Args:  []string{"--model", "$STREAM_MODEL_URI", "--served-model-name", "phi-3"},
				Ports: []corev1.ContainerPort{{ContainerPort: 5000}},
			}},
			Volumes: []corev1.Volume{
				{Name: KaitoSASSharedVolumeName, VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
				}},
				{Name: "fetch-sas-script", VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "ws-fetch-sas-script"},
					},
				}},
				{Name: AzureIdentityTokenVolumeName, VolumeSource: corev1.VolumeSource{
					Projected: &corev1.ProjectedVolumeSource{},
				}},
				// Must NOT be cloned: mounted by another container only.
				{Name: "cuda-toolkit", VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/usr/local/nvidia"},
				}},
				{Name: "weights", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "weights"},
				}},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
}

func TestBuildStreamingIdentity_SAS(t *testing.T) {
	id := buildStreamingIdentity(newSASOriginalPod(), streamingModeSAS)

	if id.serviceAccount != "kaito-model-streamer" {
		t.Errorf("serviceAccount = %q", id.serviceAccount)
	}
	if id.podLabels[AzureWorkloadIdentityUseLabel] != "true" {
		t.Errorf("missing workload identity label: %v", id.podLabels)
	}
	if len(id.initContainers) != 1 || id.initContainers[0].Name != KaitoSASFetchInitContainerName {
		t.Fatalf("expected only the fetch-sas container, got %+v", id.initContainers)
	}

	cloned := id.initContainers[0]
	for _, e := range cloned.Env {
		if _, injected := azureWorkloadIdentityEnvVars[e.Name]; injected {
			t.Errorf("webhook-injected env %q survived the clone", e.Name)
		}
	}
	// The streaming config itself must survive.
	if envValue(cloned.Env, "STREAM_DATAREFS_URL") == "" ||
		envValue(cloned.Env, "STREAM_SOURCE_TYPE") != "public" {
		t.Errorf("streaming env lost in clone: %+v", cloned.Env)
	}
	for _, m := range cloned.VolumeMounts {
		if m.Name == AzureIdentityTokenVolumeName {
			t.Error("projected token mount survived the clone")
		}
	}
	if id.sasEnvFilePath != "/mnt/streaming/env" {
		t.Errorf("sasEnvFilePath = %q", id.sasEnvFilePath)
	}

	got := map[string]bool{}
	for _, v := range id.volumes {
		got[v.Name] = true
	}
	if !got[KaitoSASSharedVolumeName] || !got["fetch-sas-script"] {
		t.Errorf("missing referenced volumes: %v", got)
	}
	for _, unwanted := range []string{"cuda-toolkit", "weights", AzureIdentityTokenVolumeName} {
		if got[unwanted] {
			t.Errorf("volume %q should not be cloned", unwanted)
		}
	}
}

func TestBuildStreamingIdentity_NoWorkloadIdentityLabelSkipsServiceAccount(t *testing.T) {
	pod := newSASOriginalPod()
	delete(pod.Labels, AzureWorkloadIdentityUseLabel)

	if id := buildStreamingIdentity(pod, streamingModeSAS); id.serviceAccount != "" {
		t.Errorf("serviceAccount = %q, want empty when the pod carries no workload-identity label", id.serviceAccount)
	}
}

func TestBuildStreamingIdentity_DirectAzure(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ws-0", Namespace: "models",
			Labels: map[string]string{AzureWorkloadIdentityUseLabel: "true"},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: "kaito-model-streamer",
			Containers: []corev1.Container{{
				Name: "ws",
				Args: []string{"--model", "az://weights/phi-3"},
				Env:  []corev1.EnvVar{{Name: "AZURE_STORAGE_ACCOUNT_NAME", Value: "mystorage"}},
			}},
		},
	}

	id := buildStreamingIdentity(pod, streamingModeDirectAzure)
	if id.modelURI != "az://weights/phi-3" {
		t.Errorf("modelURI = %q", id.modelURI)
	}
	if len(id.initContainers) != 0 {
		t.Errorf("direct-azure must clone no init containers, got %d", len(id.initContainers))
	}
	if envValue(id.providerEnv, "AZURE_STORAGE_ACCOUNT_NAME") != "mystorage" {
		t.Errorf("providerEnv = %+v", id.providerEnv)
	}
}

// ── Probe container ─────────────────────────────────────────────────────────

func TestBuildProbeContainer_SAS(t *testing.T) {
	id := buildStreamingIdentity(newSASOriginalPod(), streamingModeSAS)
	c := buildProbeContainer(id, testConfig(), "probe-cm")

	if c.Name != StreamingProbeContainerName {
		t.Errorf("name = %q", c.Name)
	}
	if len(c.Ports) != 0 {
		t.Errorf("probe must declare no ports, got %v", c.Ports)
	}
	if _, hasGPU := c.Resources.Limits["nvidia.com/gpu"]; hasGPU {
		t.Error("probe must not request GPUs")
	}
	if c.Resources.Requests.Memory().IsZero() || c.Resources.Limits.Memory().IsZero() {
		t.Error("probe must declare memory requests and limits")
	}
	if envValue(c.Env, KaitoSASEnvFileEnvVar) != "/mnt/streaming/env" {
		t.Errorf("missing SAS env file path: %+v", c.Env)
	}
	if envValue(c.Env, "RUNAI_STREAMER_MEMORY_LIMIT") == "" {
		t.Error("probe must pin the streamer ring buffer size")
	}

	script := c.Command[2]
	// SAS validates by listing over plain HTTPS, so it must install nothing:
	// pulling torch here would cost ~1.3 GB and minutes for no added coverage.
	if strings.Contains(script, "pip install") {
		t.Errorf("sas mode must not install any wheels: %s", script)
	}
	if !strings.Contains(script, `. "$STREAM_ENV_FILE"`) {
		t.Error("sas mode must source the SAS env file")
	}
	// Auto-export rather than a name list, so a new variable in the SAS env
	// file reaches the streamer without a change here.
	if !strings.Contains(script, "set -a") {
		t.Error("sas env file must be sourced under set -a")
	}

	mounts := map[string]bool{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name] = true
	}
	if !mounts["probe-cm"] || !mounts[KaitoSASSharedVolumeName] {
		t.Errorf("unexpected mounts: %v", mounts)
	}
}

// The probe must read the SAS env file where fetch-sas actually writes it, so
// both the path and its mount are taken from the cloned container rather than
// assumed. A KAITO-side move of either would otherwise leave the probe mounting
// one path while reading another.
func TestBuildProbeContainer_FollowsOriginalSASPaths(t *testing.T) {
	original := newSASOriginalPod()
	for i := range original.Spec.InitContainers {
		c := &original.Spec.InitContainers[i]
		if c.Name != KaitoSASFetchInitContainerName {
			continue
		}
		for j := range c.Env {
			if c.Env[j].Name == KaitoSASEnvFileEnvVar {
				c.Env[j].Value = "/elsewhere/sas.env"
			}
		}
		for j := range c.VolumeMounts {
			if c.VolumeMounts[j].Name == KaitoSASSharedVolumeName {
				c.VolumeMounts[j].MountPath = "/elsewhere"
			}
		}
	}

	probe := buildProbeContainer(buildStreamingIdentity(original, streamingModeSAS), testConfig(), "probe-cm")
	if got := envValue(probe.Env, KaitoSASEnvFileEnvVar); got != "/elsewhere/sas.env" {
		t.Errorf("STREAM_ENV_FILE = %q, want the original's value", got)
	}
	if got := mountPath(probe.VolumeMounts, KaitoSASSharedVolumeName); got != "/elsewhere" {
		t.Errorf("shared volume mounted at %q, want the original's mount path", got)
	}
}

func TestBuildProbeContainer_DirectAzureDoesNotSourceEnvFile(t *testing.T) {
	id := streamingIdentity{mode: streamingModeDirectAzure, modelURI: "az://weights/phi-3"}
	c := buildProbeContainer(id, testConfig(), "probe-cm")

	if envValue(c.Env, "PROBE_MODEL_URI") != "az://weights/phi-3" {
		t.Errorf("missing PROBE_MODEL_URI: %+v", c.Env)
	}
	if strings.Contains(c.Command[2], "STREAM_ENV_FILE") {
		t.Error("direct-azure has no SAS env file to source")
	}

	script := c.Command[2]
	torchIdx := strings.Index(script, TorchCPUIndexURL)
	streamerIdx := strings.Index(script, "runai-model-streamer[azure]")
	if torchIdx < 0 || streamerIdx < 0 {
		t.Fatalf("direct-azure must install torch and the streamer: %s", script)
	}
	// CPU torch must resolve first, otherwise the streamer install pulls the CUDA
	// build and ~1.3 GB of nvidia-* wheels onto a CPU node.
	if torchIdx > streamerIdx {
		t.Error("torch must be installed from the CPU index before the streamer")
	}
	if !strings.Contains(script, "torch=="+TorchVersion) {
		t.Errorf("torch must be pinned to %s: %s", TorchVersion, script)
	}
}

// ── Probe script ────────────────────────────────────────────────────────────

func TestProbeScriptIsCPUOnly(t *testing.T) {
	for _, forbidden := range []string{"import torch", ".cuda(", `"cuda"`, "'cuda'"} {
		if strings.Contains(streamingProbeScript, forbidden) {
			t.Errorf("probe script must stay CPU-only but contains %q", forbidden)
		}
	}
}

func TestProbeScriptCompiles(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	path := filepath.Join(t.TempDir(), StreamingProbeScriptFileName)
	if err := os.WriteFile(path, []byte(streamingProbeScript), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	if out, err := exec.Command(python, "-m", "py_compile", path).CombinedOutput(); err != nil {
		t.Fatalf("probe script does not compile: %v\n%s", err, out)
	}
}

func TestProbeScriptScrubsQueryStrings(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, StreamingProbeScriptFileName)
	if err := os.WriteFile(path, []byte(streamingProbeScript), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}

	// A SAS token lives in the query string, so _scrub must drop it.
	const prog = `
import sys; sys.path.insert(0, sys.argv[1])
from streaming_probe import _scrub
out = _scrub("https://acct.blob.core.windows.net/c/m.safetensors?sig=SECRET&se=x")
assert "SECRET" not in out, out
assert "sig=" not in out, out
assert "m.safetensors" in out, out
print("ok")
`
	if out, err := exec.Command(python, "-c", prog, dir).CombinedOutput(); err != nil {
		t.Fatalf("scrub check failed: %v\n%s", err, out)
	}
}

// ── ConfigMap ───────────────────────────────────────────────────────────────

func TestProbeScriptConfigMapNameIsContentAddressed(t *testing.T) {
	name := probeScriptConfigMapName("shadow-default-falcon-0")
	if !strings.HasPrefix(name, "shadow-default-falcon-0-probe-") {
		t.Errorf("unexpected name %q", name)
	}
	if name != probeScriptConfigMapName("shadow-default-falcon-0") {
		t.Error("name must be deterministic for the same script")
	}
	if !strings.HasSuffix(name, hash8(streamingProbeScript)) {
		t.Errorf("name %q must end with the script hash", name)
	}
}

func TestEnsureProbeScriptConfigMap(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	r := &ShadowPodReconciler{Client: cl, Config: testConfig()}

	owner := metav1.OwnerReference{APIVersion: "v1", Kind: "Pod", Name: "ws-0", UID: "uid"}
	name := probeScriptConfigMapName("shadow-models-ws-0")
	if err := r.ensureProbeScriptConfigMap(ctx, "models", name, owner); err != nil {
		t.Fatalf("ensureProbeScriptConfigMap: %v", err)
	}

	cm := &corev1.ConfigMap{}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "models", Name: name}, cm); err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	if cm.Data[StreamingProbeScriptFileName] != streamingProbeScript {
		t.Error("configmap does not carry the embedded script")
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].Name != "ws-0" {
		t.Errorf("owner references = %+v", cm.OwnerReferences)
	}

	// Idempotent.
	if err := r.ensureProbeScriptConfigMap(ctx, "models", name, owner); err != nil {
		t.Fatalf("second ensureProbeScriptConfigMap: %v", err)
	}
}

// ── Shadow pod shape ────────────────────────────────────────────────────────

func newProbeReconciler(t *testing.T, cfg Config, objs ...client.Object) (*ShadowPodReconciler, client.Client) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "models"}}
	all := append([]client.Object{ns}, objs...)
	cl := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(all...).Build()
	return &ShadowPodReconciler{Client: cl, Config: cfg}, cl
}

func initContainerNames(pod *corev1.Pod) []string {
	names := make([]string, 0, len(pod.Spec.InitContainers))
	for _, c := range pod.Spec.InitContainers {
		names = append(names, c.Name)
	}
	return names
}

func TestEnsureShadowPod_SASPath(t *testing.T) {
	original := newSASOriginalPod()
	r, cl := newProbeReconciler(t, testConfig(), original)

	shadow, err := r.ensureShadowPod(context.Background(), original, "shadow-models-ws-0")
	if err != nil {
		t.Fatalf("ensureShadowPod: %v", err)
	}

	// fetch-sas must run first: the probe sources the SAS env file it writes.
	got := initContainerNames(shadow)
	want := []string{KaitoSASFetchInitContainerName, StreamingProbeContainerName}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("init containers = %v, want %v", got, want)
	}
	if shadow.Spec.ServiceAccountName != "kaito-model-streamer" {
		t.Errorf("serviceAccountName = %q", shadow.Spec.ServiceAccountName)
	}
	if shadow.Labels[AzureWorkloadIdentityUseLabel] != "true" {
		t.Errorf("missing workload identity label: %v", shadow.Labels)
	}

	vols := map[string]bool{}
	for _, v := range shadow.Spec.Volumes {
		vols[v.Name] = true
	}
	for _, want := range []string{"config", KaitoSASSharedVolumeName, "fetch-sas-script"} {
		if !vols[want] {
			t.Errorf("missing volume %q, have %v", want, vols)
		}
	}
	for _, unwanted := range []string{"cuda-toolkit", "weights", AzureIdentityTokenVolumeName} {
		if vols[unwanted] {
			t.Errorf("GPU-only volume %q leaked into the shadow pod", unwanted)
		}
	}

	cmName := probeScriptConfigMapName("shadow-models-ws-0")
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "models", Name: cmName}, &corev1.ConfigMap{}); err != nil {
		t.Errorf("probe script configmap not created: %v", err)
	}
}

func TestEnsureShadowPod_DirectAzurePath(t *testing.T) {
	original := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ws-0", Namespace: "models",
			Labels: map[string]string{
				InferenceSetCreatedByLabelKey: "phi-3",
				AzureWorkloadIdentityUseLabel: "true",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:           "fake-ws1",
			ServiceAccountName: "kaito-model-streamer",
			Containers: []corev1.Container{{
				Name:  "ws",
				Args:  []string{"--model", "az://weights/phi-3", "--served-model-name", "phi-3"},
				Env:   []corev1.EnvVar{{Name: "AZURE_STORAGE_ACCOUNT_NAME", Value: "mystorage"}},
				Ports: []corev1.ContainerPort{{ContainerPort: 5000}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	r, _ := newProbeReconciler(t, testConfig(), original)

	shadow, err := r.ensureShadowPod(context.Background(), original, "shadow-models-ws-0")
	if err != nil {
		t.Fatalf("ensureShadowPod: %v", err)
	}

	got := initContainerNames(shadow)
	if len(got) != 1 || got[0] != StreamingProbeContainerName {
		t.Fatalf("init containers = %v, want just the probe", got)
	}
	probe := shadow.Spec.InitContainers[0]
	if envValue(probe.Env, "PROBE_MODEL_URI") != "az://weights/phi-3" {
		t.Errorf("probe env = %+v", probe.Env)
	}
	if envValue(probe.Env, "AZURE_STORAGE_ACCOUNT_NAME") != "mystorage" {
		t.Errorf("storage account not propagated: %+v", probe.Env)
	}
}

func TestEnsureShadowPod_UnsupportedSchemeSkipsProbe(t *testing.T) {
	original := newSASOriginalPod()
	original.Spec.InitContainers = nil
	original.Spec.Containers[0].Args = []string{"--model", "s3://bucket/phi-3"}
	r, _ := newProbeReconciler(t, testConfig(), original)

	shadow, err := r.ensureShadowPod(context.Background(), original, "shadow-models-ws-0")
	if err != nil {
		t.Fatalf("ensureShadowPod: %v", err)
	}
	if len(shadow.Spec.InitContainers) != 0 {
		t.Errorf("s3:// is not probed yet, got %v", initContainerNames(shadow))
	}
	if shadow.Spec.ServiceAccountName != "" {
		t.Errorf("serviceAccountName = %q, want empty", shadow.Spec.ServiceAccountName)
	}
}

func TestEnsureShadowPod_NoGPUResources(t *testing.T) {
	original := newSASOriginalPod()
	r, _ := newProbeReconciler(t, testConfig(), original)

	shadow, err := r.ensureShadowPod(context.Background(), original, "shadow-models-ws-0")
	if err != nil {
		t.Fatalf("ensureShadowPod: %v", err)
	}
	all := append(append([]corev1.Container{}, shadow.Spec.InitContainers...), shadow.Spec.Containers...)
	for _, c := range all {
		for _, rl := range []corev1.ResourceList{c.Resources.Requests, c.Resources.Limits} {
			if _, hasGPU := rl["nvidia.com/gpu"]; hasGPU {
				t.Errorf("container %q requests a GPU", c.Name)
			}
		}
	}
	terms := shadow.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if terms[0].MatchExpressions[0].Key != LabelFakeNode {
		t.Error("fake-node anti-affinity must survive the streaming path")
	}
}

func TestFirstUnreadyContainerReason(t *testing.T) {
	// Init containers gate the pod, so they are reported first.
	pod := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name:  StreamingProbeContainerName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "probe failed"}},
		}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "llm-d-inference-sim",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
		}},
	}}
	reason, message := firstUnreadyContainerReason(pod)
	if !strings.Contains(reason, "CrashLoopBackOff") || message != "probe failed" {
		t.Errorf("reason=%q message=%q", reason, message)
	}

	if r, m := firstUnreadyContainerReason(&corev1.Pod{}); r != "" || m != "" {
		t.Errorf("healthy pod should report nothing, got %q/%q", r, m)
	}
}
