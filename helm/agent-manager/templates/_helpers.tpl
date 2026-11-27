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
Chart label value. A label value must begin and end alphanumeric: the
63-character cut of a dev version (0.2.10-dev.<branch>.<date>.<time>.h<sha>)
can land on any run of ".", "_" (from "+") and "-", so the whole run is trimmed.
*/}}
{{- define "agent-manager.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimAll "-._" }}
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
Existing Secret with the provider credentials of the deprecated keys:
oauth.existingSecret, else the platform's global.identity.existingSecret,
else the chart-rendered one.
*/}}
{{- define "agent-manager.oauthSecretName" -}}
{{- $g := include "agent-manager.globalIdentity" . | fromJson -}}
{{- .Values.oauth.existingSecret | default (dig "existingSecret" "" $g) | default (printf "%s-oauth" (include "agent-manager.fullname" .)) }}
{{- end }}

{{/*
Whether the chart renders its own OAuth Secret: no
authorizationServer.upstream.client.secretKeyRef, no existing Secret named.
*/}}
{{- define "agent-manager.oauthRendersSecret" -}}
{{- $g := include "agent-manager.globalIdentity" . | fromJson -}}
{{- $o := .Values.oauth -}}
{{- if and $o.enabled $o.authorizationServer.enabled (not $o.authorizationServer.upstream.client.secretKeyRef.name) (not $o.existingSecret) (not (dig "existingSecret" "" $g)) }}true{{ end }}
{{- end }}

{{/*
The provider people sign in with: authorizationServer.upstream.type, else the
deprecated oauth.provider, else global.identity.type, else dex.
*/}}
{{- define "agent-manager.oauthProvider" -}}
{{- $g := include "agent-manager.globalIdentity" . | fromJson -}}
{{- .Values.oauth.authorizationServer.upstream.type | default .Values.oauth.provider | default (dig "type" "" $g) | default "dex" }}
{{- end }}

{{/*
The audiences, as JSON, that muster requests at login and the forwarded ID
token then carries for the Kubernetes API server: muster.mcpServer.auth.requiredAudiences,
else global.identity.kubernetes.audiences.
*/}}
{{- define "agent-manager.requiredAudiences" -}}
{{- $g := include "agent-manager.globalIdentity" . | fromJson -}}
{{- .Values.muster.mcpServer.auth.requiredAudiences | default (dig "kubernetes" "audiences" list $g) | toJson }}
{{- end }}

