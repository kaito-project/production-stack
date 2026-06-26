{{- define "gpu-node-mocker.name" -}}
gpu-node-mocker
{{- end }}

{{/*
Fixed object name. Pinned to "gpu-node-mocker" (rather than .Release.Name) so
the chart is safe to consume as a subchart of the productionstack umbrella —
the auto-generated ClusterRole hardcodes this name, and a release-name-based
fullname would mismatch the ClusterRoleBinding's roleRef.
*/}}
{{- define "gpu-node-mocker.fullname" -}}
gpu-node-mocker
{{- end }}

{{/*
Install namespace for namespaced resources. Defaults to the release namespace;
override via `namespaceOverride` when installed as a subchart so the mocker
can be pinned to a namespace independent of the umbrella release.
*/}}
{{- define "gpu-node-mocker.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end }}

{{- define "gpu-node-mocker.labels" -}}
app.kubernetes.io/name: {{ include "gpu-node-mocker.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "gpu-node-mocker.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gpu-node-mocker.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
