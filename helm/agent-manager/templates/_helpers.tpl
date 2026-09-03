{{/*
Expand the name of the chart.
*/}}
{{- define "agent-manager.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "agent-manager.fullname" -}}
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
Chart label value.
*/}}
{{- define "agent-manager.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "agent-manager.labels" -}}
helm.sh/chart: {{ include "agent-manager.chart" . }}
{{ include "agent-manager.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
application.giantswarm.io/team: {{ index .Chart.Annotations "io.giantswarm.application.team" | quote }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "agent-manager.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agent-manager.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "agent-manager.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "agent-manager.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Every namespace agents may live in: kagent.namespace plus the additional ones,
deduplicated. Rendered as a JSON list.
*/}}
{{- define "agent-manager.managedNamespaces" -}}
{{- $all := list .Values.kagent.namespace -}}
{{- range .Values.kagent.additionalNamespaces -}}
{{- $all = append $all . -}}
{{- end -}}
{{- $all | uniq | toJson -}}
{{- end }}
