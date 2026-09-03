# agent-manager

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.1.0](https://img.shields.io/badge/AppVersion-0.1.0-informational?style=flat-square)

Agent lifecycle service for the Agent Platform — creates, updates, deletes and inspects kagent agents as Flux HelmReleases of the agent chart, validated against the chart schema, exposed as REST and MCP

The chart deploys one Deployment that serves the REST/JSON API under `/api/v1`
(contract: `/api/v1/openapi.yaml`) and the MCP streamable-HTTP endpoint under
`/mcp`. An agent is a Flux `HelmRelease` of the `agent` chart (one release
renders one kagent `Agent`) plus the shared per-namespace `OCIRepository` of
that chart; agent-manager composes both the way the portal's create flow does,
validates the values against the chart's `values.schema.json` before applying,
and reads agents back from the Agent CR, the HelmRelease and the workload.

- `kagent.namespace` (plus `kagent.additionalNamespaces`) are the namespaces
  agents live in; the Role is created there. Requests naming another
  namespace are refused.
- `agentChart.ociUrl` / `agentChart.semver` are what the composed
  OCIRepository carries; the same registry is read at run time for the latest
  version and its values schema (`GET /api/v1/info` says which schema is in
  use — the embedded copy is the offline fallback).
- `skills.repositories` are the GitHub repositories `list_skills` discovers
  `SKILL.md` files in; `skills.github.tokenSecret` names a Secret with a token
  for private repositories.
- Writes run under the chart's ServiceAccount; the service checks no identity
  itself. On the platform the agentgateway JWT policy in front of the route
  (rendered by `agent-platform-standalone`) and muster in front of the MCP
  endpoint are the trust boundary.

Used as a dependency of `agent-platform-standalone` behind a `condition`;
standalone installs set `kagent.namespace`, `image.*`, `mcp.enabled`,
`muster.mcpServer.*` and `skills.repositories`.

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
| image.registry | string | `"gsoci.azurecr.io"` | Image registry. |
| image.repository | string | `"giantswarm/agent-manager"` | Image repository. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.tag | string | `""` | Image tag. Defaults to the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets. |
| nameOverride | string | `""` | Override the chart name. |
| fullnameOverride | string | `""` | Override the fully qualified release name (the umbrella chart pins the Service name through this). |
| kagent.namespace | string | `"kagent"` | Namespace agents are created in and listed from by default — the namespace kagent watches and the platform admin provisions ModelConfigs into. The Role for HelmReleases, OCIRepositories, Agents, ModelConfigs, Deployments, pods and events is created here. |
| kagent.additionalNamespaces | list | `[]` | Additional namespaces agents may live in (multi-tenant installs). Each gets the same Role; requests naming any other namespace are refused. |
| kagent.apiVersion | string | `"auto"` | kagent.dev API version for Agents and ModelConfigs; `auto` discovers the server's preferred version. |
| agentChart.ociUrl | string | `"oci://gsoci.azurecr.io/charts/giantswarm/agent"` | OCI URL of the `agent` chart every agent renders from. Written into the shared per-namespace OCIRepository, and read at run time for the chart's values.schema.json that validates every create/update before it is applied (the pod needs egress to this registry; the embedded copy of the schema is the offline fallback). |
| agentChart.semver | string | `"x.x.x"` | Semver range the OCIRepository tracks. `x.x.x` follows every published release, the platform convention for agents. |
| agentChart.refresh | string | `"10m"` | How often the registry is re-read for the latest version and schema. |
| flux.helmReleaseInterval | string | `"10m"` | HelmRelease.spec.interval of every composed agent. |
| flux.ociRepositoryInterval | string | `"30m"` | OCIRepository.spec.interval of the shared chart source. |
| flux.helmReleaseServiceAccount | string | `""` | HelmRelease.spec.serviceAccountName, required by a Flux multi-tenancy admission policy in tenant namespaces; empty omits it. |
| flux.helmReleaseApiVersion | string | `"auto"` | helm.toolkit.fluxcd.io API version composed into HelmReleases; `auto` discovers it. |
| flux.ociRepositoryApiVersion | string | `"auto"` | source.toolkit.fluxcd.io API version composed into OCIRepositories; `auto` discovers it. |
| skills.repositories | list | `["https://github.com/giantswarm/agent-skills"]` | GitHub repositories whose SKILL.md files list_skills offers (the same list the portal's create flow reads). Empty turns the skills capability off. |
| skills.github.apiUrl | string | `"https://api.github.com"` | GitHub API base URL. |
| skills.github.tokenSecret.name | string | `""` | Existing Secret in the release namespace with a GitHub token (private skill repositories, higher rate limit). Empty: anonymous. |
| skills.github.tokenSecret.key | string | `"token"` | Key of the token in that Secret. |
| skills.cacheTTL | string | `"5m"` | How long a repository's discovered skills are reused. |
| mcp.enabled | bool | `true` | Serve the MCP streamable-HTTP endpoint alongside the REST API. |
| mcp.path | string | `"/mcp"` | MCP endpoint path. |
| muster.mcpServer.enabled | bool | `false` | Register this server with muster by rendering an `mcpservers.muster.giantswarm.io` CR in the release namespace. Tools then appear as `x_<name>_<tool>`. |
| muster.mcpServer.name | string | `"agent-manager"` | MCPServer CR name (drives the tool prefix). |
| muster.mcpServer.autoStart | bool | `true` | Start the server connection when muster initializes. |
| muster.mcpServer.description | string | `"Agent lifecycle (create, update, delete, status, skills, model configs) for the Agent Platform"` | Human-readable description shown by muster. |
| muster.mcpServer.labels | object | `{}` | Extra labels on the MCPServer CR. |
| httpRoute.enabled | bool | `false` | Expose the service through a Gateway API HTTPRoute. |
| httpRoute.parentRefs | list | `[]` | parentRefs of the HTTPRoute (required when enabled). |
| httpRoute.hostnames | list | `[]` | Hostnames matched by the route. |
| httpRoute.annotations | object | `{}` | Annotations on the HTTPRoute. |
| httpRoute.labels | object | `{}` | Labels on the HTTPRoute. |
| networkPolicy.enabled | bool | `false` | Create a Kubernetes NetworkPolicy for the pod (off when an umbrella chart renders the policies). |
| networkPolicy.ingressNamespaces | list | `[]` | Namespaces allowed to reach the API (label kubernetes.io/metadata.name). Empty allows ingress from the release namespace only. |
| networkPolicy.allowKubeAPI | bool | `true` | Allow egress to the Kubernetes API server. |
| networkPolicy.allowInternet | bool | `true` | Allow egress on 443 to every destination: the chart registry (schema and version) and GitHub (skills). Vanilla NetworkPolicy cannot select by name; narrow with egressCIDRs instead when the addresses are known. |
| networkPolicy.egressCIDRs | list | `[]` | Extra egress CIDRs. |
| serviceAccount.create | bool | `true` | Create a ServiceAccount. |
| serviceAccount.annotations | object | `{}` | Annotations on the ServiceAccount. |
| serviceAccount.name | string | `""` | ServiceAccount name (generated when empty). |
| rbac.create | bool | `true` | Create the Role/RoleBinding in `kagent.namespace` (and every `kagent.additionalNamespaces` entry): HelmReleases and OCIRepositories read/write; Agents, ModelConfigs, Deployments, pods and events read. |
| podAnnotations | object | `{}` | Annotations on the pod. |
| podLabels | object | `{}` | Labels on the pod. |
| podSecurityContext | object | `{"fsGroup":1000,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | Container security context. |
| service.type | string | `"ClusterIP"` | Service type. |
| service.port | int | `8080` | Service port (container listens on 8080). |
| resources | object | `{"limits":{"cpu":"500m","memory":"256Mi"},"requests":{"cpu":"50m","memory":"64Mi"}}` | Container resources. |
| logging.verbose | bool | `false` | Enable debug logging. |
| extraArgs | list | `[]` | Extra container arguments. |
| extraEnv | list | `[]` | Extra environment variables. |
| nodeSelector | object | `{}` | Node selector. |
| tolerations | list | `[]` | Tolerations. |
| affinity | object | `{}` | Affinity. |
