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
Resolved Gateway name. Falls back to the workload namespace when
.Values.gatewayName is empty, matching the convention used by
charts/modelharness so per-deployment HTTPRoutes attach to the same
Gateway that modelharness provisions in the workload namespace.
*/}}
{{- define "modeldeployment.gatewayName" -}}
{{- $ns := include "modeldeployment.namespace" . -}}
{{- default $ns .Values.gatewayName -}}
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

{{/*
Optional scalar rendered as a string; "" when the value is unset (nil) or
empty. Unlike a plain truthiness check, a numeric 0 renders as "0" so a
threshold of 0 counts as set.
*/}}
{{- define "modeldeployment.optionalString" -}}
{{- if not (kindIs "invalid" .) -}}
{{- toString . -}}
{{- end -}}
{{- end }}

{{/*
Validates an optional whole number of seconds against [min, max] and renders it
without leading zeros; "" when unset. Input: dict "field" "value" "min" "max".
Complements values.schema.json, which cannot bound numeric strings (e.g. from
--set-string), so out-of-range values fail at render time instead of when KEDA
creates the HPA.
*/}}
{{- define "modeldeployment.boundedSeconds" -}}
{{- $v := include "modeldeployment.optionalString" .value -}}
{{- if ne $v "" -}}
{{- if not (regexMatch "^[0-9]+$" $v) -}}
{{- fail (printf "modeldeployment: %s (%s) must be a whole number of seconds" .field $v) -}}
{{- end -}}
{{- $n := regexReplaceAll "^0+(.)" $v "${1}" -}}
{{- if or (gt (len $n) 10) (lt (atoi $n) .min) (gt (atoi $n) .max) -}}
{{- fail (printf "modeldeployment: %s (%s) must be between %d and %d seconds" .field $v .min .max) -}}
{{- end -}}
{{- $n -}}
{{- end -}}
{{- end }}

{{/*
Validates an optional HPA tolerance ratio: a non-negative decimal strictly
below "max", the ratio at or above which the scaling formula's signal for that
direction (2.0 up, 0.5 down) falls inside the deadband and never fires; ""
when unset. Input: dict "field" "value" "max".
*/}}
{{- define "modeldeployment.boundedTolerance" -}}
{{- $v := include "modeldeployment.optionalString" .value -}}
{{- if ne $v "" -}}
{{- if not (regexMatch "^[0-9]+(\\.[0-9]+)?$" $v) -}}
{{- fail (printf "modeldeployment: %s (%s) must be a non-negative decimal" .field $v) -}}
{{- end -}}
{{- if ge (float64 $v) .max -}}
{{- fail (printf "modeldeployment: %s (%s) must be less than %v, otherwise that scaling direction never triggers" .field $v .max) -}}
{{- end -}}
{{- $v -}}
{{- end -}}
{{- end }}

{{/*
Autoscaling lower bound wired to scaledobject.kaito.sh/min-replicas, as a
string; "" when minReplicas is unset or empty. Required (enforced in
inferenceset.yaml) whenever enableScaling is true.
*/}}
{{- define "modeldeployment.minReplicas" -}}
{{- include "modeldeployment.optionalString" .Values.minReplicas -}}
{{- end }}

{{/*
"true" when autoscaling is enabled with a zero lower bound, i.e.
keda-kaito-scaler drives the 0 <-> 1 transition.
*/}}
{{- define "modeldeployment.scaleToZero" -}}
{{- if and .Values.enableScaling (eq (include "modeldeployment.minReplicas" .) "0") -}}
true
{{- end -}}
{{- end }}

{{/*
"true" when the EPP runs with its flowControl feature gate. Scale to zero
forces it on: with no model pods the EPP must hold incoming requests
(rather than fail them) so the flow-control queue gauge can wake the
workload, and the held requests are dispatched once a replica is Ready.
*/}}
{{- define "modeldeployment.eppFlowControlEnabled" -}}
{{- if or ((.Values.epp).flowControl).enabled (include "modeldeployment.scaleToZero" .) -}}
true
{{- end -}}
{{- end }}

{{/*
EPP flow-control settings. Rendered into the EPP ConfigMap and hashed into
the EPP pod template so a change rolls the EPP (it only reads its config
file at startup).
*/}}
{{- define "modeldeployment.eppFlowControlSettings" -}}
{{- $fc := (.Values.epp).flowControl | default dict -}}
maxConcurrency: {{ $fc.maxConcurrency | default 1000000 | int64 }}
{{- with $fc.maxBytes }}
maxBytes: {{ . | quote }}
{{- end }}
{{- with $fc.defaultRequestTTL }}
defaultRequestTTL: {{ . | quote }}
{{- end }}
{{- end }}
