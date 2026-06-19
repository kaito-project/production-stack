{{- define "productionstack-status-reporter.name" -}}
productionstack-status-reporter
{{- end }}

{{- define "productionstack-status-reporter.fullname" -}}
{{- printf "%s-status-reporter" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/*
Resolved install namespace: namespaceOverride when set, otherwise the
umbrella release namespace.
*/}}
{{- define "productionstack-status-reporter.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end }}

{{- define "productionstack-status-reporter.labels" -}}
app.kubernetes.io/name: {{ include "productionstack-status-reporter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: status-reporter
{{- end }}

{{- define "productionstack-status-reporter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "productionstack-status-reporter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
