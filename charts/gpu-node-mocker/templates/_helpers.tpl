{{- define "gpu-node-mocker.name" -}}
gpu-node-mocker
{{- end }}

{{- define "gpu-node-mocker.fullname" -}}
{{ .Release.Name }}
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
