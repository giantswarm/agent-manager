# agent-manager

[![CircleCI](https://dl.circleci.com/status-badge/img/gh/giantswarm/agent-manager/tree/main.svg?style=shield)](https://dl.circleci.com/status-badge/redirect/gh/giantswarm/agent-manager/tree/main)

Agent lifecycle service for the Giant Swarm Agent Platform: the write surface
for **agents themselves**, the sibling of [model-manager](https://github.com/giantswarm/model-manager)
(models) next to muster's own management tools (MCP servers, workflows).

On the platform an agent is a Flux `HelmRelease` of the Generic
[`agent` chart](https://github.com/giantswarm/agent) (1.x) — one release
renders one `kagent.dev/v1alpha3` `AgentTemplate` plus the agent's own muster
`RemoteMCPServer` — that renders from the shared per-namespace `OCIRepository`
of that chart, tracking it at `1.x`. That is exactly what the portal's create
flow composes; agent-manager composes the same two objects from a small,
curated argument set, pins every skill to an immutable source, validates the
values against the chart's `values.schema.json` **before** anything is applied,
and reads an agent back from its AgentTemplate, its RemoteMCPServer and its
owning HelmRelease (Flux provenance labels). Readiness is what the platform
Harness reports on the template — there is no per-agent Deployment or pod; agents
run as Substrate actors.

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
chart plus Flux helm-controller and the kagent controller, this is the
MCP-server-writer half.

## API at a glance

| Operation | REST | MCP tool | Writes |
|---|---|---|---|
| Version, chart (OCI URL, `1.x` range, latest version, schema in use), managed namespaces, capabilities, API versions, platform Harness, muster URL, identity | `GET /api/v1/info` | `get_info` | no |
| Agents of a namespace (AgentTemplates + HelmReleases of the chart not rendered yet) | `GET /api/v1/agents?namespace=` | `list_agents` | no |
| One agent with its HelmRelease values, pinned skills, toolset and per-Harness status | `GET /api/v1/agents/{ns}/{name}` | `get_agent` | no |
| Create: OCIRepository (when missing) + HelmRelease, after skill pinning, schema and ModelConfig validation | `POST /api/v1/agents` | `create_agent` | HelmRelease, OCIRepository |
| Update: merge into the HelmRelease values (`refreshSkills` re-pins git skills), validate, update | `PATCH /api/v1/agents/{ns}/{name}[?force=true]` | `update_agent` | HelmRelease |
| Delete: the HelmRelease; the OCIRepository only when nothing else references it | `DELETE /api/v1/agents/{ns}/{name}[?force=true]` | `delete_agent` | HelmRelease, OCIRepository, (bare AgentTemplate with force) |
| Status verdict: the platform Harness's entry on the AgentTemplate, HelmRelease conditions/history, Warning events | `GET /api/v1/agents/{ns}/{name}/status` | `get_agent_status` | no |
| Dry run of create/update: pinned skills, composed manifests + every violation | `POST /api/v1/agents/validate` | `validate_agent` | no |
| kagent ModelConfigs of a namespace | `GET /api/v1/modelconfigs?namespace=` | `list_model_configs` | no |
| Skills (`SKILL.md`) of the configured GitHub repositories, each with its head commit | `GET /api/v1/skills[?repository=&ref=&refresh=]` | `list_skills` | no |
| Health | `GET /healthz`, `GET /readyz` | — | no |

Errors are `{"error":{"code":"not_found|invalid_request|conflict|forbidden|unsupported|backend_error","message":"…"}}`;
`conflict` (409) covers "exists already", "GitOps-owned", "suspended" and
"bare AgentTemplate" — the cases `force` overrides where documented.

## What an agent is made of

`create_agent` takes the technical **name** (a DNS-1123 label the caller
chose and confirmed — the service never derives one from a display name, per
the creating-agents PRD), the **modelConfig** (must exist in the namespace;
the error lists the valid ones), the **toolset** (required, see below), and
optionally `displayName`, `description`, `systemMessage`, `iconUrl`,
`skills` (see below), `labels`, `annotations`, `namespace`. It emits only what
was set so the chart's defaults apply to everything else — the portal's rule —
plus the platform's own two values, the Harness (`agent.harness`) and, when
configured, the muster URL (`muster.url`), and composes:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata: {name: agent, namespace: kagent}
spec: {interval: 30m, url: oci://gsoci.azurecr.io/charts/giantswarm/agent, ref: {semver: 1.x}}
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: sre, namespace: kagent}
spec:
  interval: 10m
  chartRef: {kind: OCIRepository, name: agent, namespace: kagent}
  values:
    agent: {name: sre, displayName: SRE Assistant, systemMessage: …, harness: kagent}
    modelConfig: {name: default-model-config}
    skills:
      - {name: runbooks, path: runbooks, git: {url: https://github.com/giantswarm/agent-skills, commit: 0123456789abcdef0123456789abcdef01234567}}
      - {name: kubectl, oci: ghcr.io/giantswarm/skills/kubectl@sha256:5b0b…1270}
    toolset: [preset:read-only, workflow:incident-triage]
    muster: {url: http://muster.agent-platform.svc.cluster.local:8090/mcp}
```

The chart renders the `AgentTemplate` (`spec.description`, `spec.systemPrompt`,
`spec.modelConfig`, `spec.skills[]`, the annotations `ui.giantswarm.io/display-name`
and `ui.giantswarm.io/icon-url`, the admission label
`agent-platform.giantswarm.io/harness: <agent.harness>`) and the agent's own
`RemoteMCPServer` (named after the agent, pointing at muster, carrying the
toolset header). ModelConfigs, their Secrets and the platform Harness are
platform-admin owned: agent-manager only reads them.

There is no `runtime` argument: on kagent API v2 the platform Harness (the Go
ADK) is the runtime of every agent. A request still carrying `runtime`, or the
0.x `skills.gitAuthSecretName`, is refused with the reason.

## Skills are pinned

kagent API v2 reads skills from immutable sources only: a git repository at a
full commit id, or an OCI image by digest. `skills` is a list of

- `{name, path, git: {url, ref | commit}}` — a git skill. `commit` (40 or 64
  hex characters; `list_skills` reports it) is the pin. `ref` — a branch or a
  tag — is resolved to its head commit at write time through the GitHub API
  (`--skills-github-api`, `GITHUB_TOKEN`); neither means the head of the
  repository's default branch. Give one, not both.
- `{name, oci: <registry>/<repository>:<tag> | @sha256:<digest>}` — an OCI
  skill; a tag is resolved to its digest against the registry.

What is written is always the pin, and `get_agent` reports it. A reference
that cannot be resolved — a private repository without a token, an unknown
tag — is a `400` naming the repository or image; nothing is written.
`update_agent` with `refreshSkills: true` re-resolves every git skill of the
agent to the head of its repository's default branch (a skill passed in
`skills` with a `ref` in the same call goes to that ref's head) and changes
nothing else on the release — the way to pick up new skill commits. `name`
defaults to the last path segment (else the repository or image name) and
must be unique per agent; `path` is relative without `..`.

## The toolset

Every agent declares a **toolset**: the list of selectors that bounds which of
the gateway's tools its meta-tools can see and call. It is the agent chart's
top-level `toolset` value, which the chart renders as the `X-Muster-Toolset`
header on the agent's own `RemoteMCPServer`; muster resolves it per request
and per caller. agent-manager validates the inline grammar and composes the
list exactly as given — it never resolves a toolset, and it never writes
`muster.tools` (that key narrows the MCP binding, not the toolset; the former
`toolNames` argument narrowed nothing and a caller still passing it is told
so).

- Selectors: `preset:<name>`, `server:<name>`, `workflow:<name>`,
  `tool:<name>` — exact, case-sensitive names; at most 32 inline (define a
  preset for more). `toolset:<name>` is reserved; `label:` selectors exist
  inside presets only.
- Presets every installation has: `read-only`, `none`, `infrastructure`,
  `agent-platform`, `full`. `create_agent` refuses a request without a
  toolset and names them: `preset:none` for a chat-only agent without tools,
  `preset:full` for the deliberate choice of every tool the gateway exposes.
  An empty list is refused too, so "no tools" is never confused with implicit
  full access.
- `update_agent` replaces the whole list — the edit path, and the way agents
  that predate toolsets get one. `get_agent` / `list_agents` report the
  declared `toolset`, or `implicitFullAccess: true` for a release without one
  (a bare template's toolset is read from its RemoteMCPServer's header).

It is composition, not authorization: the invoking human's identity and the
backends' own authorization remain the boundary.

## Status

`get_agent_status` folds three sources into `ready | progressing | failed |
unknown` and one sentence:

- the AgentTemplate's `status.harnesses[]` entry for the platform Harness
  (`--harness-name`, default `kagent`): `ready` when `Ready` is True and
  `desiredRevision` equals `latestSuccessfulRevision`; `progressing` while a
  revision compiles (`desiredRevision` ahead, or `Ready` False with reason
  `ActorTemplatePending`); `failed` with the condition's message when
  `Accepted`, `ResolvedRefs` or `Compatible` is False, when `Ready` is False
  for another reason, or when no Harness admits the template — the answer then
  lists the namespace's Harnesses and what their selectors admit. The
  Harness's `warnings` ride along.
- the owning HelmRelease's conditions and recent history (a failed render is
  `failed`; not yet reconciled is `progressing`);
- the namespace's recent Warning events on the agent's objects.

## Ownership and the meta agent's rule

`list_agents` says how each agent is managed:

- `helmrelease` — a HelmRelease agent-manager (or the portal, or a hand-applied
  manifest) owns: writable here.
- `gitops` — the HelmRelease carries `kustomize.toolkit.fluxcd.io/name`: its
  desired state lives in git and a live write would be undone on the next
  reconciliation. `update_agent` and `delete_agent` refuse it unless `force`.
  A meta agent opens a pull request in the GitOps repository instead. The
  release may live in another namespace (the fleet's `sre-agent` releases sit
  in `flux-giantswarm` with `targetNamespace: kagent`); the template's
  provenance labels lead to it.
- `none` — a bare AgentTemplate with no HelmRelease behind it: nothing to
  write to; `delete_agent` removes it only with `force`.

A suspended HelmRelease is refused the same way: Flux drops its finalizer
without uninstalling, so deleting it would leave the rendered objects behind
(with `force` the AgentTemplate and the agent's RemoteMCPServer are deleted
too).

## Validation

Every create and update is validated against the `agent` chart's
`values.schema.json` before it is applied. The schema comes from the chart
registry (`agentChart.ociUrl`, the newest version in `agentChart.semver` —
`1.x`, the same resolution Flux's OCIRepository performs, so a pre-release
build never counts) and is re-read every `agentChart.refresh`; when the
registry cannot be reached the copy compiled into the binary (the chart 1.x
contract) validates and `get_info` reports `chart.schemaSource: embedded` with
the error. The schema refuses every 0.x key the 1.x contract removed
(`agent.runtime`, `replicas`, `resources`, `nodeSelector`, `tolerations`,
`muster.serverRef`, `muster.allowedHeaders`, `muster.stsWellKnownUri`,
`skills.gitAuthSecretRef`; `muster.toolNames` became `muster.tools`), and
agent-manager never composes one. `validate_agent` returns the composed
manifests — skills pinned — and every violation without writing.

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
AgentTemplate, RemoteMCPServer, Harness, ModelConfig and event reads — through
per-caller clients (`internal/kube.CallerProvider`, built from
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
OCIRepositories read/write; AgentTemplates, RemoteMCPServers, Harnesses,
ModelConfigs and events read; AgentTemplates and RemoteMCPServers delete for
the forced cases) — only for a server nothing but a trusted proxy (the
agentgateway JWT policy, muster) can reach.

## Running

```sh
agent-manager serve \
  --kubeconfig ~/.kube/config --kube-context kind-agentlab \
  --kagent-namespace kagent --harness-name kagent \
  --agent-chart-oci-url oci://gsoci.azurecr.io/charts/giantswarm/agent \
  --skills-repositories https://github.com/giantswarm/agent-skills
```

Every flag has an environment variable (`agent-manager serve --help`).
Kubernetes access is required; the kagent.dev API version is discovered from
the server on `agenttemplates` (`--kagent-api-version auto`, fallback
`v1alpha3`), the Flux API versions on their resources. `--muster-url`
(`AGENT_MUSTER_URL`) composes the platform's muster MCP URL into every agent as
`muster.url`; unset, nothing is composed and the chart default applies.

## Migrating an installation: `agent-manager migrate`

An installation that moves from the 0.10 platform to kagent API v2 has agents
as Generic chart 0.x releases rendering `kagent.dev/v1alpha2` `Agent` objects.
Chart 1.x refuses their values, upstream ships no migration, and the old CRD
and its objects survive the kagent upgrade untouched (`helm.sh/resource-policy:
keep`). `agent-manager migrate` is the expand–contract migration for that: the
connectivity chart of the [`agent-platform`](https://github.com/giantswarm/agent-platform)
meta chart runs it once per installation as a Job under the platform's Flux
identity, and it runs by hand with a kubeconfig the same way. Every phase is
idempotent and gated on the previous one; re-running is always safe, and a
second run on a migrated installation changes nothing and says so.

```sh
agent-manager migrate --dry-run \
  --kubeconfig ~/.kube/config --kagent-namespace kagent --harness-name kagent \
  --agent-chart-oci-url oci://gsoci.azurecr.io/charts/giantswarm/agent --agent-chart-semver 1.x \
  --gitops-namespaces flux-giantswarm
GITHUB_TOKEN=… agent-manager migrate --kubeconfig ~/.kube/config     # writes
```

It works on every Generic-chart release that renders into a managed namespace
(`--kagent-namespace`, `--managed-namespaces` — the same flags and variables
`serve` takes): the HelmReleases of the namespace whose `chartRef` is an
`OCIRepository` of the agent chart (`--agent-chart-oci-url`), plus the releases
elsewhere that the rendered objects' Flux provenance labels
(`helm.toolkit.fluxcd.io/name`, `helm.toolkit.fluxcd.io/namespace`) point at —
the fleet's GitOps-owned `sre-agent` releases live in `flux-giantswarm` with
`targetNamespace: kagent` — and any Generic-chart release in
`--gitops-namespaces` with a `targetNamespace` in the managed set. Releases
outside the managed namespaces, and releases applied by a Flux Kustomization
(`kustomize.toolkit.fluxcd.io/name`), are read and never written.

### The phases

1. **Expand.** For every release on the 0.x values: the removed keys are
   dropped (`agent.runtime`, `replicas`, `resources`, `nodeSelector`,
   `tolerations`, the whole `muster.serverRef`, `muster.allowedHeaders`,
   `muster.stsWellKnownUri`, `skills.gitAuthSecretRef` — the composer's own
   list), `muster.toolNames` becomes `muster.tools`, the 0.x `skills` object
   (`refs[]`, `gitRefs[]`) becomes the 1.x list with every git ref resolved to
   that ref's head commit and every tagged image to its digest (the same path
   `create_agent` pins with), `agent.harness` is set when `--harness-name` is
   not the chart default, and everything else — `agent.*`, `modelConfig`,
   `toolset`, `muster.enabled`, `extraTools`, `extraAgentSpec`, `labels`,
   `annotations` — is kept (`agent.iconUrl` keeps its name; chart 1.x renders
   it as `ui.giantswarm.io/icon-url`). The result is validated against the
   chart 1.x `values.schema.json` **before** anything is written; a release
   whose rewrite fails validation, or whose skill reference cannot be resolved
   (a private repository without a token), is left untouched and reported. A
   writable release is updated in place; a GitOps-owned or external release
   gets its rewrite as a unified diff of the manifest in the report — values,
   the `driftDetection.ignore` paths under `/spec/declarative` (v1alpha2
   fields), and its OCIRepository's range — for the pull request in the owning
   repository. Last, the namespace's agent-chart `OCIRepository` moves to the
   target range (`--agent-chart-semver`, `1.x`), only when every release it
   serves is on 1.x values and the registry has a version in that range; a
   pending or failed release leaves the range untouched. Nothing is written
   while the registry has no 1.x chart (1.x values under a 0.x chart would
   fail every render).
2. **Wait.** The contract runs only when every release rendering into the
   namespace is deployed from a chart in the target range, every
   `AgentTemplate` of the namespace is Ready on the platform Harness (the
   `get_agent_status` verdict, `--harness-name`) and no v1alpha2 `Agent` is
   still rendered by a release Flux has yet to upgrade. Until then the report
   names what is pending and the command exits 0.
3. **Contract.** The leftover `kagent.dev/v1alpha2` `Agent` objects of the
   managed namespaces are deleted (the bundled example agents of the kagent
   0.10 chart among them — an `Agent` no Generic-chart release renders is not
   migratable and is listed for this phase), then the CRDs `agents`,
   `sandboxagents`, `agentharnesses`, `memories` and `toolservers` of
   `kagent.dev`, once every managed namespace has passed the gate and no
   v1alpha2 `Agent` exists outside the managed namespaces.

### The report

Every run writes a ConfigMap **`agent-manager-migrate-report`**
(`--report-configmap`) in each managed namespace and prints the same to
stdout; `--dry-run` prints and writes nothing, not even the report. Keys:

| Key | Content |
|---|---|
| `phase` | `expand` (a release still on 0.x values, a diff not merged, a source not on the range), `wait` (expand done, Flux/kagent catching up), `contract` (the gate passed; leftovers deleted this run), `complete` (nothing of the 0.x world left) |
| `summary` | one sentence |
| `report.yaml` | the structured report below |

```yaml
namespace: kagent
phase: expand
summary: "expand phase: 2 release(s) rewritten, 0 unchanged, 1 with a diff for the owning repository, 0 pending, 0 failed; 1 item(s) gate the next phase"
run: {at: 2026-09-11T02:00:00Z, dryRun: false, version: 1.1.0, harness: kagent,
      chart: {ociUrl: oci://gsoci.azurecr.io/charts/giantswarm/agent, targetSemver: 1.x, latestVersion: 1.0.0, schemaVersion: 1.0.0, schemaSource: registry}}
changed: true                      # this run wrote something in the namespace
releases:
  - name: sre
    namespace: kagent
    ownership: helmrelease         # helmrelease | gitops | external
    action: rewritten              # rewritten | unchanged | diff | pending | failed
    chartVersion: 0.6.1            # deployed chart version
    changes:
      removed: [agent.runtime, replicas, resources, skills.gitAuthSecretRef]
      renamed: ["muster.toolNames -> muster.tools"]
      set: ["agent.harness=kagent"]                         # only when not the chart default
      skills:
        - {name: runbooks, source: "https://github.com/giantswarm/agent-skills@main path=runbooks", pinned: 0123456789abcdef0123456789abcdef01234567}
      driftIgnoreRemoved: [/spec/declarative/deployment/podSecurityContext]
    template: {name: sre, exists: true, verdict: ready, summary: "…"}   # wait phase
  - name: sre-agent
    namespace: flux-giantswarm
    targetNamespace: kagent
    ownership: external
    action: diff
    reason: "never written by this command: …"
    diff: |                        # unified diff of the manifest (runtime metadata and Flux labels stripped)
      --- a/helmrelease-flux-giantswarm-sre-agent.yaml
      +++ b/helmrelease-flux-giantswarm-sre-agent.yaml
      …
sources:                           # the agent-chart OCIRepositories
  - {name: agent, namespace: kagent, ownership: helmrelease, action: moved, from: ">=0.2.1 <1.0.0", to: 1.x}      # moved | unchanged | not-moved | diff
agents:                            # kagent.dev/v1alpha2 Agent objects found
  - {name: k8s-agent, namespace: kagent, owner: agent-platform/kagent, action: not-migratable, reason: "rendered by HelmRelease agent-platform/kagent, which is not a Generic-chart release …"}
  - {name: sre, namespace: kagent, owner: kagent/sre, action: awaiting-upgrade}      # awaiting-upgrade | not-migratable | deleted
pending:                           # what gates the next phase
  - "release flux-giantswarm/sre-agent: its rewrite (the diff in this report) is not applied in the owning repository yet"
contract:                          # once the gate passed
  agentsDeleted: [k8s-agent]
  crds:
    - {name: agents.kagent.dev, action: deleted}          # deleted | absent | kept
```

### Exit codes and permissions

The exit code is `0` whenever the run did what the cluster's state admits —
pending or failed releases included, they are in the report — and non-zero
only for what needs an operator: no cluster access, kagent API v2 not served
(the command refuses to touch a 0.10 installation), a read or write the API
server refused, the report not writable.

The Job's identity (the connectivity chart binds the platform's Flux
ServiceAccount) needs, in the managed namespaces: `helmreleases` and
`ocirepositories` get/list/update/patch, `agenttemplates`, `harnesses` and
`remotemcpservers` get/list, `agents` (`kagent.dev/v1alpha2`) list/delete,
`configmaps` get/create/update, `events` list (diagnostics; optional); in the
namespaces the provenance labels and `--gitops-namespaces` name:
`helmreleases` and `ocirepositories` get/list; cluster-wide:
`customresourcedefinitions` get/delete for the five names above, and — best
effort, the run goes on without it — `agents` list to refuse the CRD deletion
while objects exist outside the managed namespaces. Skill refs are resolved
through the GitHub API (`--skills-github-api`, `GITHUB_TOKEN`) as `list_skills`
does; without a token public repositories resolve and a private ref is
reported as pending. The Job also needs egress to the GitHub API and the chart
registry.

## Helm chart

`helm/agent-manager` — see its [README](helm/agent-manager/README.md). Keys
the [`giantswarm/agent-platform`](https://github.com/giantswarm/agent-platform)
meta chart sets for its `agent-manager` component: `kagent.namespace`,
`harness.name`, `agentChart.*`, `muster.url`, `mcp.enabled`, `oauth.*`,
`muster.mcpServer.*`, `skills.repositories`, and
`flux.helmReleaseServiceAccount` (derived from `kagent.fluxServiceAccountName`).
Optional, off by default: `muster.mcpServer.enabled` (renders an
`mcpservers.muster.giantswarm.io` CR), `httpRoute.enabled`,
`networkPolicy.enabled`.

## Development

See [docs/development.md](docs/development.md).