{{/*
The OAuth settings the Deployment renders, as JSON. Each comes from the
shared layout (oauth.resourceServer, oauth.authorizationServer), else from
the deprecated key it replaces, else from global.identity:
  issuer          authorizationServer.issuer, baseURL, https://<fullname>.<global.domain>
  provider        authorizationServer.upstream.type, provider, global.identity.type, dex
  upstreamIssuer  authorizationServer.upstream.issuer, dex.issuerURL, global.identity.issuerUrl
  clientID        authorizationServer.upstream.client.id, dex.clientID or google.clientID,
                  global.identity.clientId (dex)
  clientSecret    authorizationServer.upstream.client.secretKeyRef, existingSecret,
                  global.identity.existingSecret, the chart-rendered Secret
  upstreamCA      authorizationServer.upstream.certificateAuthority.secretKeyRef,
                  dex.caSecret, global.identity.ca
  trustedIssuers  resourceServer.trustedIssuers, else one entry for the
                  provider people sign in with (openIDProvider), accepting its
                  ID tokens for the union, in this order and without
                  duplicates, of the deprecated trustedAudiences (else
                  clientID, else global.identity.clientId) and
                  agent-manager.requiredAudiences: every token muster
                  forwards carries those by construction and the
                  kube-apiserver trusts them, so a portal session's ID token
                  is accepted too
A CA is a mount: {volume, secret or configMap, key, dir}; caFile is dir/key.
*/}}
{{- define "agent-manager.oauth" -}}
{{- $o := .Values.oauth -}}
{{- $as := $o.authorizationServer -}}
{{- $up := $as.upstream -}}
{{- $rs := $o.resourceServer -}}
{{- $g := include "agent-manager.globalIdentity" . | fromJson -}}
{{- $domain := dig "domain" "" (.Values.global | default dict) -}}
{{- $derived := "" -}}
{{- if $domain }}{{ $derived = printf "https://%s.%s" (include "agent-manager.fullname" .) $domain }}{{ end -}}
{{- $r := dict "authorizationServer" $as.enabled "mounts" list -}}
{{- $_ := set $r "issuer" (required "oauth.authorizationServer.issuer is required when oauth.enabled (or set global.domain)" ($as.issuer | default $o.baseURL | default $derived)) -}}
{{- $provider := include "agent-manager.oauthProvider" . -}}
{{- $_ = set $r "provider" $provider -}}
{{- $upstreamIssuer := "" -}}
{{- $clientID := "" -}}
{{- if eq $provider "dex" -}}
{{- $upstreamIssuer = $up.issuer | default $o.dex.issuerURL | default (dig "issuerUrl" "" $g) -}}
{{- $clientID = $up.client.id | default $o.dex.clientID | default (dig "clientId" "" $g) -}}
{{- else -}}
{{- $upstreamIssuer = "https://accounts.google.com" -}}
{{- $clientID = $up.client.id | default $o.google.clientID -}}
{{- end -}}
{{- $_ = set $r "upstreamIssuer" $upstreamIssuer -}}
{{- $_ = set $r "clientID" $clientID -}}
{{- if $up.client.secretKeyRef.name -}}
{{- $_ = set $r "clientSecret" (dict "name" $up.client.secretKeyRef.name "key" (required "oauth.authorizationServer.upstream.client.secretKeyRef.key is required with its name" $up.client.secretKeyRef.key)) -}}
{{- else -}}
{{- $_ = set $r "clientSecret" (dict "name" (include "agent-manager.oauthSecretName" .) "key" (printf "%s-client-secret" $provider)) -}}
{{- if and (eq $provider "google") (not $clientID) -}}
{{- $_ = set $r "clientIDSecret" (dict "name" (include "agent-manager.oauthSecretName" .) "key" "google-client-id") -}}
{{- end -}}
{{- end -}}
{{- if eq $provider "dex" -}}
{{- $ca := dict -}}
{{- if $up.certificateAuthority.secretKeyRef.name -}}
{{- $ca = dict "secret" $up.certificateAuthority.secretKeyRef.name "key" ($up.certificateAuthority.secretKeyRef.key | default "ca.crt") -}}
{{- else if $o.dex.caSecret.name -}}
{{- $ca = dict "secret" $o.dex.caSecret.name "key" ($o.dex.caSecret.key | default "ca.crt") -}}
{{- else if dig "ca" "secretName" "" $g -}}
{{- $ca = dict "secret" (dig "ca" "secretName" "" $g) "key" (dig "ca" "key" "" $g | default $o.dex.caSecret.key | default "ca.crt") -}}
{{- end -}}
{{- if $ca.secret -}}
{{- $_ = set $ca "volume" "idp-ca" -}}
{{- $_ = set $ca "dir" "/etc/agent-manager/idp-ca" -}}
{{- $_ = set $r "upstreamCA" $ca -}}
{{- $_ = set $r "mounts" (append $r.mounts $ca) -}}
{{- end -}}
{{- end -}}
{{- $issuers := list -}}
{{- range $i, $entry := $rs.trustedIssuers -}}
{{- $out := omit $entry "certificateAuthority" -}}
{{- with $entry.certificateAuthority -}}
{{- $ca := dict "volume" (printf "trusted-issuer-ca-%d" $i) "dir" (printf "/etc/agent-manager/trusted-issuer-ca/%d" $i) -}}
{{- if .secretKeyRef -}}
{{- $_ := set $ca "secret" (required (printf "oauth.resourceServer.trustedIssuers[%d].certificateAuthority.secretKeyRef.name is required" $i) .secretKeyRef.name) -}}
{{- $_ = set $ca "key" (.secretKeyRef.key | default "ca.crt") -}}
{{- else -}}
{{- $_ := set $ca "configMap" (required (printf "oauth.resourceServer.trustedIssuers[%d].certificateAuthority needs secretKeyRef or configMapKeyRef" $i) (dig "configMapKeyRef" "name" "" .)) -}}
{{- $_ = set $ca "key" (dig "configMapKeyRef" "key" "" . | default "ca.crt") -}}
{{- end -}}
{{- $_ := set $out "caFile" (printf "%s/%s" $ca.dir $ca.key) -}}
{{- $_ = set $r "mounts" (append $r.mounts $ca) -}}
{{- end -}}
{{- $issuers = append $issuers $out -}}
{{- end -}}
{{- if not $issuers -}}
{{- $audiences := list -}}
{{- range concat ($o.trustedAudiences | default (list (dig "clientId" "" $g | default $clientID))) (include "agent-manager.requiredAudiences" . | fromJsonArray) -}}
{{- if and . (not (has . $audiences)) }}{{ $audiences = append $audiences . }}{{ end -}}
{{- end -}}
{{- if and $audiences $upstreamIssuer -}}
{{- $entry := dict "issuer" $upstreamIssuer "audiences" $audiences "openIDProvider" true -}}
{{- if eq $provider "dex" -}}
{{- /* Dex serves its JWKS at <issuer>/keys and signs ID tokens without a typ header. mcp-oauth does not imply the issuer host for private JWKS addresses, so it is listed. */ -}}
{{- $_ := set $entry "jwksURI" (printf "%s/keys" (trimSuffix "/" $upstreamIssuer)) -}}
{{- $_ = set $entry "acceptedTypHeaders" (list "" "JWT") -}}
{{- $_ = set $entry "allowPrivateIPJWKSHosts" (concat (list (urlParse $upstreamIssuer).hostname) $o.sso.allowPrivateIPJWKSHosts | uniq) -}}
{{- with $r.upstreamCA }}{{ $_ = set $entry "caFile" (printf "%s/%s" .dir .key) }}{{ end -}}
{{- else -}}
{{- $_ := set $entry "jwksURI" "https://www.googleapis.com/oauth2/v3/certs" -}}
{{- $_ = set $entry "acceptedTypHeaders" (list "JWT") -}}
{{- end -}}
{{- $issuers = list $entry -}}
{{- end -}}
{{- end -}}
{{- if not $as.enabled -}}
{{- if not $issuers -}}
{{- fail "oauth.authorizationServer.enabled false accepts only trusted issuer tokens: set oauth.resourceServer.trustedIssuers, or the upstream issuer and client ID it is derived from" -}}
{{- end -}}
{{- if not $rs.authorizationServers -}}
{{- fail "oauth.authorizationServer.enabled false needs oauth.resourceServer.authorizationServers: MCP clients discover the authorization server from the protected resource metadata (RFC 9728), and this server then runs none" -}}
{{- end -}}
{{- end -}}
{{- $_ = set $r "trustedIssuers" $issuers -}}
{{- toJson $r -}}
{{- end }}

