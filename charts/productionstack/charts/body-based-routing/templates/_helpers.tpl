{{/*
Common labels stamped on every chart-rendered object so cluster
operators can list / select all BBR resources with a single label
selector (e.g. `kubectl -n istio-system get all -l app.kubernetes.io/name=body-based-routing`).
*/}}
{{- define "bbr.labels" -}}
app.kubernetes.io/name: body-based-routing
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}

{{/*
Selector labels — the subset of `bbr.labels` that is stable across
rolling upgrades (so the Deployment/Service selectors are not
invalidated when the chart version bumps).
*/}}
{{- define "bbr.selectorLabels" -}}
app: {{ .Values.bbr.name }}
app.kubernetes.io/name: body-based-routing
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name. Suffixed with the Helm release name so multiple
copies of the chart cannot collide on a single SA in the same
namespace (defense in depth — the chart itself is intended to be
installed at most once per cluster, but this keeps `helm install`
idempotent across release-name changes during development).
*/}}
{{- define "bbr.serviceAccountName" -}}
{{ .Values.bbr.name }}-{{ .Release.Name }}
{{- end }}

{{/*
Install namespace for every namespaced resource rendered by this chart.

Defaults to the Helm release namespace, but can be overridden via
`namespaceOverride` so the chart can be installed into a different
namespace than the surrounding umbrella release (e.g. forced into
`istio-system` when the parent chart is installed elsewhere). When
overridden, the target namespace MUST already exist — `helm install
--create-namespace` only creates the release namespace.
*/}}
{{- define "bbr.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end }}
