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
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// streamingProbeScript is the probe body, delivered to the probe init container
// via a per-shadow-pod ConfigMap. Same pattern KAITO uses for fetch_sas.py.
//
//go:embed streaming_probe.py
var streamingProbeScript string

// streamingMode classifies how (or whether) an original KAITO pod streams its
// model weights from object storage.
type streamingMode string

const (
	streamingModeNone        streamingMode = "none"
	streamingModeSAS         streamingMode = "sas"
	streamingModeDirectAzure streamingMode = "direct-azure"
	streamingModeUnsupported streamingMode = "unsupported-scheme"
)

// azureBlobScheme and the unsupported schemes below are the object-store URI
// prefixes KAITO passes to vLLM's --model on a streaming pod.
const (
	azureBlobScheme = "az://"
	s3Scheme        = "s3://"
	gcsScheme       = "gs://"
)

// azureWorkloadIdentityEnvVars are injected into the ORIGINAL pod by the
// azure-workload-identity mutating webhook. They are stripped when cloning so
// the webhook re-injects them cleanly into the shadow pod, which makes the
// clone correct whether or not the webhook is idempotent.
var azureWorkloadIdentityEnvVars = map[string]struct{}{
	"AZURE_CLIENT_ID":            {},
	"AZURE_TENANT_ID":            {},
	"AZURE_FEDERATED_TOKEN_FILE": {},
	"AZURE_AUTHORITY_HOST":       {},
}

// workloadIdentityPodLabels are the provider labels that mark a pod as carrying
// a streaming identity. Only these are copied onto the shadow pod.
var workloadIdentityPodLabels = []string{AzureWorkloadIdentityUseLabel}

// detectStreamingMode classifies an original KAITO pod from its spec alone.
//
// Annotations are never consulted: KAITO stamps the streaming annotations on the
// Workspace, and GenerateStatefulSetManifest copies only labels onto the pod
// template, so they never reach the Pod.
//
// Deliberately does NOT look at --load-format. KAITO also sets
// --load-format=runai_streamer on its local-weights path, where the weights
// already sit on the node's disk and --model is a plain directory. Keying on
// that flag would classify such a pod as streaming and send the probe after a
// path the shadow pod does not have.
func detectStreamingMode(pod *corev1.Pod) streamingMode {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == KaitoSASFetchInitContainerName {
			return streamingModeSAS
		}
	}
	// Same first-match-across-containers rule as extractModelName.
	switch model := extractModelName(pod); {
	case strings.HasPrefix(model, azureBlobScheme):
		return streamingModeDirectAzure
	case strings.HasPrefix(model, s3Scheme), strings.HasPrefix(model, gcsScheme):
		return streamingModeUnsupported
	default:
		return streamingModeNone
	}
}

// streamingIdentity is the material cloned from a streaming original pod so the
// shadow pod can authenticate to object storage exactly as the real pod would.
type streamingIdentity struct {
	mode           streamingMode
	serviceAccount string
	podLabels      map[string]string
	initContainers []corev1.Container
	volumes        []corev1.Volume
	modelURI       string
	providerEnv    []corev1.EnvVar
	// sasEnvFilePath and sasMountPath are read off the cloned fetch-sas
	// container rather than assumed, so the probe reads the SAS env file from
	// wherever fetch-sas actually writes it.
	sasEnvFilePath string
	sasMountPath   string
}

