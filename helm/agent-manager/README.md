# agent-manager

CIMD is enabled by default. DCR is deprecated by MCP 2026-07-28 and remains a
compatibility fallback; see the [OAuth registration guide](../../docs/oauth-client-registration.md).

Agent lifecycle service for the Agent Platform — creates, updates, deletes and inspects kagent agents as Flux HelmReleases of the agent chart, validated against the chart schema, exposed as REST and MCP

The chart deploys one Deployment that serves the REST/JSON API under `/api/v1`
(contract: `/api/v1/openapi.yaml`) and the MCP streamable-HTTP endpoint under
`/mcp`. An agent is a Flux `HelmRelease` of the Generic `agent` chart (2.x: one
release renders one `api.kagent.dev/v1alpha3` `Agent`, its template inline and
its Harness by name, plus the agent's own muster `RemoteMCPServer`) plus the
shared per-namespace `OCIRepository` of that chart tracking `2.x`;
agent-manager composes both the way the portal's create flow does, pins every
skill to a commit or a digest, validates the values against the chart's
`values.schema.json` and has the API server dry-run the objects the chart
renders from them before applying, and reads agents back from the Agent,
the RemoteMCPServer and the HelmRelease. Readiness is the Agent's own
`status.conditions`.

- `kagent.namespace` (plus `kagent.additionalNamespaces`) are the namespaces
  agents live in; the Role is created there. Requests naming another
  namespace are refused.
- `harness.name` is the platform Harness an agent runs on unless
  `create_agent` names another (rendered by the connectivity chart of the
  `agent-platform` meta chart); it is the Agent's `spec.harnessRef.name`.
- `agentChart.ociUrl` / `agentChart.semver` are what the composed
  OCIRepository carries (`2.x`, never a pre-release build); the same registry
  is read at run time for the latest version, its values schema and its
  templates (`GET /api/v1/info` says which chart is in use — the embedded
  copy is the offline fallback).
- `muster.url` is the platform's muster MCP URL composed into every agent as
  the chart value `muster.url`; empty leaves the Generic chart's default.
- `skillManager.url` names skill-manager: skills and plugins are pinned
  through it and `list_skills` proxies its catalogue, as the caller.
- Without it (deprecated): `skills.repositories` are the GitHub repositories
  `list_skills` discovers `SKILL.md` files in, reporting the head commit an
  agent pins; `skills.github.tokenSecret` names a Secret with the token that
  resolves branches and tags in private repositories (there is no per-agent
  skill credential).
- With `oauth.enabled` the service is an OAuth 2.1 resource server: every
  request carries the caller's identity, and with `oauth.downstream.enabled`
  every Kubernetes call is made as the caller (the ServiceAccount holds no
  RBAC). The bearer tokens it accepts are the IdP id_tokens muster forwards,
  whose audience must be trusted: `oauth.trustedAudiences` (default: the
  platform client, `global.identity.clientId`) plus, always,
  `muster.mcpServer.auth.requiredAudiences` — every forwarded token carries
  those by construction and they are what the kube-apiserver trusts, so a
  portal session, whose id_token names the portal's own client, is accepted
  without a per-installation list. A token for none of them is refused with
  `401`, naming its `aud` next to the trusted audiences.
- Without OAuth writes run under the chart's ServiceAccount and the service
  checks no identity itself; the agentgateway JWT policy in front of the
  route (rendered by the connectivity chart of the `agent-platform` meta chart)
  and muster in front of the MCP endpoint are then the trust boundary.

A component of the [`giantswarm/agent-platform`](https://github.com/giantswarm/agent-platform)
meta chart (`components.agent-manager.enabled`), which sets `kagent.namespace`,
`harness.name`, `agentChart.*`, `muster.url`, `mcp.enabled`, `oauth.*`,
`muster.mcpServer.*` and `skills.repositories`, and derives
`flux.helmReleaseServiceAccount` from its `kagent.fluxServiceAccountName`.

**Homepage:** <https://github.com/giantswarm/agent-manager>

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| Giant Swarm | <team-bumblebee@giantswarm.io> |  |

## Source Code

* <https://github.com/giantswarm/agent-manager>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| replicaCount | int | `1` | Number of replicas. The service is stateless (every read goes to the API server); more than one is fine. |
| revisionHistoryLimit | int | `3` | Number of old ReplicaSets retained for rollback. Keep tight so frequent rollouts don't leak zero-replica ReplicaSets across the cluster (each one still carries the pod template of an older chart version, and a Kyverno background scan reports every stale ReplicaSet that fails a newer policy). |
| image.registry | string | `"gsoci.azurecr.io"` | Image registry. |
| image.repository | string | `"giantswarm/agent-manager"` | Image repository. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.tag | string | `""` | Image tag. Defaults to the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets. |
| nameOverride | string | `""` | Override the chart name. |
| fullnameOverride | string | `""` | Override the fully qualified release name (the umbrella chart pins the Service name through this). |
| global | object | `{}` | Platform-wide values a parent chart (the `agent-platform` meta chart) shares with every component. `global.domain` derives the OAuth issuer. `global.identity` is the platform's sign-in, the fallback for `oauth.authorizationServer.upstream` and the derived trusted issuer: `issuerUrl`, `clientId`, `existingSecret` (`dex-client-secret`), `ca` (`secretName`, `key`), `type` (`dex` or `google`) and `kubernetes.audiences`, the audiences the Kubernetes API server accepts (its `AuthenticationConfiguration` `jwt.issuer.audiences`: a token passes when its `aud` holds at least one). Empty here; a chart installed on its own sets `oauth.*` directly. |
| kagent.namespace | string | `"kagent"` | Namespace agents are created in and listed from by default — the namespace the kagent controller watches, the platform admin provisions ModelConfigs into and the connectivity chart renders the platform Harness into. The Role for HelmReleases, OCIRepositories, Agents, RemoteMCPServers, Harnesses, ModelConfigs and events is created here. |
| kagent.additionalNamespaces | list | `[]` | Additional namespaces agents may live in (multi-tenant installs). Each gets the same Role; requests naming any other namespace are refused. |
| kagent.apiVersion | string | `"auto"` | api.kagent.dev API version for Agents, Harnesses, RemoteMCPServers and ModelConfigs; `auto` discovers the version serving `agents` (default v1alpha3). |
| kagent.controllerTarget | string | `"grpc://agentgateway.agent-platform.svc.cluster.local:8080"` | The kagent controller's gRPC API through agentgateway, which `list_sessions`, `get_session`, `start_session` and `delete_session` call as the caller (SessionService and the A2A service, the caller's IdP token as the bearer): `grpc://host:port` (h2c, the in-cluster agentgateway Service) or `grpcs://host[:port]` (TLS). Rendered only with `oauth.enabled`, since the calls never run as the ServiceAccount; empty turns the session tools off (`get_info` capabilities.sessions). With `networkPolicy.enabled` the policy opens egress to it: the Service's namespace for an in-cluster host, every address otherwise. |
| defaultAgentModel.agent | string | `""` | An Agent of `kagent.namespace` whose empty modelConfig is filled with the namespace's default model (the ModelConfig annotated `agent-platform.giantswarm.io/is-default-model-config: "true"`); once set, by anyone, it is never changed. None marked: nothing; several: a warning, nothing. Checked at start and every 30s as the ServiceAccount, with a Role that may get and patch that Agent and list the ModelConfigs. Empty: off. |
| toolchains.repository | string | `""` | The OCI repository the installation's toolchains are published under: toolchain `<name>` is the image `<repository>/<name>`, its versions its tags (`latest` the newest), read anonymously unless `registryConfig` has credentials for its host. A version the toolchain-manager's `list_toolchains` lists (`muster.toolchainsToolsPrefix`) is read from the image of the source that offers it, another registry too. `start_session` mounts the toolchains it names, else the agent's `defaultToolchains`, into the session at their digests, refuses one whose `io.giantswarm.toolchain.harnesses` label does not list the agent's Harness runtime, and gives the session the origins its `io.giantswarm.toolchain.egress` label declares. The kagent controller must admit the repository for session tool images (`KAGENT_SESSION_TOOL_IMAGE_REPOSITORIES`): set both together, on a kagent line that mounts them (`gsoci.azurecr.io/giantswarm/agent-toolchains`). Rendered with `kagent.controllerTarget`. Empty (the default) refuses toolchains and `get_info` reports the capability `toolchains` false. |
| toolchains.registryConfig.secretName | string | `""` | A `kubernetes.io/dockerconfigjson` Secret in the release namespace: agent-manager reads the toolchains of a registry host its `.dockerconfigjson` has credentials for with them, a private source's (`--toolchain-registry-config`). The kagent controller pulls the images with its own pull secret. Optional: until the Secret exists, and when empty (the default), every registry is read anonymously. Read when the pod starts. Rendered with `repository`. |
| harness.name | string | `"kagent"` | Name of the platform Harness an agent runs on unless `create_agent` names another (the connectivity chart renders it): composed as the chart value `agent.harness`, the Agent's `spec.harnessRef.name`. |
| harness.egress | object | GitHub for `claude`, `codex` and `omp` | The origins a coding agent (on a Harness whose runtime is not kagent: claude, codex, byo) reaches, by Harness name: the installation's forge and git hosts. agent-manager writes the list into the Agent's `spec.egress` and refuses any other origin; an agent on a coding Harness without an entry is refused, an empty list reaches none. Origins are `http(s)://host[:port]`, a host may start with `*.`. A toolchain's registries are not listed here: a session reaches the origins its toolchains declare (`toolchains.repository`). |
| agentChart.ociUrl | string | `"oci://gsoci.azurecr.io/charts/giantswarm/agent"` | OCI URL of the Generic `agent` chart every agent renders from (2.x: one release renders one api.kagent.dev/v1alpha3 Agent plus the agent's muster RemoteMCPServer). Written into the shared per-namespace OCIRepository, and read at run time for the chart's values.schema.json and templates: every create/update is validated against the schema and its rendered objects dry-run before it is applied (the pod needs egress to this registry; the embedded copy of the chart is the offline fallback). |
| agentChart.semver | string | `"2.x"` | Semver range the OCIRepository tracks. `2.x` follows every 2.x release of the Generic chart and never a pre-release build or a next major. |
| agentChart.semverFilter | string | `""` | Regular expression the chart's tags must match before `semver` is evaluated (the OCIRepository's ref.semverFilter, and the version the values schema is read from). Set it beside a range that admits pre-releases (`>=1.5.0-0 <2.0.0-0`): the chart's branch builds are pushed to the same repository, and `^v?[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$` keeps them out while admitting release candidates. Empty filters nothing. |
| agentChart.refresh | string | `"10m"` | How often the registry is re-read for the latest version and schema. |
| flux.helmReleaseInterval | string | `"10m"` | HelmRelease.spec.interval of every composed agent. |
| flux.ociRepositoryInterval | string | `"30m"` | OCIRepository.spec.interval of the shared chart source. |
| flux.helmReleaseServiceAccount | string | `""` | HelmRelease.spec.serviceAccountName, required by a Flux multi-tenancy admission policy in tenant namespaces; empty omits it. |
| flux.helmReleaseApiVersion | string | `"auto"` | helm.toolkit.fluxcd.io API version composed into HelmReleases; `auto` discovers it. |
| flux.ociRepositoryApiVersion | string | `"auto"` | source.toolkit.fluxcd.io API version composed into OCIRepositories; `auto` discovers it. |
| skillManager.url | string | `""` | skill-manager's base URL (`http://skill-manager.agent-platform.svc:8080`): skills and plugins are pinned through it, `list_skills` proxies its catalogue (deprecated), as the caller; `skills.repositories` is unused. Empty: in-process discovery and pinning (deprecated). An outside host with one or two labels is written with a trailing dot (`example.com.`): otherwise the egress rule reads it as `<service>.<namespace>`. |
| skills.repositories | list | `["https://github.com/giantswarm/agent-skills"]` | GitHub repositories whose SKILL.md files list_skills offers (the same list the portal's create flow reads), each with the head commit an agent pins. Empty turns the skills capability off; skills given by URL are still resolved through the GitHub API. Unused when `skillManager.url` is set. Deprecated: skill-manager's sources replace it. |
| skills.github.apiUrl | string | `"https://api.github.com"` | GitHub API base URL. |
| skills.github.app.secretName | string | `""` | Existing Secret in the release namespace of the read-only skills GitHub App, with the keys `app-id`, `installation-id` and `private-key` (PEM): skills are discovered and pinned with the App's short-lived installation tokens, at the App's rate limit, and a private repository is offered to and usable by only the people who can read it themselves (their GitHub login, verified through the `github` pin below; without the pin private repositories are refused). Mutually exclusive with `tokenSecret`. |
| skills.github.tokenSecret.name | string | `""` | Existing Secret in the release namespace with a static GitHub token for skill discovery and pinning. Prefer `app`. Empty (and no `app`): anonymous, public repositories only. |
| skills.github.tokenSecret.key | string | `"token"` | Key of the token in that Secret. |
| skills.gitAuthSecretName | string | `""` | Secret (key `token`) in the agents' namespace every agent with a git skill fetches its skills with at boot, unless the agent names its own `gitAuthSecretName`: composed as the agent chart's `skillsGitAuthSecretRef.name`. Empty: such agents fetch anonymously. |
| skills.mintGitAuthSecret | bool | `false` | Boot private git skills with the skills GitHub App instead of a provisioned token (needs `github.app.secretName`; replaces `gitAuthSecretName`): the chart renders the Secret `<fullname>-skills-token` without data in `kagent.namespace` and every `kagent.additionalNamespaces` entry, plus a Role that may only get and update it, and composes it as every git-skill agent's `skillsGitAuthSecretRef.name`. agent-manager writes an installation token into it (key `token`, the Basic form of `x-access-token:<token>`) and replaces it well before its one-hour expiry; `get_info` reports the last refresh and a failed one as `skillsGitAuthMint`. The name is the installation's own, never under the `tenant-credential-` prefix of a tenant's credentials. The Secret's earlier name `tenant-credential-<fullname>-skills-token` is a previous name (see `previousGitAuthSecretNames`): still rendered and filled, and the agents naming it are moved to the current name. |
| skills.previousGitAuthSecretNames | list | `[]` | Earlier names of the installation's skills credential Secret, after a rename: an agent naming one is composed with the current name on its next update, and the connectivity chart's `agent-manager migrate` moves the rest. With `mintGitAuthSecret` each is also rendered and kept filled until it is dropped from this list; without, the old Secret stays the operator's until no agent names it. |
| skills.cacheTTL | string | `"5m"` | How long a repository's discovered skills are reused. |
| mcp.enabled | bool | `true` | Serve the MCP streamable-HTTP endpoint alongside the REST API. |
| mcp.path | string | `"/mcp"` | MCP endpoint path. |
| oauth.enabled | bool | `false` | Make agent-manager an OAuth 2.1 resource server (mcp-oauth): the MCP endpoint and the REST API require a bearer token, and every call carries the caller's identity (logged on every write, returned as `requestedBy`). On the Agent Platform muster forwards the session's IdP ID token to this server (MCPServer `auth.forwardToken`, rendered below) and the portal sends the signed-in user's ID token through the gateway; both are accepted from the provider people sign in with (`resourceServer.trustedIssuers`). Off: anonymous, acting as the ServiceAccount, only for a server nothing but a trusted proxy can reach. |
| oauth.resourceServer.resource | string | `""` | The main protected resource, by its full URL (`OAUTH_RESOURCE`). Empty uses the issuer. |
| oauth.resourceServer.additionalResources | list | `[]` | Further protected resources on the same origin, each by its full URL (`OAUTH_ADDITIONAL_RESOURCES`). The REST API and the MCP endpoint are always protected resources. |
| oauth.resourceServer.authorizationServers | list | `[]` | Issuer identifiers the resource metadata advertises (`OAUTH_AUTHORIZATION_SERVERS`, RFC 9728 `authorization_servers`): this server's issuer or a trusted issuer. Empty advertises this server's issuer; required with `authorizationServer.enabled` false, since MCP clients find the authorization server here. |
| oauth.resourceServer.trustedIssuers | list | `[]` | The issuers whose JWTs are accepted (`OAUTH_TRUSTED_ISSUERS`). A trust anchor: set it only from the release's own values or a GitOps-managed ConfigMap that only installation administrators write. Each entry: `issuer`, `jwksURI` and `audiences` (required), `subjectClaim` (`sub` or `email`), `acceptedTypHeaders`, `allowedClaims`, `allowPrivateIPJWKSHosts`, `openIDProvider` (true for the provider people sign in with, whose bearers are their ID tokens), and `certificateAuthority` with a `secretKeyRef` or `configMapKeyRef` (`name`, `key`). Empty trusts the provider people sign in with (`authorizationServer.upstream`) as `openIDProvider`: its ID tokens are accepted as bearer tokens, a deviation from MCP's audience rule until per-backend tokens (ID-JAG, giantswarm/giantswarm#39102). They are accepted for the client people sign in with (`authorizationServer.upstream.client.id`, or the deprecated `oauth.trustedAudiences`) and `muster.mcpServer.auth.requiredAudiences`, in that order and without duplicates. |
| oauth.resourceServer.requireResourceAccessTokens | bool | `false` | Accept only tokens whose audience names this server's resource (`OAUTH_REQUIRE_RESOURCE_ACCESS_TOKENS`), as MCP requires. Off while the platform forwards ID tokens (giantswarm/giantswarm#39102). |
| oauth.authorizationServer.enabled | bool | `true` | Run the authorization server. Off makes agent-manager a resource server only (`OAUTH_ENABLE_AUTHORIZATION_SERVER=false`). |
| oauth.authorizationServer.issuer | string | `""` | Public base URL of this server, the issuer of its tokens and metadata (`OAUTH_ISSUER`; https, or http on loopback). Empty uses the deprecated `oauth.baseURL`, else `https://<fullname>.<global.domain>`. |
| oauth.authorizationServer.upstream.type | string | `""` | The OpenID Provider people sign in with: `dex` or `google` (`OAUTH_PROVIDER`). Empty uses the deprecated `oauth.provider`, else `dex`. |
| oauth.authorizationServer.upstream.issuer | string | `""` | Dex issuer URL. Empty uses the deprecated `oauth.dex.issuerURL`, else `global.identity.issuerUrl`. |
| oauth.authorizationServer.upstream.client.id | string | `""` | OAuth client ID at the provider. Empty uses the deprecated `oauth.dex.clientID` or `oauth.google.clientID`, else `global.identity.clientId` (dex). |
| oauth.authorizationServer.upstream.client.secretKeyRef | object | `{"key":"client-secret","name":""}` | Secret key with the client secret, mounted as a file. Empty name uses the deprecated `oauth.existingSecret`, else `global.identity.existingSecret`, else a Secret from the deprecated inline client secret. |
| oauth.authorizationServer.upstream.certificateAuthority.secretKeyRef | object | `{"key":"ca.crt","name":""}` | Secret key with the CA of a Dex that serves a private certificate (`OAUTH_DEX_CA_FILE`). Empty name uses the deprecated `oauth.dex.caSecret`, else `global.identity.ca`. |
| oauth.authorizationServer.dynamicClientRegistration.enabled | bool | `true` | Serve RFC 7591 dynamic client registration (`OAUTH_ENABLE_DYNAMIC_CLIENT_REGISTRATION`). MCP 2026-07-28 deprecates it in favour of client ID metadata documents; it stays on for clients that still register this way. |
| oauth.authorizationServer.dynamicClientRegistration.initialAccessToken.secretKeyRef | object | `{"key":"token","name":""}` | Secret key with the RFC 7591 initial access token a registration must present (mcp-oauth reads it from `OAUTH_INITIAL_ACCESS_TOKEN_FILE`). |
| oauth.authorizationServer.clientIDMetadataDocuments.enabled | bool | `true` | Accept URL client IDs (client ID metadata documents) (`OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS`). |
| oauth.authorizationServer.storage.type | string | `"memory"` | Token storage (`OAUTH_STORAGE_BACKEND`). Memory loses every session on restart and needs one replica. |
| oauth.baseURL | string | `""` | Deprecated: use `authorizationServer.issuer`. |
| oauth.provider | string | `""` | Deprecated: use `authorizationServer.upstream.type`. |
| oauth.dex.issuerURL | string | `""` | Deprecated: use `authorizationServer.upstream.issuer`. |
| oauth.dex.clientID | string | `""` | Deprecated: use `authorizationServer.upstream.client.id`. |
| oauth.dex.clientSecret | string | `""` | Deprecated: use `authorizationServer.upstream.client.secretKeyRef`. |
| oauth.dex.allowPrivateURLs | bool | `false` | Deprecated, no effect, not passed to the server: the Dex client reaches private addresses and follows HTTP_PROXY, HTTPS_PROXY and NO_PROXY without it. Remove it from your values; the key goes away in a later release. |
| oauth.dex.caSecret | object | `{"key":"ca.crt","name":""}` | Deprecated: use `authorizationServer.upstream.certificateAuthority`. |
| oauth.google.clientID | string | `""` | Deprecated: use `authorizationServer.upstream.client.id`. Empty reads the Secret key `google-client-id`. |
| oauth.google.clientSecret | string | `""` | Deprecated: use `authorizationServer.upstream.client.secretKeyRef`. |
| oauth.existingSecret | string | `""` | Deprecated: use `authorizationServer.upstream.client.secretKeyRef`. Existing Secret with `dex-client-secret` (dex) or `google-client-secret` (+ optional `google-client-id`) (google). |
| oauth.trustedAudiences | list | `[]` | Deprecated: list the provider in `resourceServer.trustedIssuers` with these `audiences`. The OAuth client IDs whose ID tokens of the provider people sign in with are accepted. |
| oauth.sso.allowPrivateIPs | bool | `false` | Deprecated, no effect, not passed to the server: the issuer host of the configured provider may resolve to a private address for its JWKS without it; list any other JWKS host in allowPrivateIPJWKSHosts. Remove it from your values; the key goes away in a later release. |
| oauth.sso.allowPrivateIPJWKSHosts | list | `[]` | Deprecated: list the provider in `resourceServer.trustedIssuers` with these `allowPrivateIPJWKSHosts`. Hosts, other than the issuer's own, whose JWKS may resolve to an RFC 1918 or RFC 4193 address. |
| oauth.allowPublicClientRegistration | bool | `false` | Deprecated, removed in a later release: accept dynamic client registration without an initial access token, public clients included (labs only). MCP clients register with client ID metadata documents. |
| oauth.requestStateSecret | object | `{"key":"key","name":"","previousKey":""}` | Existing Secret with the 32-byte key (base64 or hex) that seals the requestState of a URL interaction relayed to the tool's caller as URL mode elicitation, mounted as `OAUTH_REQUEST_STATE_KEY_FILE`; `previousKey` names the Secret's key holding the key before a rotation, mounted as `OAUTH_REQUEST_STATE_PREVIOUS_KEY_FILE`. Each server has its own key. Empty name: no key, and a URL interaction is answered as a tool error naming its URL. |
| oauth.downstream.enabled | bool | `false` | Call the Kubernetes API as the caller: everything a request does (HelmReleases and OCIRepositories, the Agents, RemoteMCPServers, Harnesses, ModelConfigs and events it reads) presents the caller's IdP token, so the caller's RBAC governs — the apiserver must trust the IdP and the token's audience (a Dex install lists that audience in `muster.mcpServer.auth.requiredAudiences`; a Google install's client id is the apiserver's `--oidc-client-id`). The ServiceAccount then holds no permissions: the chart renders none of its Roles (`rbac.create` is moot) and the token stays mounted only for the in-cluster API address and CA plus API discovery at startup, which every authenticated principal may read. Requests have no other identity: a request without a token is refused with 401 instead of running as the ServiceAccount. Background work keeps its own RBAC (`skills.mintGitAuthSecret`), and `tenants.enabled` lets the ServiceAccount read namespaces to find the organization namespaces. Presenting the forwarded token is token passthrough, which MCP forbids: a deviation until per-backend tokens (ID-JAG, giantswarm/giantswarm#39102). |
| github.enabled | bool | `false` | Offer commit mode (`mode: commit` on `create_agent`, `update_agent` and `delete_agent`: the manifests land as a pull request opened as the person in the repository that owns the namespace), with the person's grant of the GitHub App, which the server fetches per commit from muster's broker (`tokenExchange`, RFC 8693) and verifies with `GET /user`. Needs `tokenExchange.tokenEndpoint`, and the App installed with contents and pull-request write on every repository a commit may target. |
| github.apiURL | string | `"https://api.github.com"` | GitHub's REST API, for `GET /user` and the pull request. |
| github.tokenExchange.tokenEndpoint | string | `""` | RFC 8693 token endpoint the server fetches the person's grant of the App from, for each call that needs it. Needs `enabled`, Dex OAuth and `oauth.downstream.enabled`. |
| github.tokenExchange.audience | string | `""` | RFC 8693 audience of the grant, also the App a commit without the person's grant names in its `grant_required` answer and the MCPServer lists under `auth.providerApps` (muster's `aggregator.providerApps` key): lowercase letters, digits and hyphens, at most 63 characters. Required with `tokenEndpoint`. |
| github.tokenExchange.clientID | string | `""` | Confidential client ID. Empty reads the Secret's `client-id` key. |
| github.tokenExchange.clientSecret | string | `""` | Confidential client secret, rendered into a Secret `<fullname>-github-token-exchange`. Prefer `existingSecret`. |
| github.tokenExchange.existingSecret | string | `""` | Existing Secret in the release namespace with `client-secret` (and `client-id` when `clientID` is empty). Required with `tokenEndpoint` unless `clientSecret` is set. |
| github.tokenExchange.caSecretName | string | `""` | Optional existing Secret with `ca.crt` for the token endpoint's private CA. |
| muster.url | string | `""` | The platform's muster MCP URL, composed into every agent as the chart value `muster.url` (the URL of the agent's own RemoteMCPServer). Empty composes nothing and the Generic chart's default applies (`http://muster.agent-platform.svc.cluster.local:8090/mcp`). The `agent-platform` meta chart forwards it from its connectivity helper. |
| muster.targetUrl | string | `""` | The muster MCP URL composed into an agent placed on a workload cluster (create_agent with organization and cluster): one those clusters reach, the installation's public muster endpoint. Empty refuses target clusters. |
| muster.bindingsUrl | string | `"http://muster.agent-platform.svc.cluster.local:8090/session-bindings"` | muster's session bindings endpoint: `start_session` binds the sandbox actor of every Session with providers to the caller there, as the caller, before its first turn, and `delete_session` unbinds it, so muster releases the person's provider token to that sandbox alone. Rendered with the session tools (`oauth.enabled` and `kagent.controllerTarget`); empty refuses a workspace grant. With `networkPolicy.enabled` the policy opens egress to it: the Service's namespace for an in-cluster host, every address otherwise. |
| muster.actorBearer | bool | `false` | muster trusts the actor JWT of a bound session (muster's `credentialProvider.actorBearer`): every session `start_session` binds carries the agent's toolset, and `get_agent`, `list_agents` and `get_info` report a toolset as bound to each session at its start. Rendered with `bindingsUrl`. The agent-platform chart sets it with workspaces on. |
| muster.mcpUrl | string | `"http://muster.agent-platform.svc.cluster.local:8090/mcp"` | muster's MCP endpoint, the muster Service of `bindingsUrl`: `start_session` calls there, as the caller, the workspace-manager's `authorize_workspace` for an agent's default workspace when the start names no workspace (rendered with `bindingsUrl` and `workspacesToolsPrefix`), and the toolchain-manager's `list_toolchains` (rendered with `toolchains.repository` and `toolchainsToolsPrefix`). |
| muster.workspacesToolsPrefix | string | `"x_workspace-manager_"` | The workspace-manager's tool prefix in muster. Empty: a start that names no workspace on an agent with a default workspace is refused. |
| muster.toolchainsToolsPrefix | string | `""` | The toolchain-manager's tool prefix in muster (`x_toolchain-manager_`): `start_session` starts a toolchain only at a version its `list_toolchains` lists for the caller, the newest listed when none is named, so a deprecated or unpublished version starts nothing. Set it where the toolchain-manager runs behind muster. Empty: any version `toolchains.repository` carries starts. |
| muster.mcpServer.enabled | bool | `false` | Register this server with muster by rendering an `mcpservers.muster.giantswarm.io` CR in the release namespace. Tools then appear as `x_<name>_<tool>`. |
| muster.mcpServer.name | string | `"agent-manager"` | MCPServer CR name (drives the tool prefix). |
| muster.mcpServer.displayName | string | `"Agent Manager"` | Human-friendly name the Dev Portal shows for this server, rendered as the `ui.giantswarm.io/display-name` annotation. Empty renders none. |
| muster.mcpServer.autoStart | bool | `true` | Start the server connection when muster initializes. |
| muster.mcpServer.description | string | `"Agent lifecycle (create, update, delete, status, skills, model configs) and conversations (sessions) for the Agent Platform"` | Human-readable description shown by muster. |
| muster.mcpServer.labels | object | `{}` | Extra labels on the MCPServer CR. |
| muster.mcpServer.auth | object | `{"forwardToken":true,"requiredAudiences":[]}` | How muster authenticates to this server; rendered only with `oauth.enabled`. `forwardToken` makes muster forward the session's IdP ID token byte-identical, accepted through the OpenID Provider's trusted issuer entry. `requiredAudiences` are extra audiences that token must carry — the Dex cross-client audience the kube-apiserver trusts (`dex-k8s-authenticator` on Giant Swarm clusters; agentlab's is `kubernetes`) so `oauth.downstream` works; muster requests them at login, so users re-login after a change. The derived trusted issuer entry accepts them, since its audiences are the platform client plus this list. Empty uses `global.identity.kubernetes.audiences`. Google adds no audiences, so muster requests none there: the API server accepts the login client id, which the trusted issuer entry already holds. |
| httpRoute.enabled | bool | `false` | Expose the service through a Gateway API HTTPRoute. |
| httpRoute.parentRefs | list | `[]` | parentRefs of the HTTPRoute (required when enabled). |
| httpRoute.hostnames | list | `[]` | Hostnames matched by the route. |
| httpRoute.annotations | object | `{}` | Annotations on the HTTPRoute. |
| httpRoute.labels | object | `{}` | Labels on the HTTPRoute. |
| networkPolicy.enabled | bool | `false` | Create a Kubernetes NetworkPolicy for the pod (off when an umbrella chart renders the policies). |
| networkPolicy.ingressNamespaces | list | `[]` | Namespaces allowed to reach the API (label kubernetes.io/metadata.name). Empty allows ingress from the release namespace only. |
| networkPolicy.allowKubeAPI | bool | `true` | Allow egress to the Kubernetes API server. |
| networkPolicy.allowInternet | bool | `true` | Allow egress on 443 to every destination: the chart registry (schema and version), GitHub (skill discovery and commit resolution) and the registries of OCI skills (digest resolution). Vanilla NetworkPolicy cannot select by name; narrow with egressCIDRs instead when the addresses are known. |
| networkPolicy.egressCIDRs | list | `[]` | Extra egress CIDRs. |
| networkPolicy.metricsNamespaces | list | `["kube-system"]` | Namespaces allowed to scrape the metrics port (label kubernetes.io/metadata.name), when `observability.metrics.enabled`. |
| serviceAccount.create | bool | `true` | Create a ServiceAccount. |
| serviceAccount.annotations | object | `{}` | Annotations on the ServiceAccount. |
| serviceAccount.name | string | `""` | ServiceAccount name (generated when empty). |
| rbac.create | bool | `true` | Create the Role/RoleBinding in `kagent.namespace` (and every `kagent.additionalNamespaces` entry): HelmReleases and OCIRepositories read/write; Agents, RemoteMCPServers, Harnesses, ModelConfigs and events read (Agents and RemoteMCPServers also delete, for the forced removal of what a suspended or bare release leaves behind) — the ServiceAccount's own permissions. Ignored with `oauth.downstream.enabled`: the ServiceAccount then gets no RBAC at all, the caller's RBAC governs. |
| tenants.enabled | bool | `false` | Manage the organization namespaces beside `kagent.namespace` and `kagent.additionalNamespaces`, and serve the organization tools (`create_organization`, `add_organization_member`, `set_organization_model`, `list_organizations` and the rest). An organization namespace is labelled `agent-platform.giantswarm.io/organization` (agent-platform-operator keeps it and what it holds); a Giant Swarm Organization's `org-<name>`, labelled `giantswarm.io/organization`, is not one. Everything a request does there runs as the caller. The ServiceAccount only finds the namespaces by label and lists the organizations: the chart grants it `get` and `list` on namespaces and Organizations (`security.giantswarm.io`), read-only. Needs `oauth.downstream.enabled` and `flux.helmReleaseServiceAccount`, and the ClusterRole `agent-platform-tenant-flux` the agents' HelmReleases run as there, which the Agent Platform chart renders. An agent in an organization namespace gets no installation skills credential (`skills.gitAuthSecretName`, `skills.mintGitAuthSecret`): it names its own. Read before you enable it: Helm keeps its release Secrets in the namespace and must `list` them, so `agent-platform-tenant-flux` can read every Secret of an organization namespace. A chart rendered through it can copy such a Secret with Helm `lookup` into an object it renders, overwrite or delete any Secret there, and get the token of any ServiceAccount there. Whoever may create or change a HelmRelease in the namespace can trigger that: the members bound to `agent-platform:editor` or `agent-platform:admin`. |
| tenants.commit.repository | string | `""` | The installation's repository (`owner/name`) the organization tools' `mode: commit` opens its pull requests in, as the person. Empty: `mode: commit` is not offered for organizations. Needs `github.enabled`. The repository's CODEOWNERS must name the platform team for `objectsPath` and `contentPrefix`, and its `.sops.yaml` must cover `contentPrefix` (docs/organizations-commit-mode.md). |
| tenants.commit.branch | string | `"main"` | The branch the pull requests go to. |
| tenants.commit.objectsPath | string | `""` | The directory the installation's Kustomization applies the organization objects from: `<objectsPath>/<name>.yaml`. |
| tenants.commit.contentPrefix | string | `"agent-platform/organizations"` | The directory of the organizations' content paths, `<contentPrefix>/<name>`: agent-platform-operator's `commitMode.path` without `{name}`. |
| tenants.commit.platformModelsPath | string | `""` | Trusted repository-relative directory for installation model commits. |
| credentials.uiURL | string | `""` | HTTPS RFC 6570 URI template with one {operation} expression, for organization and installation models. It uses only {operation}, {organization} and {namespace}, in the path or query, never in the host. Installation model links need {namespace} and no {organization}, required without tenants.enabled. The UI owns its routes; agent-manager supplies only a non-authorizing operation ID. Requires downstream OAuth. |
| credentials.operationsNamespace | string | `""` | Dedicated namespace for nonsecret operation metadata. Empty derives a namespace from the release. Do not share it with application config. |
| installation | string | `""` | The installation's name in the Dev Portal, reported by `get_info` as `installation`: a portal agent id is `<installation>/<namespace>/<name>`. Empty: not reported. |
| podAnnotations | object | `{}` | Annotations on the pod. |
| podLabels | object | `{}` | Labels on the pod. |
| podSecurityContext | object | `{"fsGroup":1000,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Container security context. |
| service.type | string | `"ClusterIP"` | Service type. |
| service.port | int | `8080` | Service port (container listens on 8080). |
| resources | object | `{"limits":{"cpu":"500m","ephemeral-storage":"100Mi","memory":"512Mi"},"requests":{"cpu":"50m","ephemeral-storage":"50Mi","memory":"64Mi"}}` | Container resources. |
| observability.otel.endpoint | string | `""` | OTLP collector URL for traces, e.g. `http://otlp-gateway.kube-system.svc:4317` (gRPC) or `https://tempo.example.com:4318` (with protocol `http/protobuf`). Empty exports nothing; inbound `traceparent` headers still propagate. With `networkPolicy.enabled` the policy opens egress to it: the Service's namespace for an in-cluster host, every address otherwise. |
| observability.otel.protocol | string | `"grpc"` | OTLP protocol: `grpc` or `http/protobuf`. |
| observability.otel.headers | string | `""` | OTLP headers (`OTEL_EXPORTER_OTLP_HEADERS`), e.g. `X-Scope-OrgID=giantswarm` for a multi-tenant collector. |
| observability.otel.resourceAttributes | string | `""` | Resource attributes appended to the downward-API `k8s.pod.name`, `k8s.namespace.name` and `k8s.node.name`, e.g. `deployment.environment=glean`. |
| observability.otel.sampler | string | `"parentbased_traceidratio"` | `OTEL_TRACES_SAMPLER`. The parent-based default follows the caller's sampling decision and samples `samplerArg` of the traces that start here. |
| observability.otel.samplerArg | string | `"0.1"` | `OTEL_TRACES_SAMPLER_ARG`. |
| observability.metrics.enabled | bool | `true` | Serve Prometheus metrics on `GET /metrics` of the `metrics` port: `mcp_server_operation_duration_seconds` per tool, and `http_server_request_duration_seconds` for the REST API and the MCP endpoint. Off, `OTEL_METRICS_EXPORTER=none`: nothing is served, and nothing is pushed to `observability.otel.endpoint` either. |
| observability.metrics.port | int | `9464` | Port of the metrics listener, apart from the API port so an HTTPRoute or the OAuth guard never covers it. |
| serviceMonitor.enabled | bool | `false` | Render a ServiceMonitor for the `metrics` port. Also needs `observability.metrics.enabled`; the meta chart turns this on with the tenant label. |
| serviceMonitor.interval | string | `""` | Scrape interval; empty uses the Prometheus Operator default. |
| serviceMonitor.labels | object | `{}` | Labels on the ServiceMonitor, beside the chart's own. The Giant Swarm observability platform routes a scrape to a Mimir tenant by `observability.giantswarm.io/tenant`; a monitor without it writes to no tenant. |
| logging.verbose | bool | `false` | Enable debug logging. |
| extraArgs | list | `[]` | Extra container arguments. |
| extraEnv | list | `[]` | Extra environment variables. |
| nodeSelector | object | `{}` | Node selector. |
| tolerations | list | `[]` | Tolerations. |
| affinity | object | `{}` | Affinity. |
