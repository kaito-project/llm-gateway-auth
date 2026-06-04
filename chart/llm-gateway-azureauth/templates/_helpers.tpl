{{/*
Expand the name of the chart.
*/}}
{{- define "llm-gateway-azureauth.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "llm-gateway-azureauth.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Namespace into which the chart's namespaced resources are installed.
*/}}
{{- define "llm-gateway-azureauth.namespace" -}}
{{- .Values.namespaceOverride | default .Release.Namespace -}}
{{- end }}

{{/*
Chart label value.
*/}}
{{- define "llm-gateway-azureauth.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to every resource.
*/}}
{{- define "llm-gateway-azureauth.labels" -}}
helm.sh/chart: {{ include "llm-gateway-azureauth.chart" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: {{ include "llm-gateway-azureauth.name" . }}
{{- with .Values.global.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Authz selector labels.
*/}}
{{- define "llm-gateway-azureauth.authz.selectorLabels" -}}
app.kubernetes.io/name: {{ include "llm-gateway-azureauth.fullname" . }}-authz
app.kubernetes.io/component: azure-authz
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Authz ServiceAccount name.
*/}}
{{- define "llm-gateway-azureauth.authz.serviceAccountName" -}}
{{- if .Values.authz.serviceAccount.create }}
{{- default (printf "%s-authz" (include "llm-gateway-azureauth.fullname" .)) .Values.authz.serviceAccount.name }}
{{- else }}
{{- required "authz.serviceAccount.name is required when authz.serviceAccount.create is false" .Values.authz.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Istio sidecar injection labels for pod templates.
*/}}
{{- define "llm-gateway-azureauth.istio.podLabels" -}}
{{- if .enabled }}
sidecar.istio.io/inject: "true"
{{- if ne .revision "default" }}
istio.io/rev: {{ .revision }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Derive the JWT issuer from azure.tenantId + azure.jwt.tokenVersion when not
explicitly set. Fails if azure.tenantId is empty or tokenVersion is invalid.
*/}}
{{- define "llm-gateway-azureauth.jwt.issuer" -}}
{{- $tenant := required "azure.tenantId is required" .Values.azure.tenantId -}}
{{- if .Values.azure.jwt.issuer -}}
{{- .Values.azure.jwt.issuer -}}
{{- else -}}
{{- $ver := .Values.azure.jwt.tokenVersion | default "v1.0" -}}
{{- if eq $ver "v1.0" -}}
{{- printf "https://sts.windows.net/%s/" $tenant -}}
{{- else if eq $ver "v2.0" -}}
{{- printf "https://login.microsoftonline.com/%s/v2.0" $tenant -}}
{{- else -}}
{{- fail "azure.jwt.tokenVersion must be \"v1.0\" or \"v2.0\"" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Derive the JWKS URI from azure.tenantId + azure.jwt.tokenVersion when not
explicitly set.
*/}}
{{- define "llm-gateway-azureauth.jwt.jwksUri" -}}
{{- $tenant := required "azure.tenantId is required" .Values.azure.tenantId -}}
{{- if .Values.azure.jwt.jwksUri -}}
{{- .Values.azure.jwt.jwksUri -}}
{{- else -}}
{{- $ver := .Values.azure.jwt.tokenVersion | default "v1.0" -}}
{{- if eq $ver "v1.0" -}}
{{- printf "https://login.microsoftonline.com/%s/discovery/keys" $tenant -}}
{{- else if eq $ver "v2.0" -}}
{{- printf "https://login.microsoftonline.com/%s/discovery/v2.0/keys" $tenant -}}
{{- else -}}
{{- fail "azure.jwt.tokenVersion must be \"v1.0\" or \"v2.0\"" -}}
{{- end -}}
{{- end -}}
{{- end }}