// buildStreamingIdentity clones the streaming identity from the original pod.
//
// The original pod is bound to a fake node, so its init containers never ran and
// the SAS env file they would have written does not exist. What is cloned is the
// container SPEC; the shadow pod's own copy runs for real and mints a fresh SAS
// into the shadow pod's own volume.
func buildStreamingIdentity(original *corev1.Pod, mode streamingMode) streamingIdentity {
	id := streamingIdentity{mode: mode}

	id.podLabels = map[string]string{}
	for _, key := range workloadIdentityPodLabels {
		if v, ok := original.Labels[key]; ok && v != "" {
			id.podLabels[key] = v
		}
	}

	// Only assume the original's ServiceAccount when the pod is marked as a
	// workload identity. That SA is federated to a real cloud identity, so the
	// clone is deliberately narrowed to pods KAITO already flagged as streaming
	// rather than any SA on any pod.
	if len(id.podLabels) > 0 {
		id.serviceAccount = original.Spec.ServiceAccountName
	}

	switch mode {
	case streamingModeSAS:
		for i := range original.Spec.InitContainers {
			c := original.Spec.InitContainers[i]
			if c.Name != KaitoSASFetchInitContainerName {
				continue
			}
			cloned := sanitizeClonedContainer(*c.DeepCopy())
			id.sasEnvFilePath = envValue(cloned.Env, KaitoSASEnvFileEnvVar)
			id.sasMountPath = mountPath(cloned.VolumeMounts, KaitoSASSharedVolumeName)
			id.initContainers = append(id.initContainers, cloned)
			id.volumes = volumesForMounts(original, cloned.VolumeMounts)
			break
		}
	case streamingModeDirectAzure:
		id.modelURI = extractModelName(original)
		for i := range original.Spec.Containers {
			if v := envValue(original.Spec.Containers[i].Env, "AZURE_STORAGE_ACCOUNT_NAME"); v != "" {
				id.providerEnv = append(id.providerEnv, corev1.EnvVar{
					Name: "AZURE_STORAGE_ACCOUNT_NAME", Value: v,
				})
				break
			}
		}
	}

	return id
}

// sanitizeClonedContainer removes the state the azure-workload-identity webhook
// injected into the original pod, so the webhook can re-inject it into the
// shadow pod from a clean base.
func sanitizeClonedContainer(c corev1.Container) corev1.Container {
	env := make([]corev1.EnvVar, 0, len(c.Env))
	for _, e := range c.Env {
		if _, injected := azureWorkloadIdentityEnvVars[e.Name]; !injected {
			env = append(env, e)
		}
	}
	c.Env = env

	mounts := make([]corev1.VolumeMount, 0, len(c.VolumeMounts))
	for _, m := range c.VolumeMounts {
		if m.Name != AzureIdentityTokenVolumeName {
			mounts = append(mounts, m)
		}
	}
	c.VolumeMounts = mounts
	return c
}

// volumesForMounts returns the original pod's volumes that the given mounts
// reference. Selecting by mount rather than copying spec.Volumes wholesale keeps
// the original's GPU-node storage out of a shadow pod that runs on a CPU node.
func volumesForMounts(original *corev1.Pod, mounts []corev1.VolumeMount) []corev1.Volume {
	wanted := make(map[string]struct{}, len(mounts))
	for _, m := range mounts {
		wanted[m.Name] = struct{}{}
	}
	var out []corev1.Volume
	for i := range original.Spec.Volumes {
		if _, ok := wanted[original.Spec.Volumes[i].Name]; ok {
			out = append(out, *original.Spec.Volumes[i].DeepCopy())
		}
	}
	return out
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, e := range env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func mountPath(mounts []corev1.VolumeMount, name string) string {
	for _, m := range mounts {
		if m.Name == name {
			return m.MountPath
		}
	}
	return ""
}

// buildProbeContainer returns the probe init container. It runs after any cloned
// credential-bootstrap container and before the simulator, so a streaming
// misconfiguration keeps the shadow pod — and therefore the mocked inference pod
// — from ever going Ready.
func buildProbeContainer(id streamingIdentity, cfg Config, scriptCM string) corev1.Container {
	env := []corev1.EnvVar{
		{Name: "PROBE_MODE", Value: string(id.mode)},
		{Name: "PROBE_TIMEOUT_SECONDS", Value: strconv.Itoa(cfg.StreamingProbeTimeoutSec)},
		{Name: "RUNAI_STREAMER_MEMORY_LIMIT", Value: strconv.FormatInt(cfg.StreamingProbeStreamerMemLimit, 10)},
	}
	env = append(env, id.providerEnv...)

	mounts := []corev1.VolumeMount{
		{Name: scriptCM, MountPath: StreamingProbeMountPath, ReadOnly: true},
	}

	if id.mode == streamingModeSAS {
		env = append(env, corev1.EnvVar{Name: KaitoSASEnvFileEnvVar, Value: id.sasEnvFilePath})
		if id.sasMountPath != "" {
			mounts = append(mounts, corev1.VolumeMount{
				Name: KaitoSASSharedVolumeName, MountPath: id.sasMountPath,
			})
		}
	} else if id.modelURI != "" {
		env = append(env, corev1.EnvVar{Name: "PROBE_MODEL_URI", Value: id.modelURI})
	}

	quantity := func(s, fallback string) resource.Quantity {
		q, err := resource.ParseQuantity(s)
		if err != nil {
			return resource.MustParse(fallback)
		}
		return q
	}
	limits := corev1.ResourceList{
		corev1.ResourceCPU:    quantity(cfg.StreamingProbeCPU, DefaultStreamingProbeCPU),
		corev1.ResourceMemory: quantity(cfg.StreamingProbeMemory, DefaultStreamingProbeMemory),
	}
	memReq := resource.MustParse(DefaultStreamingProbeMemoryRequest)
	if memLimit := limits[corev1.ResourceMemory]; memReq.Cmp(memLimit) > 0 {
		memReq = memLimit
	}
	cpuReq := resource.MustParse(DefaultStreamingProbeCPURequest)
	if cpuLimit := limits[corev1.ResourceCPU]; cpuReq.Cmp(cpuLimit) > 0 {
		cpuReq = cpuLimit
	}
	requests := corev1.ResourceList{
		corev1.ResourceCPU:    cpuReq,
		corev1.ResourceMemory: memReq,
	}

	return corev1.Container{
		Name:            StreamingProbeContainerName,
		Image:           cfg.StreamingProbeImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "-c", probeShellCommand(id.mode)},
		Env:             env,
		VolumeMounts:    mounts,
		Resources:       corev1.ResourceRequirements{Requests: requests, Limits: limits},
	}
}

