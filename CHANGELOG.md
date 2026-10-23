# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- `start_session` with a toolchain set no Session has used yet no longer fails at muster's 30s tool-call timeout while kagent prepares the toolset: the call reports MCP progress every 10s while kagent's `CreateSession` waits, which restarts muster's timeout, and a `CreateSession` answered `Unavailable` or `DeadlineExceeded` is retried with the same request id for up to 3 minutes. Every tool call with a progress token can now report progress ([#253](https://github.com/giantswarm/agent-manager/issues/253)).

## [2.16.0] - 2026-10-23

### Changed

- The egress note moves out of the person's prompt: `create_agent`, `update_agent` and `validate_agent` set the agent chart's `agent.platformPrompt.egressNote` on an agent on a coding Harness instead of appending the note to `agent.systemMessage`, and the chart renders it after the prompt. A note already in `agent.systemMessage` (a line that is its heading and the paragraph after it, in any line endings) is taken out on the next write, and the text after it, which the previous rewrite cut off, stays. A coding agent created without a prompt of its own, whose prompt was the note alone, gets the chart's default prompt before the note. Every write to an agent on a kagent Harness clears the flag and the note, not only a Harness change. The 20000-character cap counts the person's prompt only, a pasted note left out. Where the agent chart that validates the agents has no `agent.platformPrompt` (an installation held below 2.4.0), the values are written without the flag and without the note. The embedded chart is 2.4.0 ([giantswarm#38803](https://github.com/giantswarm/giantswarm/issues/38803)).
- `get_agent` and `list_agents` report a managed agent's `systemMessage` from its release's values, without the egress note, instead of the Agent's rendered `systemPrompt`; without one in the values, the rendered prompt without the note (the chart default). An Agent no release renders reports its `systemPrompt` as is.

## [2.16.0] - 2026-10-23

### Changed

- An agent without `defaultToolchains`, every agent created before they existed included, starts its Sessions with the `github` toolchain at its newest version where the installation publishes toolchains, unchanged and without a person's action, since the Harness images no longer carry `gh`. It never fails a start: a `github` that does not resolve (not offered, no grant, the registry down, a misfit) starts the Session without it, and `start_session` says why in `warning`. Named toolchains, an agent's own `defaultToolchains` and `noToolchains` are unchanged. `get_agent` and `list_agents` report it as the agent's `defaultToolchains` with `defaultToolchainsImplicit`, and `toolchain_users` names the agent as a user of `github` ([giantswarm#38806](https://github.com/giantswarm/giantswarm/issues/38806)).

## [2.16.0-rc.1] - 2026-10-23

### Added

- `start_session` reads a toolchain version the toolchain-manager's `list_toolchains` lists from the image of the source that offers it, the main source (`--toolchain-repository`) or any other, another registry too. A version two sources publish starts from the main source's image when it is one of them, else it is refused. `toolchain_users` matches a version's Sessions by the digest of every source's image ([giantswarm#38450](https://github.com/giantswarm/giantswarm/issues/38450)).
- `--toolchain-registry-config` (chart `toolchains.registryConfig.secretName`, a `kubernetes.io/dockerconfigjson` Secret, mounted optional): the toolchains of a registry host it has credentials for are read with them, a private source's. Credentials go only to a token realm on the registry's own host over https. Empty, or the Secret absent, reads every registry anonymously as before.

### Changed

- The registry client caches its bearer tokens per host and scope, no longer per scope alone, so the same repository path on two registries no longer shares a token.

## [2.15.0] - 2026-10-23

### Changed

- `create_agent`, `validate_agent` and `update_agent` refuse a Claude-format git plugin on the omp Harness as on `kagent` and `codex`, a Harness change to omp included, so a revision whose boot the omp adapter would refuse is never written. A `byo` Harness is omp by its workload image, the kagent line's `omp-harness`; a `byo` Harness on any other image is unchanged ([giantswarm#38691](https://github.com/giantswarm/giantswarm/issues/38691)).

## [2.14.0] - 2026-10-23

### Added

- `start_session` holds a Session's toolchains to the toolchain-manager's `list_toolchains`, asked through muster as the caller (`--toolchains-tools-prefix`, chart `muster.toolchainsToolsPrefix`, rendered with `toolchains.repository` and `muster.mcpUrl`): a named version must be listed, `vX.Y.Z` or its tag `X.Y.Z`, and none names the newest listed one, so a deprecated or never published version, or a toolchain the installation does not offer, starts nothing. The answer reports the listed version. The caller's missing grant of the toolchain-manager's GitHub App, or muster's sign-in to it, is `auth_required`. Empty keeps any version the registry carries ([giantswarm#38751](https://github.com/giantswarm/giantswarm/issues/38751)).
- `toolchain_users` (`GET /api/v1/toolchains/{name}/users`): the agents whose default toolchains name a toolchain, or one version, and the Sessions that mount it, by digest for a version, read as the caller in every managed namespace; the toolchain-manager's `deprecate_toolchain` names them before a toolchain leaves the offered list.

### Changed

- `--muster-mcp-url` needs `--workspaces-tools-prefix` or `--toolchains-tools-prefix`; the chart renders it, and the NetworkPolicy's egress to muster, for either. The calls through muster share one client.

## [2.13.1] - 2026-10-22

### Changed

- The chart's `toolchains.repository` is empty by default: an installation sets it together with the kagent controller's session tool image repositories, on a kagent line that mounts them. Until then `get_info` reports the capability `toolchains` false and a start naming toolchains is refused, instead of a capability that every start refuses as unsupported.

## [2.13.0] - 2026-10-22

### Added

- `start_session` (and the REST API) takes `toolchains`, `name[@version]` entries (`latest` without a version, at most 16), and `noToolchains`. Each toolchain is the image `<toolchains.repository>/<name>:<version>`, resolved to its digest at the start and passed to kagent's `CreateSession` as a session tool image with the egress origins its `io.giantswarm.toolchain.egress` label declares; kagent mounts it read-only at `/opt/kagent/tools/<name>`. A start that names none gets the agent's default toolchains at their versions (`latest` by default). A toolchain whose `io.giantswarm.toolchain.harnesses` label does not list the agent's Harness runtime, an unpublished version or an image of another name is refused before the Session is created; a controller that creates the Session without its tool images has it deleted again and the start refused as unsupported. The answer lists the mounted toolchains with their digests. `get_info` reports the capability `toolchains` ([giantswarm#38748](https://github.com/giantswarm/giantswarm/issues/38748)).
- `create_agent`, `update_agent` and `validate_agent` take `defaultToolchains` (`[]` on update removes them), kept as the annotation `agent-platform.giantswarm.io/default-toolchains` in the chart's `annotations` value and checked at write time against the registry and the agent's Harness; `get_agent` and `list_agents` report them, and the annotation is refused through `annotations`.
- `--toolchain-repository` (chart `toolchains.repository`, default `gsoci.azurecr.io/giantswarm/agent-toolchains`, rendered with `kagent.controllerTarget`) names where the installation's toolchains are published; empty refuses toolchains.

### Changed

- The kagent protos are those of the fork line's `v1.24.0` with the session tool images (`CreateSessionRequest.tool_images`, `Session.tool_images`).

### Removed

- The chart's `harness.egress` defaults for the interim `claude-go` and `claude-node` Harnesses: a session reaches the Go and npm registries through the `go` and `node` toolchains' declared origins.

## [2.12.0-rc.1] - 2026-10-22

### Added

- `create_agent`, `update_agent` and `validate_agent` (and the REST API) take `egressPolicyRefs`, the names of EgressPolicies in the agent's namespace whose origins the agent may also reach, beside `egress` (`[]` on update clears it); `get_agent` returns them. They render through the agent chart's `agent.egressPolicyRefs` into the Agent's `spec.egressPolicyRefs`. A name with no EgressPolicy in the namespace, read as the caller, is refused, naming the entry and the valid ones; any entry on a coding Harness is refused; the CRD's own limits are the dry-run's answer. New read-only `list_egress_policies` (`GET /api/v1/egresspolicies`) lists a namespace's EgressPolicies as the caller, each origin with its credential header's name, never the Secret. `get_info` reports the capability `egressPolicies`. The binary embeds the agent chart 2.3.0. The ServiceAccount's Role (serviceAccount identity only) reads EgressPolicies ([giantswarm#38744](https://github.com/giantswarm/giantswarm/issues/38744)).

## [2.11.0] - 2026-10-22

### Fixed

- `start_session` binds a Session with the muster provider for a person who may start Sessions but not read HelmReleases (`agent-platform:user`, `agent-platform:platform-user`): the toolset is read, as the caller, from the Agent and its RemoteMCPServer the release rendered, the toolset the agent runs with, instead of the HelmRelease; such a person's Session was created and deleted again. An `update_agent` toolset change binds Sessions started once its release is rendered; an Agent whose own RemoteMCPServer is missing is refused a Session rather than bound with implicit full access ([agent-manager#239](https://github.com/giantswarm/agent-manager/issues/239)).

## [2.11.0-rc.2] - 2026-10-22

### Added

- `create_agent`, `update_agent` and `validate_agent` take `defaultWorkspace` (`<namespace>/<name>`, in the agent's namespace; `""` on update removes it), kept as the annotation `agent-platform.giantswarm.io/default-workspace` in the chart's `annotations` value; `get_agent` and `list_agents` report it, and the annotation is refused through `annotations`.
- `start_session` (and the REST API) takes `noWorkspace`. A start with neither `workspaceGrant` nor `noWorkspace` on an agent with a default workspace calls the workspace-manager's `authorize_workspace` through muster as the caller and starts in it with its grant; a refusal names the repositories, an unconnected provider answers `auth_required` with its muster server and `core_auth_login` link, a deleted default answers `not_found`, and none of them starts a Session. `--muster-mcp-url` and `--workspaces-tools-prefix` (chart `muster.mcpUrl`, `muster.workspacesToolsPrefix`) configure the call; without them a bare start on an agent with a default is refused. `get_info` reports the capability `defaultWorkspace`.

## [2.11.0-rc.1] - 2026-10-22

### Changed

- `create_agent`, `update_agent` and `validate_agent` (and the REST API) validate an agent by rendering the agent chart in-process and dry-running (`dryRun=All`) the rendered Agent and RemoteMCPServer as the caller, before the HelmRelease is written: the API server's field errors (CRD schema, CEL rules, ValidatingAdmissionPolicies, admission webhooks) are the answer, such as `Agent kagent/dba is invalid: spec.egress[0]: Too long: may not be more than 270 bytes`. An object the caller may not write, or a kind the cluster does not serve, is left to Flux and named in `validate_agent`'s `notes`. agent-manager's copy of the Agent CRD's `spec.egress` rules is gone, `--harness-egress` included; a host pattern and port listed over two protocols is now reported by kagent on the Agent's status. The binary embeds the agent chart 2.2.8 archive (was its 2.2.0 values schema) as the offline fallback. The chart's Role grants the ServiceAccount `create` and `update` on Agents and RemoteMCPServers for the dry-run ([giantswarm#38696](https://github.com/giantswarm/giantswarm/issues/38696)).

## [2.10.0] - 2026-10-22

### Changed

- `get_session` (and `GET /api/v1/sessions/{id}`) returns a whole session: `maxBytes` accepts up to 32 MiB (was 1 MiB) for `get_session` and `get_session_turn`, and at `detail: full` the byte budget alone bounds the answer, no longer 200 turns. `summary` and `transcript` keep the 200-turn bound. The default `maxBytes` stays 100000, so `full` without it answers as many turns as fit 100000 bytes rather than at most 200. The tool descriptions of both say such an answer belongs in a file and is customer data ([giantswarm#38577](https://github.com/giantswarm/giantswarm/issues/38577)).
- A `summary` or `transcript` cut by its 200-turn bound hints at `lastTurns` and `detail: full` instead of raising `maxBytes`, which would change nothing.
- The chart's memory limit is 512Mi (was 256Mi): a `get_session` answer at the 32 MiB ceiling takes about 130Mi while it is built.

## [2.9.1] - 2026-10-22

### Fixed

- The image's certs stage builds on alpine 3.24.2 with a current CA bundle instead of `alpine:3.20.3-giantswarm` (2024-09).

## [2.9.0] - 2026-10-21

### Added

- `start_session` binds the agent's toolset with the session at muster: a Session whose sandbox calls muster with its own actor JWT carries the toolset `get_agent` reports at its start (the declared selectors, `preset:none` for an agent without a muster server entry, none for implicit full access), and muster resolves every request of the sandbox within it, header or not. A toolset agent-manager cannot read deletes the Session again, as a refused bind does; `update_agent` applies to new sessions. `start_session` answers the bound `toolset`. ([#228](https://github.com/giantswarm/agent-manager/issues/228))
- `--muster-actor-bearer` (chart `muster.actorBearer`): where muster trusts the actor JWT, `get_agent` and `list_agents` report `toolsetBound` for an agent whose toolset is bound to each session at its start (not for implicit full access), and `get_info` the capability `toolsetBound`.

## [2.8.0-rc.1] - 2026-10-20

### Changed

- `create_agent`, `validate_agent` and `update_agent` (and the REST API) accept `tls://<host>:<port>` in `egress`, a TLS passthrough destination with the host rules of an HTTP(S) origin and a required port, and pass it to the chart's `agent.egress` unchanged; a `tls://` entry without a port is refused with the reason, as is a host pattern and port listed over two protocols. The embedded values schema is the agent chart 2.2.0's; the Agent needs kagent 1.21.0 or later ([agent-manager#104](https://github.com/giantswarm/agent-manager/issues/104)).

## [2.8.0-rc.1] - 2026-10-20

### Changed

- `create_agent`, `validate_agent` and `update_agent` refuse a Claude-format git plugin (`.claude-plugin/plugin.json`, no root `plugin.json`) on a `kagent` Harness as on `codex`, a Harness change to `kagent` included, naming the Harness's runtime and the fix, a root `plugin.json`. On both, the format is looked up on create, on a Harness change and for a plugin pin an update adds or changes; an update that keeps the Harness and the pins makes no lookup, so a GitHub outage no longer blocks a prompt or model edit. OCI plugins stay unchecked ([giantswarm#38689](https://github.com/giantswarm/giantswarm/issues/38689)).

## [2.7.2] - 2026-10-20

### Changed

- `start_session` asks kagent for the providers of a session it binds: with muster's session bindings configured, its `CreateSession` sets `bind_actor` (kagent 1.20.0), and only such a session without a workspace lists muster. A session created straight through kagent's API is not bound and keeps forwarding the caller's credential to muster. The kagent protos are those of kagent-upstream 1.20.0 ([giantswarm#38668](https://github.com/giantswarm/giantswarm/issues/38668)).

## [2.7.1] - 2026-10-19

### Changed

- `start_session` binds every session kagent gives providers to the caller in muster, not only a workspace one: where muster verifies the sandbox's own JWT, kagent lists muster on a session started without a workspace too (a Slack thread's), and its sandbox now reaches muster's tools as the person. A session without providers is not bound, as before ([giantswarm#38668](https://github.com/giantswarm/giantswarm/issues/38668)).

## [2.7.0-rc.1] - 2026-10-19

### Added

- `update_agent` takes `harness`: an existing agent moves to another Harness of its namespace after the checks a create on that Harness runs (model protocol, Claude-format plugins on codex, the Harness's egress entry, bindings); nothing is written when one fails and `validate_agent` with `update: true` lists them. A move from a coding Harness to a `kagent` one drops the coding Harness's egress list and prompt note. Sessions keep the revision they started on ([giantswarm#38635](https://github.com/giantswarm/giantswarm/issues/38635)).

## [2.6.0] - 2026-10-18

### Added

- `create_agent`, `validate_agent` and `update_agent` pin `plugins[]` at write time the way they pin skills: a git plugin's `ref` (or its default branch) resolves to its head commit, an OCI plugin's tag to its digest, and `refreshSkills` re-pins every git plugin too. A plugin's skill selection is optional, as in the Generic chart 2.1: a Claude Code format plugin loads whole and needs none. A skill name used twice across `skills` and the plugins' selections is refused, and a git plugin from a repository the caller cannot read is refused like a skill. The embedded chart schema is 2.1.0 ([giantswarm#38077](https://github.com/giantswarm/giantswarm/issues/38077)).

## [2.6.0-rc.1] - 2026-10-18

### Added

- `create_agent`, `validate_agent` and `update_agent` pin `plugins[]` at write time the way they pin skills: a git plugin's `ref` (or its default branch) resolves to its head commit, an OCI plugin's tag to its digest, and `refreshSkills` re-pins every git plugin too. A plugin's skill selection is optional, as in the Generic chart 2.1: a Claude Code format plugin loads whole and needs none. A skill name used twice across `skills` and the plugins' selections is refused, and a git plugin from a repository the caller cannot read is refused like a skill. The embedded chart schema is 2.1.0 ([giantswarm#38077](https://github.com/giantswarm/giantswarm/issues/38077)).

## [2.5.1-rc.1] - 2026-10-18

### Changed

- A workspace session's binding in muster carries the reserved provider `muster` when kagent admitted the session to it (where muster verifies Substrate's actor JWT), so muster serves the session's sandbox as the person who started it; agent-manager binds the providers kagent lists and never adds `muster` itself ([giantswarm#38064](https://github.com/giantswarm/giantswarm/issues/38064)).

## [2.5.0] - 2026-10-17

### Fixed

- The tenant admission policy `agent-manager-tenants` admits the refresh of the skills boot Secret under every previous name the chart renders while minting (`skills.previousGitAuthSecretNames` and the `tenant-credential-` name), not only the current name: agent-manager no longer logs `skills boot Secret refresh failed` every minute, and an agent still naming a previous name keeps a fresh token until it is migrated ([agent-manager#197](https://github.com/giantswarm/agent-manager/issues/197)).

## [2.5.0-rc.4] - 2026-10-17

### Fixed

- An agent in an Organization's namespace no longer keeps the installation's skills credential: `update_agent` drops a `skillsGitAuthSecretRef` naming it under its current or a previous name (it kept the current name), so Flux applies the update the tenant credential admission policy refused, and `agent-manager migrate --multi-tenant` (`AGENT_MANAGER_MULTI_TENANT`) drops it from every agent release in the tenant namespaces, found as the tenant reconciler finds them, reading and writing only HelmReleases there; the default namespace's report lists them under `tenants` ([agent-manager#191](https://github.com/giantswarm/agent-manager/issues/191)).

## [2.5.0-rc.2] - 2026-10-17

### Added

- A workspace session's sandbox acts as the person who started it: `start_session` with a `workspaceGrant` binds the session's Substrate actor (`spiffe://substrate-actor.local/actor/<atespace>/<actor>`, from the session's A2A authority) to the caller at muster's session bindings, as the caller (the forwarded IdP token), with the provider instances kagent admitted the session to, before its first message; muster then releases the caller's provider tokens to that sandbox alone. A bind muster refuses fails the start with muster's reason and deletes the session again; a session without a workspace, or whose workspace has no provider, is never bound. `delete_session` unbinds a bound session after kagent deleted it; a missed unbind is the answer's `warning` and expires with the Session idle TTL. agent-manager holds no token. New `--muster-bindings-url` (`AGENT_MANAGER_MUSTER_BINDINGS_URL`), chart `muster.bindingsUrl` (default the in-cluster `http://muster.agent-platform.svc.cluster.local:8090/session-bindings`, rendered with the session tools, the network policy opening egress to it); without it a workspace grant is refused as unsupported ([giantswarm#38597](https://github.com/giantswarm/giantswarm/issues/38597)).

## [2.5.0-rc.1] - 2026-10-17

### Changed

- The minted skills credential is the installation's own again, never a tenant's: with chart `skills.mintGitAuthSecret` the Secret is `<fullname>-skills-token`, outside the `tenant-credential-` prefix that marks a tenant's own credentials, so admission that lets an agent in an Organization's namespace name only those refuses it there. Its earlier name `tenant-credential-<fullname>-skills-token` is a previous name: still rendered and filled with the same token, `update_agent` composes an agent naming it with the current name and `agent-manager migrate` moves every other release. agent-manager warns at start when the installation's skills credential carries the `tenant-credential-` prefix.

## [2.4.1-rc.2] - 2026-10-17

### Fixed

- An MCP tool's refusal names its code once: `forbidden: create HelmRelease …` and `conflict: HelmRelease … already exists` instead of `forbidden: forbidden: …` and `conflict: conflict: …`; `not_found:` and `invalid_request:` no longer repeat `not found:` and `invalid request:`. The REST error body is unchanged.

## [2.4.0] - 2026-10-16

### Changed

- The MCP tool descriptions are about 40% shorter (~6.6k instead of ~11.1k tokens with the schemas). Each starts with a summary line of at most 120 runes, all muster's `filter_tools` shows, saying what the tool does instead of `WRITES:` or `Read-only.`, which the annotations already say; the details keep what a model needs before a call (confirm the name with the person, a toolset is required, poll `get_agent_status`, agents have no pods, `organization` and `cluster` address the agent in every later call of a tool that takes them, `update_agent` replaces whole lists and the `labels` and `annotations` maps, pass a `requestId` and reuse it on a retry) and leave what the answers and refusals already say. Every parameter description fits 150 runes. The descriptions are built with mcp-toolkit's `tooldesc.Describe` (v0.4.0), `docs/tools.md` lists every tool with its input schema as a model reads them (`make tools-doc`, kept current by `tooldesctest.AssertServerDoc`) and `tooldesc.CheckServer` holds the budgets ([giantswarm#38592](https://github.com/giantswarm/giantswarm/issues/38592)).

### Fixed

- `create_agent`, `update_agent` and `validate_agent` name the platform default ModelConfig among the valid ones when a `modelConfig` is refused in a tenant namespace: before the tenant's first agent its namespace holds none, the default comes with that agent, and the refusal said a platform admin provisions ModelConfigs.

## [2.3.1] - 2026-10-16

### Fixed

- The tenant reconciler removes a tenant's model key Secret with its last agent whatever the tenant's ModelConfig copy names: a copy that named another Secret at that moment, or one already deleted by a release that failed before the Secret, left the Secret behind until the next resync. ([#182](https://github.com/giantswarm/agent-manager/issues/182))

## [2.3.1-rc.1] - 2026-10-16

### Fixed

- `create_agent` (mode `apply`, dry run included) asks whether the caller may create a HelmRelease in the namespace (a `SelfSubjectAccessReview`) before it looks the name up: a caller who may read but not create gets `403 forbidden` whether or not the name is taken, naming the user as the API server's own refusal does, instead of `409 conflict` for a taken one. `validate_agent` for a create lists the refusal as a violation and leaves the name unchecked. Mode `commit` is unchanged. ([#176](https://github.com/giantswarm/agent-manager/issues/176))

## [2.3.0] - 2026-10-16

### Changed

- The minted skills credential is named under the tenant credential convention: with chart `skills.mintGitAuthSecret` the Secret is `tenant-credential-<fullname>-skills-token`, the one Secret name an agent's git `credentialRef` may carry, and every git-skill agent is composed with it. Its earlier name `<fullname>-skills-token` stays rendered and filled with the same token, so an agent not yet moved keeps booting. New `skills.previousGitAuthSecretNames` (`--skills-git-auth-previous-secret-names`, `AGENT_MANAGER_SKILLS_GIT_AUTH_PREVIOUS_SECRET_NAMES`; the minted Secret's earlier name is always one) lists a renamed credential's old names: `update_agent` composes an agent naming one with the current name, and `agent-manager migrate` (`--skills-git-auth-secret-name`, `--skills-git-auth-previous-secret-names`) moves every other release, also one already on the target chart major; a GitOps-owned release gets the diff, and where a release still names a previous name the run waits up to `--skills-git-auth-wait` (`AGENT_MANAGER_SKILLS_GIT_AUTH_WAIT`, default 10m, within the connectivity chart's 15-minute run deadline) for the Secret of the current name to hold its token (agent-manager's chart creates it, agent-manager fills it, and kagent refuses a credential without the key); a release whose namespace still has none keeps the previous one, with a warning, until a later run. `get_info.skillsGitAuthMint.previousSecretNames` lists the old names kept filled. agent-manager warns at start when the installation's skills credential lacks the `tenant-credential-` prefix.

## [2.2.1-rc.1] - 2026-10-15

### Fixed

- `delete_agent` of an organization namespace's last agent keeps the chart source `OCIRepository/agent` that the tenant reconciler owns: the answer has `ociRepositoryDeleted: false` and the reason in `ociRepositoryKept`, in apply and commit mode. In a namespace without the tenant label, the last delete removes the source as before. ([#175](https://github.com/giantswarm/agent-manager/issues/175))

## [2.2.0] - 2026-10-15

### Fixed

- A tenant's WorkerPool copy is exactly the platform WorkerPool's spec with the tenant's `replicas`; a field removed from the platform WorkerPool is removed from every copy at the next reconcile, so `agent-platform-tenant-workerpool-copy` no longer refuses the copy's updates. ([#173](https://github.com/giantswarm/agent-manager/issues/173))

## [2.2.0-rc.1] - 2026-10-15

### Changed

- The coding-agent guard follows the Harness's runtime kind instead of `spec.claude`: `create_agent`, `update_agent` and `validate_agent` hold an agent on every Harness that does not run the `kagent` runtime (`claude`, `codex`, `byo` such as omp, and any runtime added later) to the rules a claude Harness agent had: no `requireApproval` binding, no `extraTools` or `extraAgentSpec.tools`, any muster toolset, write tools included, the Harness's egress origins written and the egress note in the prompt. A coding Harness without a `--harness-egress` entry is refused naming what it runs; the chart's `harness.egress` ships `codex` and `omp` entries (GitHub) beside `claude`'s. They refuse a ModelConfig the runtime cannot reach, by the rules of kagent's compilers: `claude` takes Anthropic, Bedrock and AnthropicVertexAI; `codex` an OpenAI ModelConfig with `openAI.apiFormat: responses` or a Bedrock `gpt-*` one. On `codex` a Claude-format git plugin (`.claude-plugin/plugin.json`, no root `plugin.json`) is refused with the reason, never converted. ([#172](https://github.com/giantswarm/agent-manager/pull/172))

## [2.1.0] - 2026-10-15

### Added

- The chart's muster `MCPServer` carries the `ui.giantswarm.io/display-name` annotation, the name the Dev Portal shows for the server (giantswarm/backstage#3012): new value `muster.mcpServer.displayName`, default `Agent Manager`. Empty renders no annotation. ([#169](https://github.com/giantswarm/agent-manager/pull/169))

## [2.1.0-rc.2] - 2026-10-15

### Fixed

- `create_agent`, `update_agent` and `validate_agent` refuse an agent on a claude Harness that `--harness-egress` (chart `harness.egress`) has no entry for, naming the missing entry, instead of silently giving it no origins; an empty list still means no origin. The Helm test covers every default origin of `claude-go` (`sum.golang.org` included) and `claude-node`. The `requireApproval` refusal says why: approval is no boundary against Claude Code. A test proves kagent's Sandbox tools pass the guard. ([#148](https://github.com/giantswarm/agent-manager/issues/148), [#149](https://github.com/giantswarm/agent-manager/issues/149))

## [2.1.0-rc.1] - 2026-10-15

### Added

- Tenants (chart `tenants.enabled`, flag `--multi-tenant`): every namespace labelled `giantswarm.io/organization` is a managed namespace, found at run time. agent-manager keeps in each, as its ServiceAccount, the agent chart's OCIRepository, the agents' Flux ServiceAccount bound to `agent-platform-tenant-flux` and the network policies named in `tenants.networkPolicy`; while the namespace holds an agent, it also keeps copies of the platform Harness, its WorkerPool (`tenants.workerPoolReplicas`, default 1) and the default ModelConfig with its key Secret, and removes them with the last agent. A tenant's own objects are never changed. A tenant is reconciled at once when its namespace, an agent in it, one of these objects or the platform object it is copied from changes, and every tenant every `tenants.interval` (default 10m). Of several replicas, the one that holds the Lease `agent-manager-tenants` in the release namespace reconciles (`--tenant-leader-election-namespace` names another namespace, needed outside a cluster). The kinds it keeps must be served (Agent, Harness, ModelConfig, WorkerPool, HelmRelease, OCIRepository and the network policy kind): without one, agent-manager does not start and its error names each missing kind. When the reconciler stops at run time (a lost Lease, say), the API keeps serving: the replica logs the error, `get_info` reports it in `tenantReconciler` (`running`, `error`, `failedAt`), and a new reconciler starts after 10 seconds (up to 5 minutes while new ones fail) and reconciles once it holds the Lease. Until it starts, that replica's API uses the tenant namespaces known at the failure. Needs `oauth.downstream.enabled` and `flux.helmReleaseServiceAccount`; an agent in a tenant gets no installation skills credential. The chart renders the ClusterRoles `agent-platform-tenant-flux` and `<fullname>-tenants` (whatever `oauth.downstream` says), the Role `<fullname>-tenants-leader` for that Lease, `list` and `watch` on every kind the reconciler reads (on Secrets only with the model key's name) and a ValidatingAdmissionPolicy that refuses the ServiceAccount's writes outside labelled namespaces. `agent-platform-tenant-flux` has on Secrets only the verbs Helm's release storage uses (`get`, `list`, `create`, `update`, `delete`); with them it can read, overwrite and delete every Secret of the tenant, CAPI cluster kubeconfigs included, and get the token of any ServiceAccount there (`automation` included), so whoever may create or change a HelmRelease or an OCIRepository there (on a Giant Swarm management cluster, the organization's admins and the members bound to `agent-platform:editor` or `agent-platform:admin`) can do so through a chart of their own, for example with Helm `lookup` (README, Tenants). Needs Kubernetes 1.30 or later. ([#71](https://github.com/giantswarm/agent-manager/issues/71))
- An agent records its creator: a create writes the caller as `agent-platform.giantswarm.io/created-by` on the HelmRelease and on the Agent. An update keeps it, and a request that sets or changes it is refused. `get_agent` and `list_agents` report it as `createdBy`. ([#71](https://github.com/giantswarm/agent-manager/issues/71))

### Changed

- **Breaking:** `get_info` lists in `namespaces.managed` only the managed namespaces in which the caller may create agents (a `SelfSubjectAccessReview` for `create` on `helmreleases`); a namespace where the caller may only read agents is no longer listed. A client that lists agents across namespaces asks for each namespace itself. ([#71](https://github.com/giantswarm/agent-manager/issues/71))

## [2.0.1] - 2026-10-14

### Fixed

- `start_session` sends a session's first message once per `requestId`: the message id is derived from the `requestId`, and a retry whose session's first turn already carries it answers that turn's latest state as `turn` (without its history) and sends nothing, so a retry after a lost answer starts no second, paid turn. ([#136](https://github.com/giantswarm/agent-manager/issues/136))

## [2.0.0] - 2026-10-14

### Added

- `delete_session` (`DELETE /api/v1/sessions/{id}`): deletes one of the caller's sessions and its runtime state as the caller through `SessionService/DeleteSession`, answering the session in `RUNTIME_STATE_DELETED`; it is gone from `list_sessions` and `get_session` after. The tool is annotated destructive. kagent scopes the delete to the session's creator, so another person's session is refused as `not_found`; while another operation on the session runs the delete is refused as `conflict` with a retry hint. ([#154](https://github.com/giantswarm/agent-manager/issues/154))

## [2.0.0-rc.1] - 2026-10-14

### Added

- `get_session_turn` (REST: `GET /api/v1/sessions/{id}/turns/{turn}`) reads one turn of a session, by its task id, its index or `last`, through `A2AService/GetTask`: its messages and then its artifacts from `fromMessage` on, as a transcript or as A2A objects (`detail=full`), paged within `maxBytes` with `nextMessage`, or with `toolCall` one tool call whole, its response unwrapped from the MCP envelopes muster nests it in; when a call does not fit, the smaller of arguments and response stays whole. ([#150](https://github.com/giantswarm/agent-manager/pull/150))

### Changed

- **Breaking:** `get_session` (REST: `GET /api/v1/sessions/{id}`) answers a bounded summary by default instead of every task whole. `detail` picks the view: `summary` (default) is per turn the task id, state, timestamp, the user's message, the agent's reply (the status message, else the artifacts' text), one stub per tool call and the number of artifacts; `transcript` is every message and artifact of each turn, with tool call arguments clipped and tool results as stubs; `full` is the former `tasks`, always present. `lastTurns` answers only the last n turns. `maxBytes` (4096 to 1048576, default 100000) bounds every view, REST bodies are compact: the newest turns that fit are kept, at most 200, and the older ones counted in `omittedTurns`. The tasks are listed without their history and only the turns answered are read whole, so polling with `lastTurns: 1` reads one turn. A conversation of more than 10000 turns is `409 conflict` rather than answered without its newest turns. A caller that reads `tasks` passes `detail=full`. ([#150](https://github.com/giantswarm/agent-manager/pull/150))

## [1.14.0] - 2026-10-13

### Changed

- An agent on a claude Harness takes any toolset, as every other agent does: `create_agent`, `update_agent` and `validate_agent` no longer resolve it with muster's `filter_tools` and no longer refuse a tool without `readOnlyHint`, `invoke_agent_instance`, the `preset:agent-platform` write tools, a selector that selects nothing or a server awaiting sign-in. Which muster tools a coding agent reaches is the person's choice; muster authorizes every call as the person. The egress list, the egress note and the `requireApproval` and extra-binding refusals stay. The chart's network policy no longer opens egress to muster, which only that check called. ([#151](https://github.com/giantswarm/agent-manager/issues/151))

## [1.13.0] - 2026-10-12

### Changed

- `message` on `start_session` (REST: the same field) is optional. Without it, `start_session` only creates the session, with the same `workspaceGrant` checks, and answers `{session, workspace}` with no `turn`: the caller sends the first message itself over A2A with the session as `contextId`, for instance one with file parts. With it, nothing changes. ([#137](https://github.com/giantswarm/agent-manager/issues/137))

## [1.12.1-rc.1] - 2026-10-12

### Fixed

- The MCP endpoint refuses a request body larger than 1 MiB, the REST API's bound, before mcp-go reads it whole (CVE-2026-108859, no fixed mcp-go release yet; the scan ignores it until 2026-11-12). ([#139](https://github.com/giantswarm/agent-manager/issues/139))

## [1.12.0] - 2026-10-12

### Added

- `workspaceGrant` on `start_session` (REST: the same field): the signed, short-lived grant a workspace manager's `authorize_workspace` returned to the caller starts the session in that workspace. agent-manager decodes the grant without trusting it, refuses one that names another person, has expired or cannot be read (`403` or `400` with the reason, nothing created), and passes it to kagent's `CreateSession` as the `admission_token` of the `volume_source` it names (the session's own directory read-write at `/workspace`, the workspace's mirrors read-only at `/mirrors`); kagent's admission verifies it and refuses with its reason. The answer names the `workspace`, the session shows its `volumeSource`, and without a grant the request is the one the surfaces send today. The grant appears in no log line, error or response. The kagent stubs are generated from `giantswarm/kagent-upstream` v1.10.0, the line whose `CreateSession` takes a volume source and an admission token.

## [1.11.1] - 2026-10-10

### Fixed

- The claude Harness guard no longer refuses a preset for a caller with servers awaiting sign-in: a `preset:` selector spans every server and muster resolves it per request, admitting the preset's tools only, so `validate_agent`, `create_agent` and `update_agent` accept `["preset:read-only"]` before the caller has signed in to every OAuth server. A `server:` or `tool:` selector naming a server awaiting sign-in is still refused, naming that server, and a preset reaching a write tool or `invoke_agent_instance` still is. ([#129](https://github.com/giantswarm/agent-manager/issues/129))

## [1.11.0] - 2026-10-10

### Fixed

- The claude Harness guard accepts `preset:read-only`, the toolset its own refusal recommends: a `preset:agent-platform` tool is refused only when it is not read-only, so muster's read-only core tools and agent-manager's read tools pass, while `invoke_agent_instance` and every write tool stay refused. The sign-in refusal reads the server names muster sends (`name`) and appears only when at least one server awaits sign-in, where it named none before. ([#126](https://github.com/giantswarm/agent-manager/issues/126))
- The claude Harness guard refuses a `tool:`, `server:` or `workflow:` selector that resolves to no tool for the caller, naming the selector: muster resolves the toolset on every request, so a write tool appearing later under that name would have reached the agent unchecked. A preset that selects nothing (`preset:none`) stays allowed. ([#127](https://github.com/giantswarm/agent-manager/issues/127))

## [1.10.1] - 2026-10-08

### Fixed

- `validate_agent` (`POST /api/v1/agents/validate`) takes `mode` like the write tools and answers as the write would: with `update: true, mode: commit` it validates a change to an agent whose HelmRelease is applied from git (`managed: gitops`), the change `update_agent` in commit mode opens as a pull request, and returns the composed manifests and every violation, where it answered `gitops_owned` before, so a portal could never enable its Commit button for exactly the agents it is meant for. Without `mode` (apply) such a release is still refused with `gitops_owned`, now `force` or not, as `update_agent` refuses it; `force` only passes a suspended release. A create's dry run checks `mode` the same way (`unsupported` where commit is not configured).

## [1.10.0] - 2026-10-08

### Added

- Session tools for talking to an agent, as MCP tools and REST operations: `list_sessions` (`GET /api/v1/agents/{namespace}/{name}/sessions`, `allCreators`), `get_session` (`GET /api/v1/sessions/{id}`: the session and the A2A tasks of its context) and `start_session` (`POST /api/v1/agents/{namespace}/{name}/sessions`: `message`, `requestId`, `sessionName`; creates the session, sends the first message and answers once the turn is accepted). They call the kagent 1.3 `SessionService` and the A2A service through agentgateway at `--kagent-target` (`AGENT_MANAGER_KAGENT_TARGET`, chart `kagent.controllerTarget`, default `grpc://agentgateway.agent-platform.svc.cluster.local:8080`, rendered with `oauth.enabled` only) with the caller's IdP token and never as the ServiceAccount. Session and task bodies are kagent's proto3 JSON. `get_info` reports `capabilities.sessions`; the chart's network policy opens egress to the target. ([#3](https://github.com/giantswarm/agent-manager/issues/3))

## [1.10.0-rc.1] - 2026-10-08

### Added

- `plugins` on `create_agent`, `update_agent` and `validate_agent` (REST: the same field): a list of `{git: {url, commit}, path, skills: [...]}` or `{oci: <ref>@sha256:<digest>, path, skills: [...]}` entries composed as the agent chart's `plugins[]` and rendered as the Agent's `spec.template.plugins[]`. A plugin is written as given and must come pinned (a full commit id or a digest; a ref, a short SHA or an image tag is refused), and must name at least one of the bundle's skills (unique). `update_agent` replaces the whole list; `[]` clears it. `gitAuthSecretName` and the installation's skills credential reach git plugins as they reach git skills. `get_agent` / `list_agents` report `plugins`.
- `egress` on `create_agent`, `update_agent` and `validate_agent` (REST: the same field): the HTTP(S) origins the agent may reach besides what its revision compiles, such as `https://github.com:443` or `https://*.githubusercontent.com` (a wildcard replaces the leftmost label); at most 64, none twice, written as the chart value `agent.egress`, the Agent's `spec.egress`. `update_agent` replaces the whole list, `[]` clears it. `get_agent` / `list_agents` report it.
- `agent-manager migrate` moves Generic chart 1.x releases (`kagent.dev/v1alpha3` `AgentTemplate`) to 2.x (`api.kagent.dev/v1alpha3` `Agent`). Sub-agent bindings in `extraTools[]` and `extraAgentSpec.tools[]` become `{subAgent: {name, description, templateRef}}` (`isolation: Shared` dropped, `Dedicated` refused), and `labels["agent-platform.giantswarm.io/harness"]` is dropped, becoming `agent.harness` when it named another Harness. Each namespace's agent-chart `OCIRepository` moves to the 2.x range (and filter) once all its releases are on 2.x values. The wait phase gates on the `Agent` being Ready (the `get_agent_status` verdict) and on no `kagent.dev` `AgentTemplate` still rendered by a release Flux has not upgraded; the contract phase deletes the leftover `kagent.dev` `AgentTemplate` objects and no CRD. A sub-agent `templateRef` that names no `api.kagent.dev` `AgentTemplate` in the namespace is reported in the release's `warnings`. `--dry-run` works before the kagent upgrade. The report carries `run.path` and each old-line object's `kind`.
- `get_info` reports `apiVersions.agent` next to `apiVersions.agentTemplate`; `get_agent` / `list_agents` report `harness` (the Agent's `spec.harnessRef.name`, or the chart value while nothing is rendered) and `status` (the Agent's own `exists`, `generation`, `observedGeneration`, `harness`, `ready`, `accepted`, `resolvedRefs`, `compatible`, `desiredRevision`, `latestSuccessfulRevision`, `conditions`, `warnings`); `get_agent_status` reports the same block as `agent`; `delete_agent` reports `agentDeleted`.

### Changed

- kagent API group `api.kagent.dev/v1alpha3` for everything agent-manager reads and composes: the object an agent is read from is the `Agent` (its template inline under `spec.template`, its Harness by name in `spec.harnessRef`), next to the `RemoteMCPServer`, `Harness` and `ModelConfig` of the same group. Readiness is the Agent's own `status.conditions` (`Accepted`, `ResolvedRefs`, `Compatible`, `Ready`) and revisions; a Harness that does not exist is `ResolvedRefs` False. Discovery (`--kagent-api-version auto`, chart `kagent.apiVersion`) reads the version serving `agents`. The chart's Role names `agents`, `harnesses`, `remotemcpservers` and `modelconfigs` of `api.kagent.dev` (`agents` and `remotemcpservers` also delete).
- The Generic agent chart range every agent tracks moves to `2.x` (`--agent-chart-semver`, chart `agentChart.semver`, `get_info chart.semver`): the chart's next major, which renders one `Agent` named after the release. The embedded fallback schema is the 2.x values contract (`plugins[]`, `plugin.schema.json`).
- `agent-manager migrate` picks its path from the major of `--agent-chart-semver` (default `2.x`): `2.x` migrates 1.x releases, `1.x` migrates 0.x releases, and a range spanning two majors is refused. It needs `agents.api.kagent.dev` on `2.x` and `agenttemplates.kagent.dev` on `1.x`. A release deployed from a chart older than the path's source major is reported as `failed` and left as it is. Every release is validated against the target schema, including one already on target values. The releases of one chart source are written only when all of them validate: a failed or pending release holds its writable siblings as `pending` (`held: …`) instead of leaving them on target values under the old chart. On `1.x` the wait phase reads the `kagent.dev` `AgentTemplate`'s `status.harnesses[]` entry for `--harness-name`.
- `create_agent` / `validate_agent` refuse a `harness` no Harness of the namespace carries, listing the ones that exist; the platform Harness may be absent at write time (the Agent then reports `ResolvedRefs` False until the connectivity chart provisions it). The tool descriptions say what naming a Claude Code Harness implies: the agent runs as a Claude Code session, skills and plugins are Claude Code skills and plugins there, and the per-turn bounds are the Harness's (`Harness.spec.claude.limits`).
- The ATS smoke no longer applies the `kagent.dev/v1alpha3` CRDs; the `api.kagent.dev` ones are added once a `giantswarm/kagent-upstream` release tag carries them. Until then the service falls back to `v1alpha3` on discovery.

### Removed

- The `kagent.dev/v1alpha3` `AgentTemplate` as the object an agent is read from, and with it the per-Harness status: `get_agent` / `list_agents` no longer report `harnesses[]`, `get_agent_status` no longer reports `template`, and `delete_agent` no longer reports `agentTemplateDeleted` (see Changed for the replacements). The admission label `agent-platform.giantswarm.io/harness` and `Harness.spec.allowedAgentTemplates` are gone from the API: `harness` on `create_agent` / `validate_agent` is checked against the Harnesses that exist in the namespace, not against a selector.

## [1.9.3-rc.5] - 2026-10-08

### Changed

- `list_agents` (and `GET /api/v1/agents`) reports one summary per agent: `name`, `namespace`, `organization`/`cluster`, `exists`, `displayName`, `description`, `modelConfig`, `toolset`, `implicitFullAccess`, `ready` and `managed`, plus `suspended`, `deleting`, `releaseReason` and `releaseMessage` when the HelmRelease is suspended, being uninstalled or not Ready. The system prompt, pinned skills, MCP bindings, per-Harness status, HelmRelease and values come from `get_agent`, or for every agent at once from `details: true` (REST `?details=true`). On gazelle a full listing of 19 agents was about 250k characters, most of it every system prompt twice. ([giantswarm/giantswarm#38221](https://github.com/giantswarm/giantswarm/issues/38221))

## [1.9.3-rc.4] - 2026-10-08

### Changed

- `list_skills` (and `GET /api/v1/skills`) lists every skill once, under its repository. The top-level `skills`, which repeated every repository's skills, is gone, and a skill carries only `name`, `description` and `path`; its repository's `repoUrl`, `ref` and `commit` are no longer repeated on each skill. MCP tool results are compact JSON instead of indented. A full `list_skills` on gazelle (153 skills in three repositories) shrinks from about 184k to 63k characters. ([giantswarm/giantswarm#38220](https://github.com/giantswarm/giantswarm/issues/38220))

## [1.9.2] - 2026-10-05

### Fixed

- `get_agent_status` (and the REST status) reports `progressing`, not `failed`, while the HelmRelease waits for its chart source or a dependency (`Ready=False` with `SourceNotReady` or `DependencyNotReady`), so an agent created right after the namespace's shared OCIRepository was garbage-collected no longer reads as failed until Flux catches up. A source that reports its own failure (authentication, no matching tag) and a `Stalled=True` release still read `failed`, as do `InstallFailed`, `UpgradeFailed`, `ArtifactFailed` and every other `Ready=False` reason. ([#96](https://github.com/giantswarm/agent-manager/issues/96))

## [1.9.1] - 2026-10-02

### Fixed

- A pod that starts while Dex is unavailable (its only replica rescheduled, discovery answering 503) waits for it instead of crash-looping: mcp-oauth v1.7.0 retries OIDC discovery for up to 5 minutes, and the chart's startup probe (`/healthz`, up to 5m30s) keeps the liveness probe from restarting the container while it waits. ([giantswarm/giantswarm#38102](https://github.com/giantswarm/giantswarm/issues/38102))

## [1.9.1-rc.2] - 2026-10-01

### Fixed

- `validate_agent` of a create no longer fails for a caller who may not read the namespace's HelmReleases: it returns the manifests and the other violations, and `notes` (REST: the same field) says the name could not be checked for a clash. A taken name is still a violation. ([#45](https://github.com/giantswarm/agent-manager/issues/45))

## [1.9.0] - 2026-09-30

### Added

- Agents on a workload cluster, managed from the installation. `organization` and `cluster` on `create_agent`, `validate_agent`, `update_agent`, `delete_agent`, `get_agent`, `get_agent_status`, `list_agents` and `list_model_configs` (REST: the same fields and query parameters) place the agent's HelmRelease, `<cluster>-<agent>`, in `org-<organization>` on the installation with `spec.kubeConfig.secretRef` naming the cluster's Cluster API Secret `<cluster>-kubeconfig` (key `value`), `releaseName`, `targetNamespace` and `storageNamespace`, beside the shared OCIRepository there; the AgentTemplate, its RemoteMCPServer, the ModelConfigs, the Harnesses and the events are read on the workload cluster through a client built from that Secret that presents the caller's token (the kubeconfig's own credentials only when running as the ServiceAccount). A 401 there names the identity precondition. Such agents reach muster at `--target-muster-url` (`AGENT_TARGET_MUSTER_URL`, chart `muster.targetUrl`); unset, a target is refused. `get_info` reports `capabilities.targetCluster` and `muster.targetUrl`. Without a target nothing changes. ([#22](https://github.com/giantswarm/agent-manager/issues/22))

## [1.8.0] - 2026-09-30

### Added

- The skills boot Secret minted from the skills GitHub App, so no personal token is needed to boot private git skills. With chart `skills.mintGitAuthSecret` (`--skills-git-auth-mint`, `AGENT_MANAGER_SKILLS_GIT_AUTH_MINT`; needs `skills.github.app.secretName`, replaces `skills.gitAuthSecretName`) the chart renders the Secret `<fullname>-skills-token`, without data, in every managed namespace, with a Role that may only get and update it. agent-manager writes the App's installation token into it (key `token`, the Basic form of `x-access-token:<token>`), checks it every 10 minutes and replaces it once less than 45 minutes are left, retrying a failure after a minute. The Secret is every git-skill agent's `skillsGitAuthSecretRef.name`. `get_info.skillsGitAuthMint` reports the last refresh, the token's expiry and a failed refresh's error, and the failure is logged. ([#81](https://github.com/giantswarm/agent-manager/issues/81))

## [1.7.0] - 2026-09-29

### Added

- `agentChart.semverFilter` (`--agent-chart-semver-filter`, `AGENT_CHART_SEMVER_FILTER`): a regular expression the agent chart's tags must match before `agentChart.semver` is evaluated. It is composed as the per-namespace OCIRepository's `ref.semverFilter`, the resolver picks the version whose values schema validates agents among the matching tags only, and `migrate`, when the flag or variable is given (even empty), moves a source's filter together with its range; left unset, each source keeps its own. A release deployed from a tag the filter rejects counts as not upgraded yet. The report carries `targetSemverFilter` and each source's `fromSemverFilter`/`toSemverFilter`. With a range that admits pre-releases, `^v?[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$` follows release candidates and keeps the chart's branch builds out. Empty (default) filters nothing.

## [1.6.2] - 2026-09-29

### Fixed

- `list_skills` answers within muster's 30 s: the SKILL.md files of a repository are read in parallel (eight at a time) and the configured repositories side by side, where a cold read of three repositories took longer than 30 s and every meta agent's first call timed out.
- A read finishes and is cached even when the caller gives up, so a timed-out call no longer leaves a truncated listing (every remaining SKILL.md "unreadable") in the cache.
- An expired cache entry whose ref still points at the same commit is renewed with two requests instead of re-reading every SKILL.md.

## [1.6.0] - 2026-09-28

### Added

- Private skills. `gitAuthSecretName` on `create_agent`, `update_agent` and `validate_agent` (REST: the same field) names the Secret (key `token`) the agent's git skills are fetched with at boot, composed as the agent chart's `skillsGitAuthSecretRef.name`; omitted, every agent with a git skill gets the installation's (`--skills-git-auth-secret-name`, chart `skills.gitAuthSecretName`, `get_info.skillsGitAuthSecretName`), and an update keeps it in step with the skills. Discovery and pinning read GitHub with a read-only GitHub App's installation tokens (`--skills-github-app-id`, `--skills-github-app-installation-id`, `--skills-github-app-private-key-file`; chart `skills.github.app.secretName`) instead of a static token. A private skill repository is listed to, and usable by, only the callers whose GitHub login (verified by the App pin) can read it; otherwise `403 forbidden`. The embedded fallback schema is the agent chart 1.5.1's. ([#44](https://github.com/giantswarm/agent-manager/issues/44))

## [1.5.0] - 2026-09-28

### Added

- Write modes on `create_agent`, `update_agent` and `delete_agent` (REST: the same fields, `DELETE` as query parameters): `dryRun` returns what the write would do and writes nothing; `mode: commit` writes the manifests as files into `agent-manager/` under the directory of the Flux Kustomization that owns the namespace (gitops-commit's `layout`; `repository`, `branch`, `path` name the target where nothing is GitOps-owned yet) and opens the pull request as the caller, returning `commit` with the repository, files, branch and pull request. Commit mode needs the GitHub App pin: `--github-authorization-server`/`--github-api-url`, chart `github.enabled` (the MCPServer pinned to the App `giantswarm-agent-manager` with `forwardIdentity: true`; the person's App user token on the MCP call, verified with `GET /user`, the ID token in `X-Muster-Id-Token`). `get_info` reports `capabilities.commit`; new error codes `gitops_owned` (409) and `auth_required` (401). ([#24](https://github.com/giantswarm/agent-manager/issues/24))

### Changed

- A HelmRelease applied from git (`managed: gitops`) is never written live: `update_agent` and `delete_agent` in mode `apply` answer `gitops_owned` naming `mode: commit` and the target, whether `force` is passed or not; `force` keeps overriding a suspended release and deletes a bare AgentTemplate. ([#24](https://github.com/giantswarm/agent-manager/issues/24))

## [1.4.0] - 2026-09-28

### Added

- Prometheus metrics on `GET /metrics` of a separate `metrics` port (`9464`): `mcp_server_operation_duration_seconds` for every `tools/call`, by `gen_ai_tool_name` and, on failure, `error_type` (`tool_error` for an `isError` result, `_OTHER` for a handler error), and `http_server_request_duration_seconds` for the REST API and the MCP endpoint. Chart: `observability.metrics.enabled` (default `true`) and `.port`; off renders `OTEL_METRICS_EXPORTER=none`, so an OTLP endpoint set for traces pushes no metrics. A ServiceMonitor gated on `serviceMonitor.enabled`, with `serviceMonitor.labels` and `.interval`, and `networkPolicy.metricsNamespaces` (default `kube-system`) admitted to the metrics port. ([giantswarm/giantswarm#36711](https://github.com/giantswarm/giantswarm/issues/36711))

## [1.3.0] - 2026-09-28

### Added

- `harness` on `create_agent` and `validate_agent`: the Harness of the namespace that runs the agent (chart `agent.harness`), such as `claude` for a Claude Code coding agent; omitted, the platform Harness. A name no Harness of the namespace admits by `agent-platform.giantswarm.io/harness` is refused with the ones that do. Fixed at create.

## [1.2.2] - 2026-09-28

### Fixed

- `get_agent_status` and `list_agents` read the Harness the agent names in its `agent-platform.giantswarm.io/harness` label, falling back to the configured platform Harness: an agent on another Harness, such as a coding agent on `claude`, is reported Ready instead of failed.

## [1.2.0] - 2026-09-25

### Added

- OTLP trace export. With `OTEL_EXPORTER_OTLP_ENDPOINT` set every request but the probes is a server span named after its route, joined to an inbound `traceparent`; an MCP request is an `mcp.<method>` span under it, a tool call's `mcp.tools/call` span carries `gen_ai.tool.name` and `mcp.tool.name`, with a `tool.<name>` span around the handler. Chart: `observability.otel.{endpoint,protocol,headers,resourceAttributes,sampler,samplerArg}` render the `OTEL_*` environment (parent-based `0.1` sampling by default, the downward-API `k8s.*` resource attributes); with `networkPolicy.enabled` the policy opens egress to the collector. ([giantswarm/giantswarm#36711](https://github.com/giantswarm/giantswarm/issues/36711))

## [1.1.21] - 2026-09-25

### Changed

- Create and update refuse a `systemMessage` longer than 20000 characters (counted in characters, not bytes) with a 400 that names the field, the limit and the actual length. The agent chart caps `agent.systemMessage` at the same length, because the compiled agent config has to fit Substrate's 32768-character env value limit; a longer prompt used to be accepted and the agent never became Ready.

## [1.1.13] - 2026-09-23

### Fixed

- Chart: the `helm.sh/chart` label is a valid label value for any chart version. The 63-character cut of a long development version could end in a run like `--.`, and trimming each of `-`, `.` and `_` once still left a trailing `-`, so the API server refused every labelled object; the whole run of non-alphanumeric characters at the ends of the cut is now trimmed, and the chart-label unittest suite checks the label for such versions. (#58)

## [1.1.11] - 2026-09-16

### Changed

- An untagged local build reports the toolchain's pseudo-version (`1.1.9-0.20260916142110-aef0725928df`: the next patch and the commit) instead of `dev`, as the other Agent Platform managers, muster and agentlab do; `dev` remains for a build without version-control information.

## [1.1.10] - 2026-09-16

### Fixed

- The MCP tool annotations now spell out all four hints. mcp-go pre-fills an unset hint with the spec default, so a hint the server never set was advertised rather than omitted: every tool claimed `destructiveHint: true`, `create_agent` included, although it only ever adds (it refuses a name that already exists). Reads are now `readOnly: true, destructive: false`; `create_agent` is `destructive: false`; `update_agent` is `destructive: true` (it overwrites values in place) and not idempotent (`refreshSkills`, and a skill given as a ref, re-resolve on every call); `delete_agent` keeps `destructive: true`. `openWorldHint` is true on the tools that resolve a caller-named GitHub repository or OCI reference (`list_skills`, `validate_agent`, `create_agent`, `update_agent`) and false on the ones confined to the installation's own resources.

## [1.1.9] - 2026-09-16

### Fixed

- The released image reported `version=dev` (start-up log, `agent-manager version`, `get_info`): the generated release pipeline passes no `-ldflags -X`. The version, commit and build time are now resolved from the Go build info when the build left them at their defaults — the tag at HEAD (without `v`), the short `vcs.revision` (`-dirty` for a modified tree) and `vcs.time`; an untagged local build still says `dev`. The start-up log names the commit next to the version. (#53)

## [1.1.6] - 2026-09-15

### Changed

- The chart README no longer renders a version badge (`chart.badgesSection` removed from `README.md.gotmpl`): a release PR bumping `Chart.yaml`'s `version` no longer changes the checked-in `README.md`, so the helm-docs pre-commit hook no longer fails on it. ([giantswarm/devctl#2180](https://github.com/giantswarm/devctl/issues/2180))

## [1.1.5] - 2026-09-15

### Fixed

- `update_agent`: a HelmRelease write that lands on a stale `resourceVersion` — helm-controller writes the release's status while it reconciles the previous change, so an update following another closely raced it and failed with `conflict: … the object has been modified` — is retried on a fresh read of the release, merging into its latest values; a Conflict that outlasts the attempts is still reported. `migrate` retries its HelmRelease and OCIRepository writes the same way and leaves a release whose values changed underneath to the next run. (#47)

## [1.1.0] - 2026-09-11

### Added

- `agent-manager migrate`: the expand–contract migration of an installation's agents to Generic chart 1.x (kagent API v2) — every Generic-chart HelmRelease rendering into a managed namespace gets 1.x values (removed keys dropped, `muster.toolNames` → `muster.tools`, every skill pinned to a commit or a digest, validated against the 1.x schema before anything is written), GitOps-owned and external releases are never written but their rewrite is emitted as a diff, the namespace's `agent` OCIRepository moves to `1.x` last; once every release is on a 1.x chart and every AgentTemplate is Ready on the platform Harness, the leftover `kagent.dev/v1alpha2` Agent objects and the five removed CRDs are deleted. A report ConfigMap `agent-manager-migrate-report` per managed namespace (`phase`, `summary`, `report.yaml`); `--dry-run` writes nothing. Runs as the connectivity chart's Job and by hand. (#38)

## [0.4.0] - 2026-09-07

### Added

- `toolset` (list of selectors) on `create_agent` (required), `validate_agent` and `update_agent` (replaces the whole list), composed as the agent chart's top-level `toolset` value; validated against the inline grammar (`preset:` | `server:` | `workflow:` | `tool:`, at most 32, non-empty, `toolset:` reserved, `label:` preset-only). `get_agent` / `list_agents` report `toolset`, or `implicitFullAccess: true` for an agent without one. (#27)

### Removed

- `toolNames` on the three tools and the REST surface: it never narrowed anything against muster (kagent filters muster's meta-tools only). A request still carrying it is refused with that explanation and pointed at `toolset`. `muster.toolNames` is never composed. (#27)

## [0.3.0] - 2026-09-07

### Removed

- Chart: the MCPServer CR carries `agent-platform.giantswarm.io/tool-group: agent-platform` next to `muster.giantswarm.io/type` — the Agent Platform tier of MCP servers (the portal's MCP servers page, the `agent-platform` toolset preset). `muster.mcpServer.labels` still adds or overrides labels. (#23)

[Unreleased]: https://github.com/giantswarm/agent-manager/compare/v2.16.0...HEAD
[2.16.0]: https://github.com/giantswarm/agent-manager/compare/v2.16.0-rc.1...v2.16.0
[2.16.0-rc.1]: https://github.com/giantswarm/agent-manager/compare/v2.15.0...v2.16.0-rc.1
[2.15.0]: https://github.com/giantswarm/agent-manager/compare/v2.14.0...v2.15.0
[2.14.0]: https://github.com/giantswarm/agent-manager/compare/v2.13.1...v2.14.0
[2.13.1]: https://github.com/giantswarm/agent-manager/compare/v2.13.0...v2.13.1
[2.13.0]: https://github.com/giantswarm/agent-manager/compare/v2.12.0-rc.1...v2.13.0
[2.12.0-rc.1]: https://github.com/giantswarm/agent-manager/compare/v2.11.0...v2.12.0-rc.1
[2.11.0]: https://github.com/giantswarm/agent-manager/compare/v2.11.0-rc.2...v2.11.0
[2.11.0-rc.2]: https://github.com/giantswarm/agent-manager/compare/v2.11.0-rc.1...v2.11.0-rc.2
[2.11.0-rc.1]: https://github.com/giantswarm/agent-manager/compare/v2.10.0...v2.11.0-rc.1
[2.10.0]: https://github.com/giantswarm/agent-manager/compare/v2.9.1...v2.10.0
[2.9.1]: https://github.com/giantswarm/agent-manager/compare/v2.9.0...v2.9.1
[2.9.0]: https://github.com/giantswarm/agent-manager/compare/v2.8.0-rc.1...v2.9.0
[2.8.0-rc.1]: https://github.com/giantswarm/agent-manager/compare/v2.8.0-rc.1...v2.8.0-rc.1
[2.8.0-rc.1]: https://github.com/giantswarm/agent-manager/compare/v2.7.2...v2.8.0-rc.1
[2.7.2]: https://github.com/giantswarm/agent-manager/compare/v2.7.1...v2.7.2
[2.7.1]: https://github.com/giantswarm/agent-manager/compare/v2.7.0-rc.1...v2.7.1
[2.7.0-rc.1]: https://github.com/giantswarm/agent-manager/compare/v2.6.0...v2.7.0-rc.1
[2.6.0]: https://github.com/giantswarm/agent-manager/compare/v2.6.0-rc.1...v2.6.0
[2.6.0-rc.1]: https://github.com/giantswarm/agent-manager/compare/v2.5.1-rc.1...v2.6.0-rc.1
[2.5.1-rc.1]: https://github.com/giantswarm/agent-manager/compare/v2.5.0...v2.5.1-rc.1
[2.5.0]: https://github.com/giantswarm/agent-manager/compare/v2.5.0-rc.4...v2.5.0
[2.5.0-rc.4]: https://github.com/giantswarm/agent-manager/compare/v2.5.0-rc.2...v2.5.0-rc.4
[2.5.0-rc.2]: https://github.com/giantswarm/agent-manager/compare/v2.5.0-rc.1...v2.5.0-rc.2
[2.5.0-rc.1]: https://github.com/giantswarm/agent-manager/compare/v2.4.1-rc.2...v2.5.0-rc.1
[2.4.1-rc.2]: https://github.com/giantswarm/agent-manager/compare/v2.4.0...v2.4.1-rc.2
[2.4.0]: https://github.com/giantswarm/agent-manager/compare/v2.3.1...v2.4.0
[2.3.1]: https://github.com/giantswarm/agent-manager/compare/v2.3.1-rc.1...v2.3.1
[2.3.1-rc.1]: https://github.com/giantswarm/agent-manager/compare/v2.3.0...v2.3.1-rc.1
[2.3.0]: https://github.com/giantswarm/agent-manager/compare/v2.2.1-rc.1...v2.3.0
[2.2.1-rc.1]: https://github.com/giantswarm/agent-manager/compare/v2.2.0...v2.2.1-rc.1
[2.2.0]: https://github.com/giantswarm/agent-manager/compare/v2.2.0-rc.1...v2.2.0
[2.2.0-rc.1]: https://github.com/giantswarm/agent-manager/compare/v2.1.0...v2.2.0-rc.1
[2.1.0]: https://github.com/giantswarm/agent-manager/compare/v2.1.0-rc.2...v2.1.0
[2.1.0-rc.2]: https://github.com/giantswarm/agent-manager/compare/v2.1.0-rc.1...v2.1.0-rc.2
[2.1.0-rc.1]: https://github.com/giantswarm/agent-manager/compare/v2.0.1...v2.1.0-rc.1
[2.0.1]: https://github.com/giantswarm/agent-manager/compare/v2.0.0...v2.0.1
[2.0.0]: https://github.com/giantswarm/agent-manager/compare/v2.0.0-rc.1...v2.0.0
[2.0.0-rc.1]: https://github.com/giantswarm/agent-manager/compare/v1.14.0...v2.0.0-rc.1
[1.14.0]: https://github.com/giantswarm/agent-manager/compare/v1.13.0...v1.14.0
[1.13.0]: https://github.com/giantswarm/agent-manager/compare/v1.12.1-rc.1...v1.13.0
[1.12.1-rc.1]: https://github.com/giantswarm/agent-manager/compare/v1.12.0...v1.12.1-rc.1
[1.12.0]: https://github.com/giantswarm/agent-manager/compare/v1.11.1...v1.12.0
[1.11.1]: https://github.com/giantswarm/agent-manager/compare/v1.11.0...v1.11.1
[1.11.0]: https://github.com/giantswarm/agent-manager/compare/v1.10.1...v1.11.0
[1.10.1]: https://github.com/giantswarm/agent-manager/compare/v1.10.0...v1.10.1
[1.10.0]: https://github.com/giantswarm/agent-manager/compare/v1.10.0-rc.1...v1.10.0
[1.10.0-rc.1]: https://github.com/giantswarm/agent-manager/compare/v1.9.3-rc.5...v1.10.0-rc.1
[1.9.3-rc.5]: https://github.com/giantswarm/agent-manager/compare/v1.9.3-rc.4...v1.9.3-rc.5
[1.9.3-rc.4]: https://github.com/giantswarm/agent-manager/compare/v1.9.2...v1.9.3-rc.4
[1.9.2]: https://github.com/giantswarm/agent-manager/compare/v1.9.1...v1.9.2
[1.9.1]: https://github.com/giantswarm/agent-manager/compare/v1.9.1-rc.2...v1.9.1
[1.9.1-rc.2]: https://github.com/giantswarm/agent-manager/compare/v1.9.0...v1.9.1-rc.2
[1.9.0]: https://github.com/giantswarm/agent-manager/compare/v1.8.0...v1.9.0
[1.8.0]: https://github.com/giantswarm/agent-manager/compare/v1.7.0...v1.8.0
[1.7.0]: https://github.com/giantswarm/agent-manager/compare/v1.6.2...v1.7.0
[1.6.2]: https://github.com/giantswarm/agent-manager/compare/v1.6.0...v1.6.2
[1.6.0]: https://github.com/giantswarm/agent-manager/compare/v1.5.0...v1.6.0
[1.5.0]: https://github.com/giantswarm/agent-manager/compare/v1.4.0...v1.5.0
[1.4.0]: https://github.com/giantswarm/agent-manager/compare/v1.3.0...v1.4.0
[1.3.0]: https://github.com/giantswarm/agent-manager/compare/v1.2.2...v1.3.0
[1.2.2]: https://github.com/giantswarm/agent-manager/compare/v1.2.0...v1.2.2
[1.2.0]: https://github.com/giantswarm/agent-manager/compare/v1.1.21...v1.2.0
[1.1.21]: https://github.com/giantswarm/agent-manager/compare/v1.1.13...v1.1.21
[1.1.13]: https://github.com/giantswarm/agent-manager/compare/v1.1.11...v1.1.13
[1.1.11]: https://github.com/giantswarm/agent-manager/compare/v1.1.10...v1.1.11
[1.1.10]: https://github.com/giantswarm/agent-manager/compare/v1.1.9...v1.1.10
[1.1.9]: https://github.com/giantswarm/agent-manager/compare/v1.1.6...v1.1.9
[1.1.6]: https://github.com/giantswarm/agent-manager/compare/v1.1.5...v1.1.6
[1.1.5]: https://github.com/giantswarm/agent-manager/compare/v1.1.0...v1.1.5
[1.1.0]: https://github.com/giantswarm/agent-manager/compare/v0.4.0...v1.1.0
[0.4.0]: https://github.com/giantswarm/agent-manager/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/giantswarm/agent-manager/releases/tag/v0.3.0
