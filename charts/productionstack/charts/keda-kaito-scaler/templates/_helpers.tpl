{{/*
Expand the name of the chart.
*/}}
{{- define "keda-kaito-scaler.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "keda-kaito-scaler.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "keda-kaito-scaler.labels" -}}
helm.sh/chart: {{ include "keda-kaito-scaler.chart" . }}
{{ include "keda-kaito-scaler.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "keda-kaito-scaler.selectorLabels" -}}
app.kubernetes.io/name: {{ include "keda-kaito-scaler.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Install namespace for every namespaced resource rendered by this chart.

Defaults to the Helm release namespace, but can be overridden via
`namespaceOverride` so the chart can be installed into a different
namespace than the surrounding umbrella release (e.g. pinned to
`keda` when the parent chart is installed elsewhere). When
overridden, the target namespace MUST already exist — `helm install
--create-namespace` only creates the release namespace.
*/}}
{{- define "keda-kaito-scaler.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end }}