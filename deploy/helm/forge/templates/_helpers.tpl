{{/*
Expand the name of the chart.
*/}}
{{- define "forge.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this
(by the DNS naming spec).
*/}}
{{- define "forge.fullname" -}}
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
Create chart name and version as used by the chart label.
*/}}
{{- define "forge.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "forge.labels" -}}
helm.sh/chart: {{ include "forge.chart" . }}
{{ include "forge.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "forge.selectorLabels" -}}
app.kubernetes.io/name: {{ include "forge.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Service account name
*/}}
{{- define "forge.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "forge.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Container image reference, defaulting tag to appVersion.
*/}}
{{- define "forge.image" -}}
{{- printf "%s:%s" .Values.image.repository ((.Values.image.tag | default .Chart.AppVersion)) }}
{{- end }}

{{/*
Config-as-code filename. forge picks its parser from the extension, so the
ConfigMap key doubles as the format selector:
  - config.content as a string      -> forge.config.json (legacy inline JSON)
  - config.content as a map         -> forge.config.yaml (authored in values.yaml)
  - config.existingConfigMap        -> config.existingConfigMapKey, default JSON
*/}}
{{- define "forge.configFileName" -}}
{{- if .Values.config.existingConfigMap -}}
{{- .Values.config.existingConfigMapKey | default "forge.config.json" -}}
{{- else if kindIs "string" .Values.config.content -}}
forge.config.json
{{- else -}}
forge.config.yaml
{{- end -}}
{{- end -}}