// probeShellCommand builds the probe entrypoint.
//
// The SAS env file is sourced under `set -a`, mirroring KAITO's own
// export_sas_token_for_streaming.sh: it is written as shell-quoted KEY='value'
// lines, and auto-export picks up whatever fetch-sas wrote without this having
// to track the variable names. Unlike that wrapper we fail on a missing file
// rather than continuing, because a probe that skips the credentials is not
// validating anything.
//
// torch is installed FIRST, pinned, and from the CPU-only index. runai-model-
// streamer hard-depends on torch, and resolving it from PyPI would pull ~1.3 GB
// of CUDA wheels onto a CPU node. The second install omits --index-url so it
// resolves from PyPI with torch already satisfied.
//
// The SAS mode installs nothing at all: it lists the container over plain HTTPS
// with the standard library, so it needs neither torch nor the streamer.
func probeShellCommand(mode streamingMode) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	if mode == streamingModeSAS {
		b.WriteString(`if [ ! -f "$STREAM_ENV_FILE" ]; then` + "\n")
		b.WriteString(`  echo "ERROR: SAS env file $STREAM_ENV_FILE not found" >&2; exit 1` + "\n")
		b.WriteString("fi\n")
		b.WriteString("set -a\n")
		b.WriteString(`. "$STREAM_ENV_FILE"` + "\n")
		b.WriteString("set +a\n")
	} else {
		fmt.Fprintf(&b, "pip install --no-cache-dir -q --index-url %s torch==%s\n", TorchCPUIndexURL, TorchVersion)
		fmt.Fprintf(&b, "pip install --no-cache-dir -q 'runai-model-streamer[azure]==%s'\n", RunAIStreamerVersion)
	}
	fmt.Fprintf(&b, "exec python3 %s/%s\n", StreamingProbeMountPath, StreamingProbeScriptFileName)
	return b.String()
}

// probeScriptConfigMapName is content-addressed: the controller may create
// ConfigMaps but not update them, so a changed script has to land on a new
// object rather than silently reuse a stale one.
func probeScriptConfigMapName(shadowName string) string {
	name := fmt.Sprintf("%s-probe-%s", shadowName, hash8(streamingProbeScript))
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}

func hash8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// ensureProbeScriptConfigMap creates the ConfigMap carrying the probe script.
// The name is content-addressed, so an existing object always has the right
// content and can be reused as-is.
func (r *ShadowPodReconciler) ensureProbeScriptConfigMap(ctx context.Context, namespace, name string, ownerRef metav1.OwnerReference) error {
	existing := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("get probe configmap %s: %w", name, err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          map[string]string{LabelManagedBy: ControllerName},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Data: map[string]string{StreamingProbeScriptFileName: streamingProbeScript},
	}
	if err := r.Create(ctx, cm); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create probe configmap %s: %w", name, err)
	}
	return nil
}
