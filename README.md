# agent-manager

[![CircleCI](https://dl.circleci.com/status-badge/img/gh/giantswarm/agent-manager/tree/main.svg?style=shield)](https://dl.circleci.com/status-badge/redirect/gh/giantswarm/agent-manager/tree/main)

Agent lifecycle service for the Giant Swarm Agent Platform: the write surface
for **agents themselves**, the sibling of [model-manager](https://github.com/giantswarm/model-manager)
(models) next to muster's own management tools (MCP servers, workflows).

On the platform an agent is a Flux `HelmRelease` of the Generic
[`agent` chart](https://github.com/giantswarm/agent) (2.x). One release
renders one `api.kagent.dev/v1alpha3` `Agent` (the template inline under
`spec.template`, the Harness by name in `spec.harnessRef`) plus the agent's
own muster `RemoteMCPServer`, from the shared per-namespace `OCIRepository` of
that chart, tracking it at `2.x`. That is exactly what the portal's create
flow composes; agent-manager composes the same two objects from a small,
curated argument set, pins every skill to an immutable source, validates the
values against the chart's `values.schema.json` **before** anything is applied,
and reads an agent back from its Agent object, its RemoteMCPServer and its
owning HelmRelease (Flux provenance labels). Readiness is the Agent's own
`status.conditions`; there is no per-agent Deployment or pod, agents run as
Substrate actors.

The same operations are exposed twice from one process:

- **REST/JSON** under `/api/v1` for the portal backend — contract in
  [`api/openapi.yaml`](api/openapi.yaml), also served at `/api/v1/openapi.yaml`.
- **MCP** (streamable HTTP, `/mcp`) for muster — tools `get_info`,
  `list_agents`, `get_agent`, `create_agent`, `update_agent`, `delete_agent`,
  `get_agent_status`, `validate_agent`, `list_model_configs`, `list_skills`,
  `list_sessions`, `get_session`, `start_session`
  (through muster: `x_agent-manager_<tool>`). Every tool description says
  whether it writes and what it writes, and every tool sets all four
  annotations: read-only, destructive, idempotent and open-world (true for the
  tools that resolve a caller-named GitHub repository or OCI reference).

Part of the [Agent Control Plane epic](https://github.com/giantswarm/giantswarm/issues/36796)
("create and manage versioned agents"): the reconciler half is the `agent`
chart plus Flux helm-controller and the kagent controller, this is the
MCP-server-writer half.

## API at a glance

| Operation | REST | MCP tool | Writes |
|---|---|---|---|
| Version, chart (OCI URL, `2.x` range, latest version, schema in use), managed namespaces, capabilities, API versions, platform Harness, muster URL, identity | `GET /api/v1/info` | `get_info` | no |
| Agents of a namespace (Agent objects + HelmReleases of the chart not rendered yet), one summary each: display name, description, model config, Harness, toolset, readiness, management mode, and a suspended, deleting or failing release; `details` reports each in full | `GET /api/v1/agents?namespace=[&details=true]` | `list_agents` | no |
| One agent with its HelmRelease values, Harness, pinned skills and plugins, toolset and the Agent's status | `GET /api/v1/agents/{ns}/{name}` | `get_agent` | no |
| Create: OCIRepository (when missing) + HelmRelease, after skill pinning, schema, ModelConfig and Harness validation | `POST /api/v1/agents` | `create_agent` | HelmRelease, OCIRepository |
| Update: merge into the HelmRelease values (`refreshSkills` re-pins git skills), validate, update | `PATCH /api/v1/agents/{ns}/{name}[?force=true]` | `update_agent` | HelmRelease |
| Delete: the HelmRelease; the OCIRepository only when nothing else references it | `DELETE /api/v1/agents/{ns}/{name}[?force=true&mode=&dryRun=]` | `delete_agent` | HelmRelease, OCIRepository, (bare Agent with force) |
| Status verdict: the Agent's conditions, HelmRelease conditions/history, Warning events | `GET /api/v1/agents/{ns}/{name}/status` | `get_agent_status` | no |
| Dry run of create/update: pinned skills, composed manifests + every violation | `POST /api/v1/agents/validate` | `validate_agent` | no |
| kagent ModelConfigs of a namespace | `GET /api/v1/modelconfigs?namespace=` | `list_model_configs` | no |
| Skills (`SKILL.md`) of the configured GitHub repositories, grouped by repository with its head commit | `GET /api/v1/skills[?repository=&ref=&refresh=]` | `list_skills` | no |
| Sessions of an agent, as the caller (`SessionService/ListSessions`; `allCreators` when the controller grants it) | `GET /api/v1/agents/{ns}/{name}/sessions[?allCreators=true]` | `list_sessions` | no |
| Start a session: create it, send the first message, answer once the turn is accepted | `POST /api/v1/agents/{ns}/{name}/sessions` | `start_session` | a kagent Session |
| One session and its conversation (the A2A tasks of its context, oldest first) | `GET /api/v1/sessions/{id}` | `get_session` | no |
| Health | `GET /healthz`, `GET /readyz` | — | no |

The three writes take `dryRun` and `mode` (`apply` | `commit`, see
[Write modes](#write-modes-apply-and-commit)); in commit mode the "Writes"
column is files of a pull request, nothing live.

Errors are `{"error":{"code":"not_found|invalid_request|conflict|gitops_owned|forbidden|unauthenticated|auth_required|unsupported|backend_error","message":"…"}}`;
`conflict` (409) covers "exists already", "suspended" and "bare
Agent", the cases `force` overrides where documented;
`gitops_owned` (409) is an apply-mode write to a release applied from git,
`auth_required` (401) a commit without the caller's GitHub authorization.

## What an agent is made of

`create_agent` takes the technical **name** (a DNS-1123 label the caller
chose and confirmed — the service never derives one from a display name, per
the creating-agents PRD), the **modelConfig** (must exist in the namespace;
the error lists the valid ones), the **toolset** (required, see below), and
optionally `harness`, `displayName`, `description`, `systemMessage`,
`iconUrl`, `skills` and `plugins` (see below), `egress` (the HTTP(S) origins
the agent may reach besides what its revision compiles, such as
`https://github.com:443` or `https://*.githubusercontent.com`; chart
`agent.egress`, the Agent's `spec.egress`), `labels`, `annotations`,
`namespace`. It emits only what was set so the chart's defaults apply to
everything else (the portal's rule) plus the platform's own two values, the
Harness (`agent.harness`) and, when configured, the muster URL (`muster.url`),
and composes:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata: {name: agent, namespace: kagent}
spec: {interval: 30m, url: oci://gsoci.azurecr.io/charts/giantswarm/agent, ref: {semver: 2.x}}
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: coder, namespace: kagent}
spec:
  interval: 10m
  chartRef: {kind: OCIRepository, name: agent, namespace: kagent}
  values:
    agent:
      name: coder
      displayName: Coder
      systemMessage: …
      harness: claude
      egress: [https://github.com:443, "https://*.githubusercontent.com"]
    modelConfig: {name: default-model-config}
    skills:
      - {name: runbooks, path: runbooks, git: {url: https://github.com/giantswarm/agent-skills, commit: 0123456789abcdef0123456789abcdef01234567}}
      - {name: kubectl, oci: ghcr.io/giantswarm/skills/kubectl@sha256:5b0b…1270}
    plugins:
      - {git: {url: https://github.com/giantswarm/agent-skills, commit: fedcba9876543210fedcba9876543210fedcba98}, path: bundles/sre, skills: [triage, postmortem]}
    toolset: [preset:read-only, workflow:incident-triage]
    muster: {url: http://muster.agent-platform.svc.cluster.local:8090/mcp}
```

The chart renders the `Agent` (`spec.harnessRef.name: <agent.harness>`,
`spec.egress: <agent.egress>`; under
`spec.template` the `description`, `systemPrompt`, `modelConfig`,
`skills[]`, `plugins[]` and the muster binding; the annotations
`ui.giantswarm.io/display-name` and `ui.giantswarm.io/icon-url`) and the
agent's own `RemoteMCPServer` (named after the agent, pointing at muster,
carrying the toolset header). No admission label is involved: the Agent
selects its Harness by name. ModelConfigs, their Secrets and the Harnesses are
platform-admin owned: agent-manager only reads them.

## The Harness and plugins

An agent runs on the platform Harness (`kagent`, the Go ADK) unless
`create_agent` names another Harness of the namespace in `harness`; the name
must be a Harness that exists there (the refusal lists the ones that do) and
it is fixed at create. `claude` is a Harness whose runtime is Claude Code: the
agent then runs as a Claude Code session and its skills and plugins are
Claude Code skills and plugins. Per-turn bounds (a budget, a turn cap) are the
Harness's (`Harness.spec.claude.limits`, platform-admin owned), not the
agent's.

- `plugins` is a list of Agent Plugins bundles, `{git: {url, commit}, path,
  skills}` or `{oci: <ref>@sha256:<digest>, path, skills}` (chart `plugins[]`,
  `spec.template.plugins[]`). A plugin is written as given and must come
  pinned: a full commit id or a digest, never a branch, tag or image tag.
  `skills` names the bundle's skills to enable (at least one, unique); nothing
  else of the bundle reaches the agent. A private git plugin reads with the
  same credential as a private git skill.

There is no `runtime` argument: a request still carrying `runtime` is refused
with the reason, and the 0.x `skills.gitAuthSecretName` is refused naming its
replacement, the top-level `gitAuthSecretName`.

## Skills are pinned

kagent API v2 reads skills from immutable sources only: a git repository at a
full commit id, or an OCI image by digest. `skills` is a list of

- `{name, path, git: {url, ref | commit}}` — a git skill. `commit` (40 or 64
  hex characters; `list_skills` reports it per repository) is the pin. `ref` — a branch or a
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

## Private skills

Two credentials are involved, neither of them a person's token:

- **Discovery and pinning** (`list_skills`, resolving a `ref`) read GitHub
  with the read-only skills GitHub App's short-lived installation tokens
  (`--skills-github-app-id`, `--skills-github-app-installation-id`,
  `--skills-github-app-private-key-file`; chart `skills.github.app.secretName`,
  a Secret with `app-id`, `installation-id` and `private-key`), at the App's
  rate limit. A static `GITHUB_TOKEN` (chart `skills.github.tokenSecret`) is
  the alternative; the two are mutually exclusive.
- **Boot**: the platform fetches an agent's git skills and git plugins with
  the Secret named by the chart value `skillsGitAuthSecretRef.name` (key
  `token`, in the agent's namespace). `create_agent`, `update_agent` and
  `validate_agent` take `gitAuthSecretName`; omitted, every agent with a git
  skill or plugin gets the
  installation's (`--skills-git-auth-secret-name`, chart
  `skills.gitAuthSecretName`, reported by `get_info` as
  `skillsGitAuthSecretName`). An update keeps the credential in step with the
  skills and plugins (an agent written before the credential existed gets it
  on its next update) and drops it when no git source is left.
  With `--skills-git-auth-mint` (chart `skills.mintGitAuthSecret`) that
  Secret carries the skills GitHub App's installation token too: the chart
  renders `<fullname>-skills-token` without data in every managed namespace
  with a Role that may only get and update it, and agent-manager writes the
  token (the Basic form of `x-access-token:<token>`) and replaces it well
  before its one-hour expiry. The egress gateway reads the Secret on every
  fetch, so a replaced token needs no restart. `get_info` reports the last
  refresh and a failed one as `skillsGitAuthMint`.

A private repository is only for the people who can read it themselves:
`list_skills` shows it only to a caller whose GitHub login has read access
(`GET /repos/{owner}/{repo}/collaborators/{login}/permission`), and
`create_agent`, `update_agent` and `validate_agent` refuse a skill from it
otherwise (`403 forbidden`, naming the repository). The login is the one the
GitHub App pin verifies (see "Write modes"); a caller without one can use
public repositories only.

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
  (a bare Agent's toolset is read from its RemoteMCPServer's header).

It is composition, not authorization: the invoking human's identity and the
backends' own authorization remain the boundary.

## Agents on a workload cluster

An agent can run on a workload cluster of the installation, managed from the
installation: `create_agent` (and `POST /api/v1/agents`) take `organization`
and `cluster`, and every read and write tool takes the same pair (REST: the
query parameters `organization` and `cluster`). `namespace` is then the
namespace on the workload cluster. `get_info` reports `capabilities.targetCluster`.

- **The HelmRelease stays on the installation**, in the organization
  namespace `org-<organization>`, named `<cluster>-<agent>` and labelled
  `agent-platform.giantswarm.io/cluster: <cluster>`, beside the shared
  OCIRepository of the agent chart there. It carries
  `spec.kubeConfig.secretRef` (`<cluster>-kubeconfig`, key `value`),
  `releaseName: <agent>` and `targetNamespace`/`storageNamespace: <namespace>`,
  so the installation's helm-controller installs the chart on the workload
  cluster. It carries no `serviceAccountName`: Flux would impersonate it on
  the workload cluster, where it does not exist. A helm-controller with the
  multi-tenancy lockdown (`--default-service-account`) impersonates that
  default account of `org-<organization>` on the workload cluster instead: it
  must exist there with the rights to install the agent chart.
- **The kubeconfig Secret contract** is Cluster API's: the Secret
  `<cluster>-kubeconfig` in `org-<organization>` with the kubeconfig under the
  key `value`. agent-manager reads it as the caller and keeps only its server
  address and CA; the credentials in it are never used when the server acts
  as the caller (`oauth.downstream`). Running as the ServiceAccount, the
  kubeconfig is used as it is.
- **The agent, its ModelConfig and its Harness are on the workload cluster**:
  the Agent, its RemoteMCPServer, the ModelConfigs `modelConfig` must
  name, the Harnesses and the agent's events are read there. The cluster runs
  the agent runtime slice of the `agent-platform` chart (kagent and its CRDs,
  Substrate, agentgateway, the platform Harness).
- **Identity precondition**: the reads on the workload cluster present the
  caller's token, so the workload cluster's apiserver must trust the
  installation's identity provider (OIDC issuer and client id set when the
  cluster is created) and grant the caller read access to the kagent
  resources and events of the namespace. Otherwise the call answers 401 and
  names that precondition.
- **Egress**: the agent reaches muster at `--target-muster-url`
  (`AGENT_TARGET_MUSTER_URL`, chart `muster.targetUrl`), the installation's
  public muster endpoint; unset, a target is refused. The installation's
  skills credential exists on the installation only and is not composed: an
  agent on a workload cluster with private git skills names its own Secret
  there (`gitAuthSecretName`).

Without `organization` and `cluster` nothing changes: the objects are the
ones composed for the installation's own cluster.

## Status

`get_agent_status` folds three sources into `ready | progressing | failed |
unknown` and one sentence:

- the Agent's `status.conditions`, reported by the Harness it references
  (`spec.harnessRef.name`): `ready` when `Ready` is True and
  `desiredRevision` equals `latestSuccessfulRevision`; `progressing` while a
  revision compiles (`desiredRevision` ahead, or `Ready` False with reason
  `ActorTemplatePending`); `failed` with the condition's message when
  `Accepted`, `ResolvedRefs` or `Compatible` is False (a Harness that does not
  exist is `ResolvedRefs` False) or when `Ready` is False for another reason.
  The Agent's `status.warnings` ride along.
- the owning HelmRelease's conditions and recent history (a failed render is
  `failed`; not yet reconciled is `progressing`);
- the namespace's recent Warning events on the agent's objects.

## Ownership and the meta agent's rule

`list_agents` says how each agent is managed:

- `helmrelease` — a HelmRelease agent-manager (or the portal, or a hand-applied
  manifest) owns: writable here.
- `gitops` — the HelmRelease carries `kustomize.toolkit.fluxcd.io/name`: its
  desired state lives in git and a live write would be undone on the next
  reconciliation. `update_agent` and `delete_agent` refuse it in mode `apply`
  with `gitops_owned`, `force` or not; mode `commit` changes it with a pull
  request in the repository that owns it. The
  release may live in another namespace (the fleet's `sre-agent` releases sit
  in `flux-giantswarm` with `targetNamespace: kagent`); the Agent's
  provenance labels lead to it.
- `none` — a bare Agent with no HelmRelease behind it: nothing to write to;
  `delete_agent` removes it only with `force`.

A suspended HelmRelease is refused the same way: Flux drops its finalizer
without uninstalling, so deleting it would leave the rendered objects behind
(with `force` the Agent and the agent's RemoteMCPServer are deleted too).

## Validation

Every create and update is validated against the `agent` chart's
`values.schema.json` before it is applied. The schema comes from the chart
registry (`agentChart.ociUrl`, the newest version in `agentChart.semver`,
`2.x`, the same resolution Flux's OCIRepository performs, so a pre-release
build never counts) and is re-read every `agentChart.refresh`; when the
registry cannot be reached the copy compiled into the binary (the chart 2.x
contract) validates and `get_info` reports `chart.schemaSource: embedded` with
the error. The schema refuses every 0.x key the contract removed
(`agent.runtime`, `replicas`, `resources`, `nodeSelector`, `tolerations`,
`muster.serverRef`, `muster.allowedHeaders`, `muster.stsWellKnownUri`,
`skills.gitAuthSecretRef`; `muster.toolNames` became `muster.tools`), and
agent-manager never composes one. `validate_agent` returns the composed
manifests — skills pinned — and every violation without writing, a name that
is already taken among them. A check it cannot make, such as the name check
for a caller who may not read the namespace's HelmReleases, is listed in
`notes` instead of failing the dry run.

## Write modes: apply and commit

Every write tool (`create_agent`, `update_agent`, `delete_agent`) renders the
standard manifests a kubectl or GitOps user would apply, returns them on
`dryRun: true` and writes nothing then, and takes `mode`:

- `apply` (default) writes live as the caller. It never writes a release
  applied from git: that answer is `gitops_owned`, naming `mode: commit` and
  the repository and directory it would write.
- `commit` writes the manifests as files and opens a pull request **as the
  caller**; nothing is written live. The target is derived from Flux: the
  `kustomize.toolkit.fluxcd.io/name|namespace` labels of the release (update,
  delete) or, for a new agent, of the namespace's chart source, one of its
  agent releases or the Namespace → the Kustomization's `spec.path` → its
  GitRepository's URL and branch. Where nothing GitOps-owned lives yet,
  `repository` (owner/name), `branch` and `path` name it. The files follow
  [gitops-commit](https://github.com/giantswarm/gitops-commit)'s `layout`, the
  one cluster-manager and model-manager write: an `agent-manager/` directory
  under that path, one file per object (`<agent>.yaml` for the HelmRelease,
  `agent.yaml` for the chart source when the namespace has none yet), its
  `kustomization.yaml` and the parent's entry. An update rewrites the
  release's file and a delete removes it (and the chart source's when no
  other release uses it); a release defined elsewhere in the repository is
  refused. A Kustomization that does not prune leaves a removed release in
  place, which the answer's `liveSteps` names. The result carries
  `commit: {repository, base, directory, kustomization, prune, branch, files:
  [{path, action, content (dry run)}], pullRequest, number, author}`.

Commit mode needs the GitHub App pin (`github.enabled`, below) and reports
itself in `get_info` as `capabilities.commit`; without it `mode: commit`
answers `unsupported`. A call that carries no GitHub authorization answers
`auth_required`, naming the one-time consent (`core_auth_login
server=agent-manager`).

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
everything a request does (the HelmRelease and OCIRepository writes, the
Agent, RemoteMCPServer, Harness, ModelConfig and event reads) through
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

With `--github-authorization-server` (chart `github.enabled`) the MCP
registration is pinned to agent-manager's own user-to-server GitHub App,
`giantswarm-agent-manager`: the MCPServer's auth names the App as the
authorization server with `forwardIdentity: true`, so muster runs the App's
consent once per person and puts the person's App user token on every MCP call
as the bearer, and the person's IdP ID token in `X-Muster-Id-Token`. The server
verifies the GitHub token with `GET /user` (cached for 15 minutes), keeps it
for commit mode's pull request, and validates the ID token exactly as a
forwarded bearer, so apply mode and every read still act on Kubernetes as the
person. The REST API keeps taking the ID token as its bearer. No GitHub
credential other than the App's OAuth client (read by muster) exists in the
pod or the chart. Operator precondition: the App installed with contents and
pull-request write on every repository a commit may target.

Without `--enable-oauth` the service checks no identity and acts as its
ServiceAccount (the Role per managed namespace: HelmReleases and
OCIRepositories read/write; Agents, RemoteMCPServers, Harnesses,
ModelConfigs and events read; Agents and RemoteMCPServers delete for
the forced cases) — only for a server nothing but a trusted proxy (the
agentgateway JWT policy, muster) can reach.

## Sessions

`list_sessions`, `get_session` and `start_session` let a caller, a meta agent
working through muster for instance, talk to an agent and read what it did. A
conversation is a kagent `Session` of the `Agent` (kagent 1.3, `SessionService`),
held in the controller's database. agent-manager calls the controller's gRPC
API through agentgateway at `--kagent-target` (`AGENT_MANAGER_KAGENT_TARGET`,
chart `kagent.controllerTarget`, default the in-cluster
`grpc://agentgateway.agent-platform.svc.cluster.local:8080`) with the caller's
IdP token as the bearer and no other identity: agentgateway validates the token
on the controller route and sets the identity the controller reads from it. The
calls never run as the ServiceAccount, so the target needs `--enable-oauth`,
and without the caller's token a session operation is `401 unauthenticated`.

- `start_session` is `SessionService/CreateSession` for the Agent (`409
  conflict` while the Agent has no ready revision), then the first message
  over `A2AService/SendStreamingMessage` with the Agent as `tenant`
  (`<namespace>/<name>`) and the session as `message.contextId`. It answers
  once the agent has accepted the turn; the turn keeps running when the stream
  closes, and `get_session` shows it finish. `requestId` makes a retry
  idempotent.
- `get_session` is `SessionService/GetSession` plus `A2AService/ListTasks`
  addressed to the session's Agent and filtered by its context.
- `list_sessions` is `SessionService/ListSessions` for one Agent.

Session and task bodies are kagent's proto3 JSON, passed through: session
`state` is `RUNTIME_STATE_*`, task `status.state` is `TASK_STATE_*`. The
`x-kagent-agent-instance-id` header is the gateway's to the runtime and is
never sent. The stubs are generated from `giantswarm/kagent-upstream`
(`hack/kagent-proto/`, `make generate-kagent`); the A2A stubs are the ones
`github.com/a2aproject/a2a-go/v2` ships, as in kagent. `get_info` reports
`capabilities.sessions`.

## Running

```sh
agent-manager serve \
  --kubeconfig ~/.kube/config --kube-context kind-agentlab \
  --kagent-namespace kagent --harness-name kagent \
  --agent-chart-oci-url oci://gsoci.azurecr.io/charts/giantswarm/agent \
  --skills-repositories https://github.com/giantswarm/agent-skills
```

Every flag has an environment variable (`agent-manager serve --help`).
Kubernetes access is required; the api.kagent.dev API version is discovered
from the server on `agents` (`--kagent-api-version auto`, fallback
`v1alpha3`), the Flux API versions on their resources. `--muster-url`
(`AGENT_MUSTER_URL`) composes the platform's muster MCP URL into every agent as
`muster.url`; unset, nothing is composed and the chart default applies.
`--target-muster-url` (`AGENT_TARGET_MUSTER_URL`) is the one agents on
workload clusters reach (see "Agents on a workload cluster").

### Tracing

With `OTEL_EXPORTER_OTLP_ENDPOINT` set (chart: `observability.otel.endpoint`)
the server exports traces over OTLP, configured by the standard `OTEL_*`
variables (`OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_HEADERS`,
`OTEL_TRACES_SAMPLER`, `OTEL_RESOURCE_ATTRIBUTES`). Every request but the
probes is a server span named after its route (`POST /mcp`, `GET /api/v1/`),
joined to an inbound `traceparent`. Under it an MCP request is an
`mcp.<method>` server span; a tool call's `mcp.tools/call` span carries
`gen_ai.tool.name` and `mcp.tool.name`, with a `tool.<name>` span around the
handler. The chart samples parent-based (`parentbased_traceidratio`, `0.1` of
the traces that start here). Unset, nothing is exported and `traceparent`
still propagates.

## Migrating an installation: `agent-manager migrate`

`agent-manager migrate` moves an installation's agents to the next Generic
chart major. Helm upgrades a release to the new chart, but nothing else moves
existing agents: the namespace's shared `OCIRepository` keeps tracking the old
range, and values the new chart or kagent API refuses are never rewritten.
`create_agent` reuses a namespace's `OCIRepository` and only warns when its
range differs. The connectivity chart of the
[`agent-platform`](https://github.com/giantswarm/agent-platform) meta chart
runs `migrate` once per installation as a Job under the platform's Flux
identity, and it runs by hand with a kubeconfig the same way. Every phase is
idempotent and gated on the previous one; re-running is always safe, and a
second run on a migrated installation changes nothing and says so.

The target range (`--agent-chart-semver`, default `2.x`) picks the path by its
major. A cluster serves one kagent line at a time and a namespace's releases
share one chart source, so the path belongs to the installation, not to a
release. Each release's deployed chart is checked against it: a release on a
chart older than the path's source major is reported as failed and never
rewritten. A range that admits two majors (`>=1.0.0`) is refused.

| Target | Path | Old line → new line | Precondition |
|---|---|---|---|
| `2.x` | 1.x → 2.x | `kagent.dev/v1alpha3` `AgentTemplate` → `api.kagent.dev/v1alpha3` `Agent` | `agents.api.kagent.dev` served (kagent 1.3 line); `--dry-run` runs before it |
| `1.x` | 0.x → 1.x | `kagent.dev/v1alpha2` `Agent` → `kagent.dev/v1alpha3` `AgentTemplate` | `agenttemplates.kagent.dev` served (kagent 1.0 to 1.2 line) |

```sh
agent-manager migrate --dry-run \
  --kubeconfig ~/.kube/config --kagent-namespace kagent --harness-name kagent \
  --agent-chart-oci-url oci://gsoci.azurecr.io/charts/giantswarm/agent --agent-chart-semver 2.x \
  --gitops-namespaces flux-giantswarm
agent-manager migrate --kubeconfig ~/.kube/config     # writes
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

### 1.x → 2.x values

The 2.x values schema adds `agent.egress` and `plugins` and changes nothing
else, so 1.x values pass it. The change is under keys the schema leaves open,
where only the `api.kagent.dev` CRD would refuse them:

- A sub-agent binding `{agent: {name, description, templateRef, isolation}}`
  becomes `{subAgent: {name, description, templateRef}}`, in `extraTools[]`
  and in `extraAgentSpec.tools[]` (the escape hatch replaces the curated
  tools). `isolation: Shared` is dropped, since every sub-agent is Shared on
  `api.kagent.dev`. `isolation: Dedicated` has no 2.x form: the release fails
  and is left as it is.
- `labels["agent-platform.giantswarm.io/harness"]` is dropped. On 1.x it
  chose the Harness (the Harness admitted templates by it, and extra labels
  won over the chart's); on 2.x the Agent names its Harness in
  `spec.harnessRef` from `agent.harness`. A label naming another Harness than
  `agent.harness` becomes `agent.harness`.
- `agent.harness` is set when `--harness-name` is not the chart default and
  the values leave it unset.

A `subAgent.templateRef` resolves to an `api.kagent.dev` `AgentTemplate` in the
agent's namespace. Chart 2.x renders an `Agent` with its template inline and
never an `AgentTemplate`, so a binding to another Generic-chart agent finds
nothing after the move. The rewrite is still written, and the release's
`warnings` name the missing template; the parent `Agent` reports
`ResolvedRefs` False, and the wait phase lists it, until an `AgentTemplate` of
that name exists.

### 0.x → 1.x values

An installation that moves from the 0.10 platform to kagent API v2 has agents
as Generic chart 0.x releases rendering `kagent.dev/v1alpha2` `Agent` objects.
Chart 1.x refuses their values, upstream ships no migration, and the old CRD
and its objects survive the kagent upgrade untouched (`helm.sh/resource-policy:
keep`). The removed keys are dropped (`agent.runtime`, `replicas`,
`resources`, `nodeSelector`, `tolerations`, the whole `muster.serverRef`,
`muster.allowedHeaders`, `muster.stsWellKnownUri`, `skills.gitAuthSecretRef`:
the composer's own list), `muster.toolNames` becomes `muster.tools`, the 0.x
`skills` object (`refs[]`, `gitRefs[]`) becomes the 1.x list with every git ref
resolved to that ref's head commit and every tagged image to its digest (the
same path `create_agent` pins with, through `--skills-github-api` and
`GITHUB_TOKEN`), `agent.harness` is set when `--harness-name` is not the chart
default, the `driftDetection.ignore` paths under `/spec/declarative` (v1alpha2
fields) are dropped from the HelmRelease, and everything else (`agent.*`,
`modelConfig`, `toolset`, `muster.enabled`, `extraTools`, `extraAgentSpec`,
`labels`, `annotations`) is kept. A skill reference that cannot be resolved
(a private repository without a token) leaves the release pending.

### The phases

1. **Expand.** Every release's rewrite is computed and validated against the
   target chart's `values.schema.json` (the registry's, else the embedded
   2.x copy) **before** anything is written, a release that is already on
   target values included. The releases of one chart source are written only
   when every one of them can be: a failed or pending release holds its
   writable siblings (`pending`, `held: …`), since the source cannot move and
   target values under the old chart would fail their render. A writable
   release is updated in place; a GitOps-owned or external release gets its
   rewrite as a unified diff of the manifest in the report, and its
   OCIRepository's range move too, for the pull request in the owning
   repository. Last, the namespace's agent-chart `OCIRepository` moves to the
   target range (`--agent-chart-semver`, and `--agent-chart-semver-filter`
   when given), only when every release it serves is on target values and
   the registry has a version in that range. Nothing is written while the
   registry has no version in the target range.
2. **Wait.** The contract runs only when the cluster serves the target line's
   resource, every release rendering into the namespace is deployed from a
   chart in the target range, every object the target chart renders is Ready
   (2.x: the `Agent`, by the `get_agent_status` verdict; 1.x: the
   `AgentTemplate`'s entry for `--harness-name` in `status.harnesses[]`), and
   no object of the old line is still rendered by a release Flux has yet to
   upgrade. Until then the report names what is pending and the command exits
   0.
3. **Contract.** The old line's leftover agent objects in the managed
   namespaces are deleted: on 2.x the `kagent.dev/v1alpha3` `AgentTemplate`
   objects no release renders any more (Helm removes the rendered ones when a
   release upgrades), on 1.x the `kagent.dev/v1alpha2` `Agent` objects (the
   bundled example agents of the kagent 0.10 chart among them). On 1.x the
   CRDs `agents`, `sandboxagents`, `agentharnesses`, `memories` and
   `toolservers` of `kagent.dev` follow, once every managed namespace has
   passed the gate and no v1alpha2 `Agent` exists outside the managed
   namespaces. On 2.x no CRD is deleted: the `kagent.dev` CRDs are the
   platform's to remove, and other components (model-manager's `ModelConfig`)
   may still use them.

### The report

Every run writes a ConfigMap **`agent-manager-migrate-report`**
(`--report-configmap`) in each managed namespace and prints the same to
stdout; `--dry-run` prints and writes nothing, not even the report. Keys:

| Key | Content |
|---|---|
| `phase` | `expand` (a release still on the old major's values, a diff not merged, a source not on the range), `wait` (expand done, Flux/kagent catching up), `contract` (the gate passed; leftovers deleted this run), `complete` (nothing of the old line left) |
| `summary` | one sentence |
| `report.yaml` | the structured report below |

```yaml
namespace: kagent
phase: expand
summary: "expand phase (1.x -> 2.x): 1 release(s) rewritten, 1 unchanged, 1 with a diff for the owning repository, 0 pending, 0 failed; 1 item(s) gate the next phase"
run: {at: 2026-10-06T02:00:00Z, dryRun: false, version: 2.0.0, harness: kagent, path: "1.x -> 2.x",
      chart: {ociUrl: oci://gsoci.azurecr.io/charts/giantswarm/agent, targetSemver: 2.x, latestVersion: 2.0.0, schemaVersion: 2.0.0, schemaSource: registry}}
changed: true                      # this run wrote something in the namespace
releases:
  - name: lead
    namespace: kagent
    ownership: helmrelease         # helmrelease | gitops | external
    action: rewritten              # rewritten | unchanged | diff | pending | failed
    chartVersion: 1.5.3            # deployed chart version
    changes:
      removed: ["extraTools[1].agent.isolation"]
      renamed: ["extraTools[1].agent -> extraTools[1].subAgent"]
      set: ["agent.harness=claude"]                         # 0.x -> 1.x also: skills, driftIgnoreRemoved
    warnings:                      # what the write does not wait for
      - "sub-agent templateRef \"helper\" names no agenttemplates.api.kagent.dev in namespace kagent; …"
    template: {name: lead, exists: true, verdict: ready, summary: "…"}  # wait phase: the Agent (2.x) or AgentTemplate (1.x)
  - {name: sre, namespace: kagent, ownership: helmrelease, action: unchanged, reason: "already on the 2.x values", chartVersion: 1.5.3}
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
  - {name: agent, namespace: kagent, ownership: helmrelease, action: moved, from: ">=1.5.0 <2.0.0", to: 2.x}      # moved | unchanged | not-moved | diff
agents:                            # the old line's agent objects found
  - {name: by-hand, namespace: kagent, kind: kagent.dev/v1alpha3 AgentTemplate, action: not-migratable, reason: "no Flux provenance labels: …"}
  - {name: sre, namespace: kagent, kind: kagent.dev/v1alpha3 AgentTemplate, owner: kagent/sre, action: awaiting-upgrade}      # awaiting-upgrade | not-migratable | deleted
pending:                           # what gates the next phase
  - "release flux-giantswarm/sre-agent: its rewrite (the diff in this report) is not applied in the owning repository yet"
contract:                          # once the gate passed
  agentsDeleted: [by-hand]
  crds:                            # 0.x -> 1.x only
    - {name: agents.kagent.dev, action: deleted}          # deleted | absent | kept
```

### Exit codes and permissions

The exit code is `0` whenever the run did what the cluster's state admits
(pending or failed releases included, they are in the report) and non-zero
only for what needs an operator: no cluster access, a target range with no
migration path, the target line's kagent API not served (on 2.x a dry run goes
ahead and reports it), a read or write the API server refused, the report not
writable.

The Job's identity (the connectivity chart binds the platform's Flux
ServiceAccount) needs, in the managed namespaces: `helmreleases` and
`ocirepositories` get/list/update/patch, `configmaps` get/create/update,
`events` list (diagnostics; optional), and per path:

- 1.x → 2.x: `agents`, `agenttemplates`, `harnesses` and `remotemcpservers` of
  `api.kagent.dev` get/list; `agenttemplates` of `kagent.dev` list/delete.
- 0.x → 1.x: `agenttemplates`, `harnesses` and `remotemcpservers` of
  `kagent.dev` get/list, `agents` (`kagent.dev/v1alpha2`) list/delete; and
  cluster-wide `customresourcedefinitions` get/delete for the five names
  above and, best effort (the run goes on without it), `agents` list to refuse
  the CRD deletion while objects exist outside the managed namespaces.

In the namespaces the provenance labels and `--gitops-namespaces` name:
`helmreleases` and `ocirepositories` get/list. Skill refs (0.x → 1.x) are
resolved through the GitHub API (`--skills-github-api`, `GITHUB_TOKEN`) as
`list_skills` does; without a token public repositories resolve and a private
ref is reported as pending. The Job also needs egress to the chart registry,
and to the GitHub API on 0.x → 1.x.

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
