{{/*
Resolved namespace. Falls back to .Release.Namespace when .Values.namespace is empty.
*/}}
{{- define "modelharness.namespace" -}}
{{- default .Release.Namespace .Values.namespace -}}
{{- end }}

{{/*
Resolved Gateway name. Falls back to "<namespace>-gw" when
.Values.gatewayName is empty. Keying it off the workload namespace makes it
self-evident which namespace owns the Gateway; the `-gw` suffix keeps the
Gateway name distinct from the namespace name.
*/}}
{{- define "modelharness.gatewayName" -}}
{{- $ns := include "modelharness.namespace" . -}}
{{- default (printf "%s-gw" $ns) .Values.gatewayName -}}
{{- end }}

{{/*
Streaming ServiceAccount name. PINNED to the constant "kaito-model-streamer"
and intentionally NOT exposed as a tunable value.
*/}}
{{- define "modelharness.streamingServiceAccountName" -}}
kaito-model-streamer
{{- end }}

{{/*
Common labels applied to every harness-owned resource.

`kaito.sh/owned-by: modelharness` is the stable ownership label the
productionstack-status-reporter keys off to enumerate the objects this chart
owns (Gateway, APIKey, EnvoyFilters, CiliumNetworkPolicy). It is distinct
from the `kaito.sh/owned-by: modeldeployment` label that charts/modeldeployment
stamps on its pods, so the two layers never get conflated.
*/}}
{{- define "modelharness.labels" -}}
app.kubernetes.io/name: modelharness
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
kaito.sh/owned-by: modelharness
{{- end }}

{{/*
Labels stamped on the workload Namespace. In addition to the common
ownership labels, the Namespace carries the
`productionstack.kaito.sh/managed-by: modelharness` discovery label that the
productionstack-status-reporter uses as its namespace label selector; it
ignores any workload Namespace that does not carry this label.
*/}}
{{- define "modelharness.namespaceLabels" -}}
productionstack.kaito.sh/managed-by: modelharness
{{ include "modelharness.labels" . }}
{{- end }}

{{/*
Entra ID JWT issuer URL. Uses azure.entra.jwtEndpoints.issuer when set;
otherwise derives the v1.0 public-cloud default from
azure.entra.tenantId (matches `az account get-access-token`).
*/}}
{{- define "modelharness.azure.entra.jwt.issuer" -}}
{{- with .Values.azure.entra.jwtEndpoints.issuer -}}
{{- . -}}
{{- else -}}
{{- $tenant := required "azure.entra.tenantId is required when jwtEndpoints.issuer is not set" .Values.azure.entra.tenantId -}}
{{- printf "https://sts.windows.net/%s/" $tenant -}}
{{- end -}}
{{- end }}

{{/*
Entra ID JWKS URI. Uses azure.entra.jwtEndpoints.jwksUri when set;
otherwise derives the v1.0 public-cloud default from
azure.entra.tenantId.
*/}}
{{- define "modelharness.azure.entra.jwt.jwksUri" -}}
{{- with .Values.azure.entra.jwtEndpoints.jwksUri -}}
{{- . -}}
{{- else -}}
{{- $tenant := required "azure.entra.tenantId is required when jwtEndpoints.jwksUri is not set" .Values.azure.entra.tenantId -}}
{{- printf "https://login.microsoftonline.com/%s/discovery/keys" $tenant -}}
{{- end -}}
{{- end }}
