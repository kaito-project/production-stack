{{/*
Resolved namespace. Falls back to .Release.Namespace when .Values.namespace is empty.
*/}}
{{- define "modelharness.namespace" -}}
{{- default .Release.Namespace .Values.namespace -}}
{{- end }}

{{/*
Resolved Gateway name. Falls back to "<namespace>-gw" when .Values.gatewayName is empty.
Keeping the Gateway name keyed off the workload namespace makes it
self-evident in logs/events which namespace owns it; the `-gw` suffix
keeps the Gateway name distinct from the namespace name so the two
never get conflated in selectors / Service names.
*/}}
{{- define "modelharness.gatewayName" -}}
{{- $ns := include "modelharness.namespace" . -}}
{{- default (printf "%s-gw" $ns) .Values.gatewayName -}}
{{- end }}

{{/*
Common labels applied to managed resources.
*/}}
{{- define "modelharness.labels" -}}
app.kubernetes.io/name: modelharness
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
