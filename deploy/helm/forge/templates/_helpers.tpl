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
{{- include "forge.imageRef" (dict "global" .Values.global "image" (merge (dict "tag" (.Values.image.tag | default .Chart.AppVersion)) (omit .Values.image "tag"))) }}
{{- end }}

{{/*
forge.imageRef renders an image reference: [registry/]repository:tag[@digest].
global.imageRegistry, when set, replaces every image's own registry, so an
air-gapped install mirrors the images under the same paths into one registry
and sets that one value. forge-stack's templates use it too (named templates
are shared with the parent chart).

  {{ include "forge.imageRef" (dict "global" .Values.global "image" .Values.x.image) }}
*/}}
{{- define "forge.imageRef" -}}
{{- $img := .image -}}
{{- $registry := $img.registry | default "" -}}
{{- with .global }}{{ with .imageRegistry }}{{ $registry = . }}{{ end }}{{ end -}}
{{- $ref := $img.repository -}}
{{- if $registry }}{{ $ref = printf "%s/%s" $registry $img.repository }}{{ end -}}
{{- if $img.tag }}{{ $ref = printf "%s:%s" $ref (toString $img.tag) }}{{ end -}}
{{- if $img.digest }}{{ $ref = printf "%s@%s" $ref $img.digest }}{{ end -}}
{{- $ref -}}
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
