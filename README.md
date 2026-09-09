# agent-manager

[![CircleCI](https://dl.circleci.com/status-badge/img/gh/giantswarm/agent-manager/tree/main.svg?style=shield)](https://dl.circleci.com/status-badge/redirect/gh/giantswarm/agent-manager/tree/main)

Agent lifecycle service for the Giant Swarm Agent Platform: the write surface
for **agents themselves**, the sibling of [model-manager](https://github.com/giantswarm/model-manager)
(models) next to muster's own management tools (MCP servers, workflows).

> **POC branch (`poc/kagent-main`)**: this branch composes agents for
> **kagent `main`** (`kagent.dev/v1alpha3`, API v2). The fleet runs kagent
> 0.10 (`v1alpha2`, Flux HelmReleases of the `agent` chart); nothing here
> lands on `main` until the platform moves.

On kagent main an agent is a `kagent.dev/v1alpha3` **`AgentTemplate`** in the
kagent namespace — description, system prompt, model config, skills pinned to
immutable sources and one MCP tool binding — that kagent compiles for every
**`Harness`** whose admission selector matches its labels
(`kagent.dev/harness: <harness>`; the platform renders one Harness per runtime:
`kagent` for the Go ADK, `claude` for the Claude harness). Conversations are
`AgentInstance`s created from the compiled template over kagent's own gRPC API
(the portal); agent-manager never creates instances. The toolset an agent
declares travels as its **toolset carrier**: a per-agent copy of the platform's
`muster` `RemoteMCPServer` (`muster-<agent>`) carrying the `X-Muster-Toolset`
header, which the template binds instead of the platform server. agent-manager
composes both objects as the caller, validates what kagent main can express
before anything is applied, and reads readiness from the template's
per-Harness status.

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
("create and manage versioned agents"): the reconciler half is kagent's
controller (templates compile to revisions, instances pin them), this is the
MCP-server-writer half.

## API at a glance

| Operation | REST | MCP tool | Writes |
|---|---|---|---|
| Version, kagent API versions, managed namespaces, the platform's muster server and default Harness, capabilities, identity | `GET /api/v1/info` | `get_info` | no |
| Agents of a namespace (every AgentTemplate with its toolset carrier) | `GET /api/v1/agents?namespace=` | `list_agents` | no |
| One agent: declaration, template spec, toolset, carrier, per-Harness status | `GET /api/v1/agents/{ns}/{name}` | `get_agent` | no |
| Create: toolset carrier + AgentTemplate, after every check | `POST /api/v1/agents` | `create_agent` | RemoteMCPServer, AgentTemplate |
| Update: merge into the declaration, re-compose, write what differs | `PATCH /api/v1/agents/{ns}/{name}[?force=true]` | `update_agent` | RemoteMCPServer and/or AgentTemplate |
| Delete: the AgentTemplate and its carrier | `DELETE /api/v1/agents/{ns}/{name}[?force=true]` | `delete_agent` | AgentTemplate, RemoteMCPServer |
| Status verdict: per-Harness conditions, revisions, warnings; carrier acceptance | `GET /api/v1/agents/{ns}/{name}/status` | `get_agent_status` | no |
| Dry run of create/update: composed manifests + every violation | `POST /api/v1/agents/validate` | `validate_agent` | no |
| kagent ModelConfigs of a namespace | `GET /api/v1/modelconfigs?namespace=` | `list_model_configs` | no |
| Skills (`SKILL.md`) of the configured GitHub repositories, with the commit each pins | `GET /api/v1/skills[?repository=&ref=&refresh=]` | `list_skills` | no |
| Health | `GET /healthz`, `GET /readyz` | — | no |

Errors are `{"error":{"code":"not_found|invalid_request|conflict|forbidden|unsupported|backend_error","message":"…"}}`;
`conflict` (409) covers "exists already", "GitOps-owned", "written by someone
else" and "carries fields agent-manager does not compose" — the cases `force`
overrides where documented.

## What an agent is made of

`create_agent` takes the technical **name** (a DNS-1123 label the caller
chose and confirmed — the service never derives one from a display name, per
the creating-agents PRD), the **modelConfig** (must exist in the namespace;
the error lists the valid ones), the **toolset** (required, see below), and
optionally `harness` (the kagent Harness that runs the agent; the installation
default when omitted — a Harness admitting the template must exist, the error
names the Harnesses and what they admit), `displayName`, `description`,
`systemMessage`, `skills` (`gitRefs` pinned to a commit — `list_skills` reports
it — and digest-pinned OCI `refs`), `labels`, `annotations`, `namespace`. It
emits only what was set and composes, in the kagent namespace:

```yaml
apiVersion: kagent.dev/v1alpha3
kind: RemoteMCPServer                  # the toolset carrier: a copy of the platform's `muster` server
metadata:
  name: muster-sre
  namespace: kagent
  labels: {app.kubernetes.io/managed-by: agent-manager, agent-manager.giantswarm.io/agent: sre, kagent.dev/discovery: disabled}
spec:
  description: Shared muster MCP gateway for platform agents — toolset [preset:read-only] of agent sre
  url: http://muster.agent-platform.svc.cluster.local:8090/mcp
  protocol: STREAMABLE_HTTP
  headersFrom:
    - {name: X-Muster-Toolset, value: "preset:read-only"}   # never an Authorization header
---
apiVersion: kagent.dev/v1alpha3
kind: AgentTemplate
metadata:
  name: sre
  namespace: kagent
  labels: {app.kubernetes.io/managed-by: agent-manager, kagent.dev/harness: kagent}
  annotations: {ui.giantswarm.io/display-name: SRE Assistant, agent-manager.giantswarm.io/requested-by: admin@example.com}
spec:
  description: helps
  systemPrompt: …
  modelConfig: {name: default-model-config}
  skills:
    - name: runbooks
      source: {git: {url: https://github.com/giantswarm/agent-skills, commit: <40 hex>}, path: runbooks}
  tools:
    - mcp: {server: {kind: RemoteMCPServer, name: muster-sre}}   # the whole server: per-tool selection needs discovery, which is off
```

What an AgentTemplate cannot express is refused with the reason, never dropped
silently, and `get_info.capabilities` says so up front: `runtime` (the Harness
is the runtime — pass `harness`), `iconUrl` (no icon field), a skill on a
branch or an OCI tag (`mutableSkillRefs`), `skills.gitAuthSecretName` (sources
are read anonymously), a per-tool selection (`perToolSelection`), and the
former `toolNames`.

ModelConfigs, their Secrets, the Harnesses and the platform's `muster`
`RemoteMCPServer` are platform-admin owned: agent-manager only reads them.

## The toolset

Every agent declares a **toolset**: the list of selectors that bounds which of
the gateway's tools its meta-tools can see and call. It rides on the agent's
toolset carrier as the `X-Muster-Toolset` header; kagent's runtime sends it
with every MCP call next to the caller's forwarded bearer, and muster resolves
it per request and per caller. agent-manager validates the inline grammar and
composes the list exactly as given — it never resolves a toolset.

- Selectors: `preset:<name>`, `server:<name>`, `workflow:<name>`,
  `tool:<name>` — exact, case-sensitive names; at most 32 inline (define a
  preset for more). `toolset:<name>` is reserved; `label:` selectors exist
  inside presets only.
- Presets every installation has: `read-only`, `none`, `infrastructure`,
  `agent-platform`, `full`. `create_agent` refuses a request without a
  toolset and names them: `preset:none` for a chat-only agent without tools,
  `preset:full` for the deliberate choice of every tool the gateway exposes.
  An empty list is refused too, so "no tools" is never confused with implicit
  full access. Every declared toolset gets a carrier — `preset:full` and
  `preset:none` included — so the declaration is always readable on the
  cluster.
- `update_agent` replaces the whole list. A toolset change rewrites the
  carrier only: the template does not change, so kagent compiles **no new
  revision**.
- `get_agent` / `list_agents` report the declared `toolset`, or
  `implicitFullAccess: true` for a template that binds the platform's `muster`
  server directly (written by hand, or before toolsets existed): its tools are
  every tool the gateway exposes to the caller. Assign it a toolset with
  `update_agent` (`force`, since agent-manager did not create it): the carrier
  appears and the template is rebound to it.

It is composition, not authorization: the invoking human's identity and the
backends' own authorization remain the boundary.

## Revisions, readiness and ownership

kagent compiles every AgentTemplate for each admitting Harness into a
revision and reports one `status.harnesses[]` entry per Harness (`Accepted`,
`ResolvedRefs`, `Compatible`, then `Ready` once the golden snapshot exists;
`desiredRevision`, `latestSuccessfulRevision`, compile `warnings`).
`get_agent_status` folds that into one verdict — `ready` when any admitting
Harness reports Ready for the current generation, `failed` on a False stage or
when no Harness admits the template, `progressing` otherwise — plus the
carrier's acceptance. Running `AgentInstance`s keep the revision they were
created with: an update that changes the template says so in its `note`.

`list_agents` says how each agent is managed:

- `agent-manager` — created here (the `app.kubernetes.io/managed-by` label):
  writable.
- `gitops` — the template carries `kustomize.toolkit.fluxcd.io/name`: its
  desired state lives in git and a live write would be undone on the next
  reconciliation. `update_agent` and `delete_agent` refuse it unless `force`.
  A meta agent opens a pull request in the GitOps repository instead.
- `external` — written by someone else (kubectl, a chart): taken over with
  `force` only.

A template carrying fields agent-manager does not compose — bucket skills,
plugins, prompt templates, `systemPromptFrom`, agent tool bindings, other MCP
servers, a per-tool selection — lists them under `unmanagedFields`; an update
would drop them and is refused unless `force`.

## Validation

Every create and update is checked before anything is applied: the name and
toolset grammar, the ModelConfig against the namespace, the Harness admission
(a Harness of the namespace whose `allowedAgentTemplates` selector matches the
composed labels), the skill sources (a full commit id or a digest), the
platform's `muster` RemoteMCPServer (the carrier is a copy of it). A create
writes the carrier first, then the template; a template the apiserver refuses
takes its carrier with it. `validate_agent` returns the composed manifests and
every violation without writing.

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
everything a request does — the AgentTemplate and RemoteMCPServer writes, the
Harness and ModelConfig reads — through per-caller
clients (`internal/kube.CallerProvider`, built from
`rest.AnonymousClientConfig` + the caller's bearer, cached until the token's
`exp`). The user's RBAC governs; the ServiceAccount holds **no** permissions
(the chart renders no Role or RoleBinding with `oauth.downstream.enabled`) and
there is no fallback: a request without an IdP token is refused with `401`, a
token that expires mid-request keeps being presented and the apiserver's `401`
fails the request, attributed to the caller. agent-manager has no background
Kubernetes work — API version discovery at startup (`agenttemplates`) is what
every authenticated principal may read — so the ServiceAccount token stays mounted only for the
in-cluster address and CA. The apiserver must trust the token: with Dex the
audience it trusts (`dex-k8s-authenticator` on Giant Swarm clusters,
`kubernetes` in agentlab) is requested as a cross-client scope through
`requiredAudiences`; a Google IdP has no cross-client scopes and the platform
client id *is* the apiserver's `--oidc-client-id` (`requiredAudiences: []`).
`get_info` reports `identity: caller` and `capabilities.writesAsCaller: true`.

Without `--enable-oauth` the service checks no identity and acts as its
ServiceAccount (the Role per managed namespace: AgentTemplates and
RemoteMCPServers read/write; Harnesses and ModelConfigs read) — only for a
server nothing but a trusted proxy (the agentgateway JWT policy, muster) can
reach.

## Running

```sh
agent-manager serve \
  --kubeconfig ~/.kube/config --kube-context kind-agentlab \
  --kagent-namespace kagent --kagent-muster-server muster --kagent-default-harness kagent \
  --skills-repositories https://github.com/giantswarm/agent-skills
```

Every flag has an environment variable (`agent-manager serve --help`).
Kubernetes access is required; the kagent.dev API version is discovered from
the server (`--kagent-api-version auto`, the version serving `agenttemplates`,
default `v1alpha3`). The binary needs neither Flux CRDs nor the `agent` chart.
The flags of the retired Flux/agent-chart composition
(`--agent-chart-*`, `--flux-*`, `--helmrelease-*`, `--ocirepository-interval`)
still parse and do nothing, so a chart release that passes them keeps
starting this binary (a deprecation notice is printed per flag).

## Helm chart

`helm/agent-manager` — see its [README](helm/agent-manager/README.md). Keys
the [`giantswarm/agent-platform`](https://github.com/giantswarm/agent-platform)
meta chart sets for its `agent-manager` component: `kagent.namespace`,
`mcp.enabled`, `oauth.*`, `muster.mcpServer.*`, `skills.repositories`;
`kagent.musterServer` and `kagent.defaultHarness` name the platform's muster
server and the default Harness. Optional, off by default:
`muster.mcpServer.enabled` (renders an `mcpservers.muster.giantswarm.io` CR),
`httpRoute.enabled`, `networkPolicy.enabled`. With `oauth.downstream.enabled`
(the platform's mode) the chart renders no RBAC — the caller's RBAC on
`agenttemplates`, `remotemcpservers`, `harnesses` and `modelconfigs` in
`kagent.dev` governs; without it the Role grants exactly those.

## Development

See [docs/development.md](docs/development.md).
