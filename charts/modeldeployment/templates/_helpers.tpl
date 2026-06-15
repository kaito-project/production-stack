{{/*
Resolved deployment name. Falls back to .Release.Name when .Values.name is empty.
*/}}
{{- define "modeldeployment.name" -}}
{{- default .Release.Name .Values.name -}}
{{- end }}

{{/*
Resolved namespace. Falls back to .Release.Namespace when .Values.namespace is empty.
*/}}
{{- define "modeldeployment.namespace" -}}
{{- default .Release.Namespace .Values.namespace -}}
{{- end }}

{{/*
Resolved Gateway name. Falls back to "<namespace>-gw" when
.Values.gatewayName is empty, matching the convention used by
charts/modelharness so per-deployment HTTPRoutes attach to the same
Gateway that modelharness provisions in the workload namespace.
*/}}
{{- define "modeldeployment.gatewayName" -}}
{{- $ns := include "modeldeployment.namespace" . -}}
{{- default (printf "%s-gw" $ns) .Values.gatewayName -}}
{{- end }}

{{/*
Derived InferencePool name, matching KAITO's naming convention.
*/}}
{{- define "modeldeployment.inferencePoolName" -}}
{{ include "modeldeployment.name" . }}-inferencepool
{{- end }}

{{/*
Derived EPP service name (used as the DestinationRule host).
*/}}
{{- define "modeldeployment.eppServiceName" -}}
{{ include "modeldeployment.inferencePoolName" . }}-epp
{{- end }}

{{/*
HTTPRoute name.
*/}}
{{- define "modeldeployment.httpRouteName" -}}
{{ include "modeldeployment.name" . }}-route
{{- end }}

{{/*
Common labels applied to managed resources.
*/}}
{{- define "modeldeployment.labels" -}}
app.kubernetes.io/name: {{ include "modeldeployment.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Stable identifying labels stamped on every chart-owned object's
metadata.labels (EPP Deployment/Service, HTTPRoute, InferencePool,
ConfigMap, and the InferenceSet itself). These give the
productionstack-status-reporter a deterministic way to correlate an
object back to the modeldeployment release that owns it:

  * kaito.sh/inferenceset: <name>  — the owning InferenceSet / deployment name.
  * kaito.sh/owned-by: modeldeployment — the owning layer, value-free and stable.

Distinct from `modeldeployment.ownerLabel` (which carries only the
`kaito.sh/owned-by` pod-selector label stamped additively into pod
template metadata for the modelharness NetworkPolicies): these labels
also pin the per-object `kaito.sh/inferenceset` identifier and are
applied to object metadata rather than pod templates.
*/}}
{{- define "modeldeployment.identifyingLabels" -}}
kaito.sh/inferenceset: {{ include "modeldeployment.name" . }}
kaito.sh/owned-by: modeldeployment
{{- end }}

{{/*
Ownership label stamped on every pod that production-stack owns —
EPP pods rendered by this chart, inference workload pods that the
KAITO InferenceSet / Workspace controller renders on our behalf,
and keda-kaito-scaler shadow pods. The modelharness NetworkPolicies
positively select on this label so they isolate inference workloads
without sweeping in unrelated user pods that happen to share the
workload namespace.

The value is intentionally stable and value-free ("modeldeployment")
so all production-stack-owned pods carry the same label regardless
of which release rendered them.
*/}}
{{- define "modeldeployment.ownerLabel" -}}
kaito.sh/owned-by: modeldeployment
{{- end }}

{{/*
Selector labels for the EPP Deployment/Service.
Matches the upstream GAIE chart convention:
  inferencepool: <epp-service-name>
*/}}
{{- define "modeldeployment.eppSelectorLabels" -}}
inferencepool: {{ include "modeldeployment.eppServiceName" . }}
{{- end }}