{{/*
The namespace of an in-cluster Service host, for an egress rule: <svc> (the
release's namespace), <svc>.<namespace> or <svc>.<namespace>.svc[...], as the
pod's resolver tries them first (its search path); empty for any other host
(egress to every address). A host outside the cluster with one or two labels
is written as an absolute name, with a trailing dot (example.com.), which the
resolver never completes.
*/}}
{{- define "agent-manager.hostNamespace" -}}
{{- $labels := splitList "." .host -}}
{{- if eq (len $labels) 1 -}}
{{- .root.Release.Namespace -}}
{{- else if or (eq (len $labels) 2) (eq (index $labels 2) "svc") -}}
{{- index $labels 1 -}}
{{- end -}}
{{- end -}}

{{/*
The OTLP collector the pod exports to, as YAML {namespace, port}: the
namespace of an in-cluster Service host (<svc>.<ns>.svc[.cluster.local]),
empty for any other host; the endpoint's port, else the protocol's default
(4317 gRPC, 4318 HTTP). Empty when no endpoint is set.
*/}}
{{- define "agent-manager.otlpEgress" -}}
{{- with .Values.observability.otel.endpoint -}}
{{- $url := urlParse (ternary . (printf "grpc://%s" .) (contains "://" .)) -}}
{{- $hostPort := splitList ":" $url.host -}}
{{- $host := first $hostPort -}}
{{- $port := ternary (last $hostPort) "" (gt (len $hostPort) 1) -}}
{{- if not $port -}}
{{- $port = ternary "4318" "4317" (hasPrefix "http/" ($.Values.observability.otel.protocol | default "grpc")) -}}
{{- end -}}
{{- $namespace := include "agent-manager.hostNamespace" (dict "host" $host "root" $) -}}
namespace: {{ $namespace | quote }}
port: {{ $port }}
{{- end -}}
{{- end -}}

