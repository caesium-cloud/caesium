{{/* Expand the name of the chart. */}}
{{- define "caesium.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Create a default fully qualified app name. */}}
{{- define "caesium.fullname" -}}
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

{{/* Chart label value. */}}
{{- define "caesium.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Common labels. */}}
{{- define "caesium.labels" -}}
helm.sh/chart: {{ include "caesium.chart" . }}
{{ include "caesium.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/* Labels used by selector objects. */}}
{{- define "caesium.selectorLabels" -}}
app.kubernetes.io/name: {{ include "caesium.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* Service account name. */}}
{{- define "caesium.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "caesium.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* Headless service name. */}}
{{- define "caesium.headlessServiceName" -}}
{{- printf "%s-headless" (include "caesium.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/* Peer discovery config map name. */}}
{{- define "caesium.peerConfigName" -}}
{{- printf "%s-peer-discovery" (include "caesium.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}
