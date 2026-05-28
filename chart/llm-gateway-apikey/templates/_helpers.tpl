{{/*
Expand the name of the chart.
*/}}
{{- define "llm-gateway-apikey.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "llm-gateway-apikey.fullname" -}}
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
Defaults to .Release.Namespace, but can be overridden via .Values.namespaceOverride
to install the apikey subchart into a different namespace than the parent release.
*/}}
{{- define "llm-gateway-apikey.namespace" -}}
{{- .Values.namespaceOverride | default .Release.Namespace -}}
{{- end }}

{{/*
Chart label value.
*/}}
{{- define "llm-gateway-apikey.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to every resource.
*/}}
{{- define "llm-gateway-apikey.labels" -}}
helm.sh/chart: {{ include "llm-gateway-apikey.chart" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: {{ include "llm-gateway-apikey.name" . }}
{{- with .Values.global.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Operator selector labels.
*/}}
{{- define "llm-gateway-apikey.operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "llm-gateway-apikey.fullname" . }}-operator
app.kubernetes.io/component: apikey-operator
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Authz selector labels.
*/}}
{{- define "llm-gateway-apikey.authz.selectorLabels" -}}
app.kubernetes.io/name: {{ include "llm-gateway-apikey.fullname" . }}-authz
app.kubernetes.io/component: apikey-authz
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Operator ServiceAccount name.
*/}}
{{- define "llm-gateway-apikey.operator.serviceAccountName" -}}
{{- if .Values.operator.serviceAccount.create }}
{{- default (printf "%s-operator" (include "llm-gateway-apikey.fullname" .)) .Values.operator.serviceAccount.name }}
{{- else }}
{{- required "operator.serviceAccount.name is required when operator.serviceAccount.create is false" .Values.operator.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Authz ServiceAccount name.
*/}}
{{- define "llm-gateway-apikey.authz.serviceAccountName" -}}
{{- if .Values.authz.serviceAccount.create }}
{{- default (printf "%s-authz" (include "llm-gateway-apikey.fullname" .)) .Values.authz.serviceAccount.name }}
{{- else }}
{{- required "authz.serviceAccount.name is required when authz.serviceAccount.create is false" .Values.authz.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Resolve image tag — falls back to .Chart.AppVersion when empty.
*/}}
{{- define "llm-gateway-apikey.imageTag" -}}
{{- . | default $.Chart.AppVersion }}
{{- end }}

{{/*
Resolve pull policy — component override > global.
*/}}
{{- define "llm-gateway-apikey.pullPolicy" -}}
{{- $component := index . 0 -}}
{{- $global := index . 1 -}}
{{- default $global $component }}
{{- end }}

{{/*
Istio sidecar injection labels for pod templates.
Outputs nothing when istio.enabled is false.
*/}}
{{- define "llm-gateway-apikey.istio.podLabels" -}}
{{- if .enabled }}
sidecar.istio.io/inject: "true"
{{- if ne .revision "default" }}
istio.io/rev: {{ .revision }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Istio sidecar disabled label for pods that should NOT get a sidecar.
*/}}
{{- define "llm-gateway-apikey.istio.podLabelsDisabled" -}}
{{- if .enabled }}
sidecar.istio.io/inject: "false"
{{- end }}
{{- end }}