{{/*
The kagent controller target's egress, when the session tools are on:
namespace (agent-manager.hostNamespace) and port.
*/}}
{{- define "agent-manager.kagentEgress" -}}
{{- if and .Values.oauth.enabled .Values.kagent.controllerTarget -}}
{{- $url := urlParse .Values.kagent.controllerTarget -}}
{{- if not (has $url.scheme (list "grpc" "grpcs")) -}}
{{- fail (printf "kagent.controllerTarget %q must be grpc://host:port or grpcs://host[:port]" .Values.kagent.controllerTarget) -}}
{{- end -}}
{{- $hostPort := splitList ":" $url.host -}}
{{- $port := ternary (last $hostPort) "" (gt (len $hostPort) 1) -}}
{{- if not $port -}}
{{- if eq $url.scheme "grpc" -}}
{{- fail (printf "kagent.controllerTarget %q must name a port" .Values.kagent.controllerTarget) -}}
{{- end -}}
{{- $port = "443" -}}
{{- end -}}
{{- $namespace := include "agent-manager.hostNamespace" (dict "host" (first $hostPort) "root" $) -}}
namespace: {{ $namespace | quote }}
port: {{ $port }}
{{- end -}}
{{- end -}}

{{/*
muster as egress, its session bindings endpoint, else its MCP endpoint when
start_session asks the toolchain-manager there: the Service's namespace for
an in-cluster host (agent-manager.hostNamespace), the port the URL names or
its scheme's. Rendered with the session tools only (oauth and a controller
target), as the flags are.
*/}}
{{- define "agent-manager.musterBindingsEgress" -}}
{{- $toolchainList := and .Values.toolchains.repository .Values.muster.mcpUrl .Values.muster.toolchainsToolsPrefix -}}
{{- $musterURL := .Values.muster.bindingsUrl | default (ternary .Values.muster.mcpUrl "" (not (empty $toolchainList))) -}}
{{- if and .Values.oauth.enabled .Values.kagent.controllerTarget $musterURL -}}
{{- $url := urlParse $musterURL -}}
{{- if not (has $url.scheme (list "http" "https")) -}}
{{- if .Values.muster.bindingsUrl -}}
{{- fail (printf "muster.bindingsUrl %q must be http(s)://host[:port]/session-bindings" $musterURL) -}}
{{- end -}}
{{- fail (printf "muster.mcpUrl %q must be http(s)://host[:port]/mcp" $musterURL) -}}
{{- end -}}
{{- $hostPort := splitList ":" $url.host -}}
{{- $port := ternary (last $hostPort) (ternary "443" "80" (eq $url.scheme "https")) (gt (len $hostPort) 1) -}}
{{- $namespace := include "agent-manager.hostNamespace" (dict "host" (first $hostPort) "root" $) -}}
namespace: {{ $namespace | quote }}
port: {{ $port }}
{{- end -}}
{{- end -}}

{{/*
skill-manager's address for the egress policy: namespace
(agent-manager.hostNamespace) and port, from skillManager.url; empty when
unset.
*/}}
{{- define "agent-manager.skillManagerEgress" -}}
{{- with .Values.skillManager.url -}}
{{- $url := urlParse . -}}
{{- if not (has $url.scheme (list "http" "https")) -}}
{{- fail (printf "skillManager.url %q must be http(s)://host[:port]" .) -}}
{{- end -}}
{{- $hostPort := splitList ":" $url.host -}}
{{- $port := ternary (last $hostPort) (ternary "443" "80" (eq $url.scheme "https")) (gt (len $hostPort) 1) -}}
{{- $namespace := include "agent-manager.hostNamespace" (dict "host" (first $hostPort) "root" $) -}}
namespace: {{ $namespace | quote }}
port: {{ $port }}
{{- end -}}
{{- end -}}

{{/*
The installation's skills boot Secret: the minted one, else
skills.gitAuthSecretName. The minted one is the installation's own,
<fullname>-skills-token, never under the tenant-credential- prefix that marks a
tenant's own credentials: admission refuses a reference to it in an
Organization's namespace.
*/}}
{{- define "agent-manager.skillsGitAuthSecretName" -}}
{{- if .Values.skills.mintGitAuthSecret -}}
{{- if .Values.skills.gitAuthSecretName -}}
{{- fail "skills.mintGitAuthSecret and skills.gitAuthSecretName are mutually exclusive: the minted Secret is the installation's skills credential" -}}
{{- end -}}
{{- if not .Values.skills.github.app.secretName -}}
{{- fail "skills.mintGitAuthSecret needs skills.github.app.secretName: the Secret is filled with the skills GitHub App's installation token" -}}
{{- end -}}
{{- printf "%s-skills-token" (include "agent-manager.fullname" .) -}}
{{- else -}}
{{- .Values.skills.gitAuthSecretName -}}
{{- end -}}
{{- end }}

