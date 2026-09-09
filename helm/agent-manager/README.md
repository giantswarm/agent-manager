# agent-manager

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.1.0](https://img.shields.io/badge/AppVersion-0.1.0-informational?style=flat-square)

Agent lifecycle service for the Agent Platform — creates, updates, deletes and inspects agents as kagent.dev/v1alpha3 AgentTemplates with a per-agent toolset carrier, validated before anything is applied, exposed as REST and MCP

The chart deploys one Deployment that serves the REST/JSON API under `/api/v1`
(contract: `/api/v1/openapi.yaml`) and the MCP streamable-HTTP endpoint under
`/mcp`. On kagent main an agent is a `kagent.dev/v1alpha3` `AgentTemplate` in
the kagent namespace plus, for the toolset it declares, a per-agent copy of
the platform's `muster` `RemoteMCPServer` carrying the `X-Muster-Toolset`
header (the toolset carrier, `muster-<agent>`); agent-manager composes both,
validates what kagent can express before applying, and reads readiness from
the template's per-Harness status.

- `kagent.namespace` (plus `kagent.additionalNamespaces`) are the namespaces
  agents live in; the Role is created there. Requests naming another
  namespace are refused.
- `kagent.musterServer` names the platform's muster `RemoteMCPServer` every
  agent binds and every toolset carrier is copied from; `kagent.defaultHarness`
  is the Harness an agent runs on when the request names none (the template is
  labelled `kagent.dev/harness: <name>`).
- `skills.repositories` are the GitHub repositories `list_skills` discovers
  `SKILL.md` files in (resolving each branch to the commit a template pins);
  `skills.github.tokenSecret` names a Secret with a token for private
  repositories.
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
`mcp.enabled`, `oauth.*`, `muster.mcpServer.*` and `skills.repositories`.

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
| global | object | `{}` |  |
| kagent.namespace | string | `"kagent"` | Namespace agents are created in and listed from by default — the namespace the kagent controller watches, the platform admin provisions ModelConfigs into and the connectivity chart renders the `muster` RemoteMCPServer and the Harnesses into. The Role for AgentTemplates, RemoteMCPServers, Harnesses and ModelConfigs is created here. |
| kagent.additionalNamespaces | list | `[]` | Additional namespaces agents may live in (multi-tenant installs). Each gets the same Role and needs its own platform `muster` RemoteMCPServer; requests naming any other namespace are refused. |
| kagent.apiVersion | string | `"auto"` | kagent.dev API version for AgentTemplates, RemoteMCPServers, Harnesses and ModelConfigs; `auto` discovers the version serving agenttemplates (default v1alpha3). |
| kagent.musterServer | string | `"muster"` | Name of the platform's muster RemoteMCPServer in every managed namespace: the server every agent binds and every per-agent toolset carrier (`<musterServer>-<agent>`, headersFrom X-Muster-Toolset) is copied from. The connectivity chart renders it. |
| kagent.defaultHarness | string | `"kagent"` | Harness an agent runs on when the request names none; the AgentTemplate is labelled `kagent.dev/harness: <name>`, which the platform's Harnesses select on. |
| skills.repositories | list | `["https://github.com/giantswarm/agent-skills"]` | GitHub repositories whose SKILL.md files list_skills offers (the same list the portal's create flow reads). Empty turns the skills capability off. |
| skills.github.apiUrl | string | `"https://api.github.com"` | GitHub API base URL. |
| skills.github.tokenSecret.name | string | `""` | Existing Secret in the release namespace with a GitHub token (private skill repositories, higher rate limit). Empty: anonymous. |
| skills.github.tokenSecret.key | string | `"token"` | Key of the token in that Secret. |
| skills.cacheTTL | string | `"5m"` | How long a repository's discovered skills are reused. |
| mcp.enabled | bool | `true` | Serve the MCP streamable-HTTP endpoint alongside the REST API. |
| mcp.path | string | `"/mcp"` | MCP endpoint path. |
| oauth.enabled | bool | `false` | Make agent-manager an OAuth 2.1 resource server (mcp-oauth): the MCP endpoint and the REST API require a bearer token the platform identity provider issued, and every call carries the caller's identity (logged on every write, returned as `requestedBy`). On the Agent Platform muster forwards the session's IdP id_token to this server (MCPServer `auth.forwardToken`, rendered below) and the portal sends the signed-in user's id_token through the gateway; both are validated against the IdP's JWKS when their audience is in `trustedAudiences`. Off: anonymous, acting as the ServiceAccount — only for a server nothing but a trusted proxy can reach. |
| oauth.baseURL | string | `""` | Public base URL of this server: the issuer of its own OAuth metadata (https, or http on loopback). Empty derives `https://<fullname>.<global.domain>` when `global.domain` is set. |
| oauth.provider | string | `"dex"` | Identity provider: `dex` or `google`. |
| oauth.dex.issuerURL | string | `""` | Dex issuer URL. Empty falls back to `global.identity.issuerUrl`. |
| oauth.dex.clientID | string | `""` | Dex OAuth client ID. Empty falls back to `global.identity.clientId`. |
| oauth.dex.clientSecret | string | `""` | Dex OAuth client secret (prefer `oauth.existingSecret`). |
| oauth.dex.allowPrivateURLs | bool | `false` | Let the issuer resolve to a private or loopback address (an in-cluster Dex). |
| oauth.dex.caSecret | object | `{"key":"ca.crt","name":""}` | Secret with the CA of a Dex that serves a private certificate; mounted and passed as `--dex-ca-file`. Empty name falls back to `global.identity.ca.secretName` / `global.identity.ca.key`. |
| oauth.google.clientID | string | `""` | Google OAuth client ID (not secret; may also come from the Secret key `google-client-id` when empty). |
| oauth.google.clientSecret | string | `""` | Google OAuth client secret (prefer `oauth.existingSecret`). |
| oauth.existingSecret | string | `""` | Existing Secret with the provider credentials: `dex-client-secret` (dex) or `google-client-secret` (+ optional `google-client-id`) (google). Empty falls back to `global.identity.existingSecret`, whose `dex-client-secret` is the platform client's; without that, the chart renders a Secret from the values above. |
| oauth.trustedAudiences | list | `[]` | OAuth client IDs whose IdP id_tokens are accepted as bearer tokens (SSO token forwarding). Empty falls back to `[global.identity.clientId]`, the platform client MCP clients and the muster CLI log in with. The server trusts the union of this list and `muster.mcpServer.auth.requiredAudiences` (in that order, without duplicates): every token muster forwards carries the required audiences by construction and they are what the kube-apiserver trusts, so a portal session — whose id_token carries them but not the platform client — is accepted without listing its client here. |
| oauth.sso.allowPrivateIPs | bool | `false` | Let the IdP's JWKS endpoint resolve to a private address when validating forwarded tokens (an in-cluster Dex). |
| oauth.allowPublicClientRegistration | bool | `false` | Accept unauthenticated dynamic client registration (labs only). |
| oauth.downstream.enabled | bool | `false` | Call the Kubernetes API as the caller: everything a request does (the AgentTemplates and RemoteMCPServers it writes, the Harnesses and ModelConfigs it reads) presents the caller's IdP token, so the caller's RBAC governs — the apiserver must trust the IdP and the token's audience (a Dex install lists that audience in `muster.mcpServer.auth.requiredAudiences`; a Google install's client id is the apiserver's `--oidc-client-id`). The ServiceAccount then holds no permissions: the chart renders none of its Roles (`rbac.create` is moot) and the token stays mounted only for the in-cluster API address and CA plus API discovery at startup, which every authenticated principal may read. agent-manager has no background work, so nothing is lost: a request without a token is refused with 401 instead of running as the ServiceAccount. |
| muster.mcpServer.enabled | bool | `false` | Register this server with muster by rendering an `mcpservers.muster.giantswarm.io` CR in the release namespace. Tools then appear as `x_<name>_<tool>`. |
| muster.mcpServer.name | string | `"agent-manager"` | MCPServer CR name (drives the tool prefix). |
| muster.mcpServer.autoStart | bool | `true` | Start the server connection when muster initializes. |
| muster.mcpServer.description | string | `"Agent lifecycle (create, update, delete, status, skills, model configs) for the Agent Platform"` | Human-readable description shown by muster. |
| muster.mcpServer.labels | object | `{}` | Extra labels on the MCPServer CR. |
| muster.mcpServer.auth | object | `{"forwardToken":true,"requiredAudiences":[]}` | How muster authenticates to this server; rendered only with `oauth.enabled`. `forwardToken` makes muster forward the session's IdP id_token byte-identical (the SSO path this chart trusts through its trusted audiences). `requiredAudiences` are extra audiences that token must carry — the Dex cross-client audience the kube-apiserver trusts (`dex-k8s-authenticator` on Giant Swarm clusters; agentlab's is `kubernetes`) so `oauth.downstream` works; muster requests them at login, so users re-login after a change. They are trusted as bearer audiences by construction, since the server's trusted audiences are `oauth.trustedAudiences` plus this list. A Google IdP has no cross-client audiences: leave the list empty. |
| httpRoute.enabled | bool | `false` | Expose the service through a Gateway API HTTPRoute. |
| httpRoute.parentRefs | list | `[]` | parentRefs of the HTTPRoute (required when enabled). |
| httpRoute.hostnames | list | `[]` | Hostnames matched by the route. |
| httpRoute.annotations | object | `{}` | Annotations on the HTTPRoute. |
| httpRoute.labels | object | `{}` | Labels on the HTTPRoute. |
| networkPolicy.enabled | bool | `false` | Create a Kubernetes NetworkPolicy for the pod (off when an umbrella chart renders the policies). |
| networkPolicy.ingressNamespaces | list | `[]` | Namespaces allowed to reach the API (label kubernetes.io/metadata.name). Empty allows ingress from the release namespace only. |
| networkPolicy.allowKubeAPI | bool | `true` | Allow egress to the Kubernetes API server. |
| networkPolicy.allowInternet | bool | `true` | Allow egress on 443 to every destination: GitHub (skill discovery). Vanilla NetworkPolicy cannot select by name; narrow with egressCIDRs instead when the addresses are known. |
| networkPolicy.egressCIDRs | list | `[]` | Extra egress CIDRs. |
| serviceAccount.create | bool | `true` | Create a ServiceAccount. |
| serviceAccount.annotations | object | `{}` | Annotations on the ServiceAccount. |
| serviceAccount.name | string | `""` | ServiceAccount name (generated when empty). |
| rbac.create | bool | `true` | Create the Role/RoleBinding in `kagent.namespace` (and every `kagent.additionalNamespaces` entry): AgentTemplates and RemoteMCPServers read/write; Harnesses and ModelConfigs read — the ServiceAccount's own permissions. Ignored with `oauth.downstream.enabled`: the ServiceAccount then gets no RBAC at all, the caller's RBAC governs. |
| podAnnotations | object | `{}` | Annotations on the pod. |
| podLabels | object | `{}` | Labels on the pod. |
| podSecurityContext | object | `{"fsGroup":1000,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Container security context. |
| service.type | string | `"ClusterIP"` | Service type. |
| service.port | int | `8080` | Service port (container listens on 8080). |
| resources | object | `{"limits":{"cpu":"500m","ephemeral-storage":"100Mi","memory":"256Mi"},"requests":{"cpu":"50m","ephemeral-storage":"50Mi","memory":"64Mi"}}` | Container resources. |
| logging.verbose | bool | `false` | Enable debug logging. |
| extraArgs | list | `[]` | Extra container arguments. |
| extraEnv | list | `[]` | Extra environment variables. |
| nodeSelector | object | `{}` | Node selector. |
| tolerations | list | `[]` | Tolerations. |
| affinity | object | `{}` | Affinity. |
