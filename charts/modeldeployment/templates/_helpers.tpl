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
Selector labels for the EPP Deployment/Service.
Matches the upstream GAIE chart convention:
  inferencepool: <epp-service-name>
*/}}
{{- define "modeldeployment.eppSelectorLabels" -}}
inferencepool: {{ include "modeldeployment.eppServiceName" . }}
{{- end }}