{{/*
The earlier names of the installation's skills boot Secret, a JSON list:
skills.previousGitAuthSecretNames and, when minting, the minted Secret's
earlier name under the tenant-credential- prefix. Agents naming one are moved
to the current name; while minting, each is rendered and kept filled as well.
*/}}
{{- define "agent-manager.skillsGitAuthPreviousSecretNames" -}}
{{- $names := list -}}
{{- if .Values.skills.mintGitAuthSecret -}}
{{- $names = append $names (printf "tenant-credential-%s-skills-token" (include "agent-manager.fullname" .)) -}}
{{- end -}}
{{- $current := include "agent-manager.skillsGitAuthSecretName" . -}}
{{- range .Values.skills.previousGitAuthSecretNames -}}
{{- if and (ne . $current) (not (has . $names)) -}}
{{- $names = append $names . -}}
{{- end -}}
{{- end -}}
{{- $names | toJson -}}
{{- end }}

{{- define "agent-manager.credentialOperationsNamespace" -}}
{{- if .Values.credentials.operationsNamespace -}}
{{- .Values.credentials.operationsNamespace -}}
{{- else -}}
{{- printf "%s-%s-credentials" (include "agent-manager.fullname" . | trunc 30 | trimSuffix "-") (printf "%s/%s" .Release.Namespace .Release.Name | sha256sum | trunc 8) -}}
{{- end -}}
{{- end -}}

{{/*
Non-empty when github.tokenExchange is configured; fails on github.enabled
without it (commit mode fetches the person's grant from muster's broker), on
an incomplete configuration, one without github.enabled, without Dex OAuth and
downstream OAuth (the person's Dex ID token is the subject token), or without
muster.mcpServer.auth.forwardToken (providerApps needs the person's
connection).
*/}}
{{- define "agent-manager.githubTokenExchange" -}}
{{- if and .Values.github.enabled (not (dig "tokenExchange" "tokenEndpoint" "" .Values.github)) }}
{{- fail "github.enabled needs github.tokenExchange: commit mode fetches the person's grant of the GitHub App from muster's broker; set github.tokenExchange.tokenEndpoint, audience and existingSecret" }}
{{- end }}
{{- with .Values.github.tokenExchange }}
{{- if .tokenEndpoint }}
{{- if not $.Values.github.enabled }}
{{- fail "github.tokenExchange needs github.enabled: set github.enabled to true, or unset github.tokenExchange.tokenEndpoint" }}
{{- end }}
{{- if not (and $.Values.oauth.enabled (eq (include "agent-manager.oauthProvider" $) "dex") $.Values.oauth.downstream.enabled) }}
{{- fail "github.tokenExchange exchanges the person's Dex ID token: set oauth.enabled with the dex provider (oauth.authorizationServer.upstream.type) and oauth.downstream.enabled" }}
{{- end }}
{{- if not $.Values.muster.mcpServer.auth.forwardToken }}
{{- fail "github.tokenExchange lists its App under the MCPServer's providerApps, which muster serves only on a forwarded token: set muster.mcpServer.auth.forwardToken" }}
{{- end }}
{{- $_ := required "github.tokenExchange.audience is required with github.tokenExchange.tokenEndpoint" .audience }}
{{- if not (or .existingSecret .clientSecret) }}
{{- fail "github.tokenExchange.existingSecret or github.tokenExchange.clientSecret is required with github.tokenExchange.tokenEndpoint" }}
{{- end }}
{{- "true" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The Secret holding the GitHub token exchange client: existingSecret, else the
one the chart renders from clientSecret.
*/}}
{{- define "agent-manager.githubTokenExchangeSecretName" -}}
{{- .Values.github.tokenExchange.existingSecret | default (printf "%s-github-token-exchange" (include "agent-manager.fullname" .)) }}
{{- end }}
