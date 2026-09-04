# agent-manager

[![CircleCI](https://dl.circleci.com/status-badge/img/gh/giantswarm/agent-manager/tree/main.svg?style=shield)](https://dl.circleci.com/status-badge/redirect/gh/giantswarm/agent-manager/tree/main)

Agent lifecycle service for the Giant Swarm Agent Platform: the write surface
for **agents themselves**, the sibling of [model-manager](https://github.com/giantswarm/model-manager)
(models) next to muster's own management tools (MCP servers, workflows).

On the platform an agent is a Flux `HelmRelease` of the
[`agent` chart](https://github.com/giantswarm/agent) — one release renders one
kagent `Agent` — that renders from the shared per-namespace `OCIRepository` of
that chart, tracking it by semver range. That is exactly what the portal's
create flow composes; agent-manager composes the same two objects from a
small, curated argument set, validates the values against the chart's
`values.schema.json` **before** anything is applied, and reads an agent back
from its Agent CR, its owning HelmRelease (Flux provenance labels) and the
Deployment kagent runs for it.

The same operations are exposed twice from one process:

- **REST/JSON** under `/api/v1` for the portal backend — contract in
  [`api/openapi.yaml`](api/openapi.yaml), also served at `/api/v1/openapi.yaml`.
- **MCP** (streamable HTTP, `/mcp`) for muster — tools `get_info`,
  `list_agents`, `get_agent`, `create_agent`, `update_agent`, `delete_agent`,
  `get_agent_status`, `validate_agent`, `list_model_configs`, `list_skills`
  (through muster: `x_agent-manager_<tool>`). Every tool description says
  whether it writes and what it writes; the read-only and destructive
  annotations are set.

Part of the [Agent Control Plane epic](https://github.com/giantswarm/giantswarm/issues/36796)
("create and manage versioned agents"): the reconciler half is the `agent`
chart plus Flux helm-controller, this is the MCP-server-writer half.

## API at a glance

| Operation | REST | MCP tool | Writes |
|---|---|---|---|
| Version, chart (OCI URL, semver range, latest version, schema in use), managed namespaces, capabilities, identity | `GET /api/v1/info` | `get_info` | no |
| Agents of a namespace (Agent CRs + HelmReleases of the chart not rendered yet) | `GET /api/v1/agents?namespace=` | `list_agents` | no |
| One agent with its HelmRelease values | `GET /api/v1/agents/{ns}/{name}` | `get_agent` | no |
| Create: OCIRepository (when missing) + HelmRelease, after schema and ModelConfig validation | `POST /api/v1/agents` | `create_agent` | HelmRelease, OCIRepository |
| Update: merge into the HelmRelease values, validate, update | `PATCH /api/v1/agents/{ns}/{name}[?force=true]` | `update_agent` | HelmRelease |
| Delete: the HelmRelease; the OCIRepository only when nothing else references it | `DELETE /api/v1/agents/{ns}/{name}[?force=true]` | `delete_agent` | HelmRelease, OCIRepository, (bare Agent with force) |
| Status verdict: Agent + HelmRelease conditions/history, Deployment, pods (waiting reasons), Warning events | `GET /api/v1/agents/{ns}/{name}/status` | `get_agent_status` | no |
| Dry run of create/update: composed manifests + every violation | `POST /api/v1/agents/validate` | `validate_agent` | no |
| kagent ModelConfigs of a namespace | `GET /api/v1/modelconfigs?namespace=` | `list_model_configs` | no |
| Skills (`SKILL.md`) of the configured GitHub repositories | `GET /api/v1/skills[?repository=&ref=&refresh=]` | `list_skills` | no |
| Health | `GET /healthz`, `GET /readyz` | — | no |

Errors are `{"error":{"code":"not_found|invalid_request|conflict|forbidden|unsupported|backend_error","message":"…"}}`;
`conflict` (409) covers "exists already", "GitOps-owned", "suspended" and
"bare Agent CR" — the cases `force` overrides where documented.

## What an agent is made of

`create_agent` takes the technical **name** (a DNS-1123 label the caller
chose and confirmed — the service never derives one from a display name, per
the creating-agents PRD), the **modelConfig** (must exist in the namespace;
the error lists the valid ones), and optionally `displayName`, `description`,
`systemMessage`, `iconUrl`, `runtime` (go|python), `skills` (`gitRefs` from
`list_skills`, OCI `refs`), `toolNames` (narrow the muster tools; none means
every tool the gateway exposes), `labels`, `annotations`, `namespace`. It
emits only what was set so the chart's defaults apply to everything else —
the portal's rule — and composes:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata: {name: agent, namespace: kagent}
spec: {interval: 30m, url: oci://gsoci.azurecr.io/charts/giantswarm/agent, ref: {semver: x.x.x}}
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: sre, namespace: kagent}
spec:
  interval: 10m
  chartRef: {kind: OCIRepository, name: agent, namespace: kagent}
  values:
    agent: {name: sre, displayName: SRE Assistant, systemMessage: …}
    modelConfig: {name: default-model-config}
    skills: {gitRefs: [{url: https://github.com/giantswarm/agent-skills, path: runbooks, ref: main, name: runbooks}]}
```

ModelConfigs, their Secrets and the shared muster `RemoteMCPServer` are
platform-admin owned: agent-manager only reads ModelConfigs.

## Ownership and the meta agent's rule

`list_agents` says how each agent is managed:

- `helmrelease` — a HelmRelease agent-manager (or the portal, or a hand-applied
  manifest) owns: writable here.
- `gitops` — the HelmRelease carries `kustomize.toolkit.fluxcd.io/name`: its
  desired state lives in git and a live write would be undone on the next
  reconciliation. `update_agent` and `delete_agent` refuse it unless `force`.
  A meta agent opens a pull request in the GitOps repository instead.
- `none` — a bare Agent CR with no HelmRelease behind it: nothing to write to;
  `delete_agent` removes it only with `force`.

A suspended HelmRelease is refused the same way: Flux drops its finalizer
without uninstalling, so deleting it would leave the Agent behind (with `force`
the Agent is deleted too).

## Validation

Every create and update is validated against the `agent` chart's
`values.schema.json` before it is applied. The schema comes from the chart
registry (`agentChart.ociUrl`, the newest version in `agentChart.semver`, the
same resolution Flux's OCIRepository performs) and is re-read every
`agentChart.refresh`; when the registry cannot be reached the copy compiled
into the binary (chart 0.5.2) validates and `get_info` reports
`chart.schemaSource: embedded` with the error. `validate_agent` returns the
composed manifests and every violation without writing.

## Identity

The caller, not the ServiceAccount. With `--enable-oauth` agent-manager is an
OAuth 2.1 resource server ([mcp-oauth](https://github.com/giantswarm/mcp-oauth))
in front of **both** the MCP endpoint and the REST API; providers `dex` (with
`--dex-ca-file` and `--allow-private-oauth-urls` for an in-cluster Dex) and
`google`. Nobody logs in to agent-manager itself: muster forwards the session's
IdP id_token byte-identical (MCPServer `auth: {type: oauth, forwardToken: true,
requiredAudiences}`, rendered by the chart under `muster.mcpServer.auth`) and
the portal sends the signed-in user's id_token through the gateway; both are
validated against the IdP's JWKS because their audience is in
`--oauth-trusted-audiences`. Which audience that is depends on the session:
muster's own sessions and MCP clients sign in through the platform OAuth
client, a portal session forwards the id_token of the portal's own client —
and every forwarded token carries the audiences the MCPServer requires, the
ones the kube-apiserver trusts. So the chart trusts both: `oauth.trustedAudiences`
(default `[global.identity.clientId]`) plus `muster.mcpServer.auth.requiredAudiences`,
always. A token for none of them is refused with `401`; the log line and the
`WWW-Authenticate` description name its `aud` next to the trusted audiences
(muster shows the description in its session hint). The caller
(`internal/identity`: subject, email, groups, source `sso|oauth`) is on every
write's log line (`caller=`) and on every create/update/delete result as
`requestedBy`.

`--downstream-oauth` presents the caller's token to the kube-apiserver for
everything a request does — the HelmRelease and OCIRepository writes, the
Agent, ModelConfig, Deployment, pod and event reads — through per-caller
clients (`internal/kube.CallerProvider`, built from
`rest.AnonymousClientConfig` + the caller's bearer, cached until the token's
`exp`). The user's RBAC governs; the ServiceAccount holds **no** permissions
(the chart renders no Role or RoleBinding with `oauth.downstream.enabled`) and
there is no fallback: a request without an IdP token is refused with `401`, a
token that expires mid-request keeps being presented and the apiserver's `401`
fails the request, attributed to the caller. agent-manager has no background
Kubernetes work — API version discovery at startup is what every authenticated
principal may read — so the ServiceAccount token stays mounted only for the
in-cluster address and CA. The apiserver must trust the token: with Dex the
audience it trusts (`dex-k8s-authenticator` on Giant Swarm clusters,
`kubernetes` in agentlab) is requested as a cross-client scope through
`requiredAudiences`; a Google IdP has no cross-client scopes and the platform
client id *is* the apiserver's `--oidc-client-id` (`requiredAudiences: []`).
`get_info` reports `identity: caller` and `capabilities.writesAsCaller: true`.

Without `--enable-oauth` the service checks no identity and acts as its
ServiceAccount (the Role per managed namespace: HelmReleases and
OCIRepositories read/write; Agents, ModelConfigs, Deployments, pods and events
read; Agents delete for the forced bare-CR case) — only for a server nothing
but a trusted proxy (the agentgateway JWT policy, muster) can reach.

## Running

```sh
agent-manager serve \
  --kubeconfig ~/.kube/config --kube-context kind-agentlab \
  --kagent-namespace kagent \
  --agent-chart-oci-url oci://gsoci.azurecr.io/charts/giantswarm/agent \
  --skills-repositories https://github.com/giantswarm/agent-skills
```

Every flag has an environment variable (`agent-manager serve --help`).
Kubernetes access is required; the kagent.dev and Flux API versions are
discovered from the server (`--*-api-version auto`).

## Helm chart

`helm/agent-manager` — see its [README](helm/agent-manager/README.md). Keys
the umbrella chart (`agent-platform-standalone`) sets: `kagent.namespace`,
`image.*`, `mcp.enabled`, `muster.mcpServer.*`, `skills.repositories`.
Optional, off by default: `muster.mcpServer.enabled` (renders an
`mcpservers.muster.giantswarm.io` CR), `httpRoute.enabled`,
`networkPolicy.enabled`.

## Development

See [docs/development.md](docs/development.md).
