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

{{/*
The platform identity contract (global.identity), an empty dict when absent.
*/}}
{{- define "agent-manager.globalIdentity" -}}
{{- dig "identity" (dict) (.Values.global | default dict) | toJson }}
{{- end }}

{{/*
Whether the ServiceAccount holds any RBAC: not with downstream OAuth, where
every Kubernetes call carries the caller's token.
*/}}
{{- define "agent-manager.serviceAccountRBAC" -}}
{{- if and .Values.rbac.create (not (and .Values.oauth.enabled .Values.oauth.downstream.enabled)) }}true{{ end }}
{{- end }}

{{/*
Existing Secret with the provider credentials: oauth.existingSecret, else the
platform's global.identity.existingSecret, else the chart-rendered one.
*/}}
{{- define "agent-manager.oauthSecretName" -}}
{{- $g := include "agent-manager.globalIdentity" . | fromJson -}}
{{- .Values.oauth.existingSecret | default (dig "existingSecret" "" $g) | default (printf "%s-oauth" (include "agent-manager.fullname" .)) }}
{{- end }}

{{/*
Whether the chart renders its own OAuth Secret (no existing one named).
*/}}
{{- define "agent-manager.oauthRendersSecret" -}}
{{- $g := include "agent-manager.globalIdentity" . | fromJson -}}
{{- if and .Values.oauth.enabled (not .Values.oauth.existingSecret) (not (dig "existingSecret" "" $g)) }}true{{ end }}
{{- end }}

{{/*
OAuth base URL: oauth.baseURL, else https://<fullname>.<global.domain>.
*/}}
{{- define "agent-manager.oauthBaseURL" -}}
{{- $domain := dig "domain" "" (.Values.global | default dict) -}}
{{- $derived := "" -}}
{{- if $domain }}{{ $derived = printf "https://%s.%s" (include "agent-manager.fullname" .) $domain }}{{ end -}}
{{- required "oauth.baseURL is required when oauth.enabled (or set global.domain)" (.Values.oauth.baseURL | default $derived) }}
{{- end }}

{{/*
Dex issuer / client id with the global.identity fallbacks.
*/}}
{{- define "agent-manager.oauthDexIssuerURL" -}}
{{- $g := include "agent-manager.globalIdentity" . | fromJson -}}
{{- required "oauth.dex.issuerURL (or global.identity.issuerUrl) is required for the dex provider" (.Values.oauth.dex.issuerURL | default (dig "issuerUrl" "" $g)) }}
{{- end }}

{{- define "agent-manager.oauthDexClientID" -}}
{{- $g := include "agent-manager.globalIdentity" . | fromJson -}}
{{- required "oauth.dex.clientID (or global.identity.clientId) is required for the dex provider" (.Values.oauth.dex.clientID | default (dig "clientId" "" $g)) }}
{{- end }}

{{/*
CA Secret of a private-certificate Dex: oauth.dex.caSecret, else
global.identity.ca. Name empty means system trust.
*/}}
{{- define "agent-manager.oauthDexCASecretName" -}}
{{- $g := include "agent-manager.globalIdentity" . | fromJson -}}
{{- .Values.oauth.dex.caSecret.name | default (dig "ca" "secretName" "" $g) }}
{{- end }}

{{- define "agent-manager.oauthDexCASecretKey" -}}
{{- $g := include "agent-manager.globalIdentity" . | fromJson -}}
{{- if .Values.oauth.dex.caSecret.name }}{{ .Values.oauth.dex.caSecret.key | default "ca.crt" }}{{ else }}{{ dig "ca" "key" "" $g | default .Values.oauth.dex.caSecret.key | default "ca.crt" }}{{ end }}
{{- end }}

{{/*
Trusted audiences, comma-separated: oauth.trustedAudiences, else the platform
client id.
*/}}
{{- define "agent-manager.oauthTrustedAudiences" -}}
{{- $g := include "agent-manager.globalIdentity" . | fromJson -}}
{{- if .Values.oauth.trustedAudiences }}{{ join "," .Values.oauth.trustedAudiences }}{{ else }}{{ dig "clientId" "" $g }}{{ end }}
{{- end }}
