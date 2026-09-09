# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Changed (POC: kagent main, `kagent.dev/v1alpha3`)

- Agents are composed as `kagent.dev/v1alpha3` `AgentTemplate`s plus, per declared toolset, a **toolset carrier** — a per-agent copy of the platform's `muster` `RemoteMCPServer` (`muster-<agent>`) carrying the `X-Muster-Toolset` header, which the template binds. The Flux `HelmRelease`/`OCIRepository` composition of the `agent` chart, the chart schema validation and the embedded schema are gone; the binary needs neither Flux CRDs nor the chart. Writes stay the caller's (field manager `agent-manager`).
- New `harness` argument on `create_agent`/`update_agent`/`validate_agent` (the kagent Harness that runs the agent; the template is labelled `kagent.dev/harness=<harness>`, default `--kagent-default-harness`); a Harness admitting the template must exist, checked against the Harness CRs the way kagent's controller pairs them.
- Readiness comes from `AgentTemplate.status.harnesses[]`: `get_agent_status` reports the per-Harness conditions, revisions and warnings; `ready` = any admitting Harness Ready for the current generation. `update_agent` says in `note` when the template changed (a new revision; running instances keep theirs); a toolset change rewrites the carrier only.
- `get_info`: `apiVersions.{agentTemplate,remoteMcpServer,harness,modelConfig}`, `kagent.{musterServer,defaultHarness,toolsetHeader}`, capability flags for what an AgentTemplate cannot express (`perToolSelection`, `runtime`, `iconUrl`, `skillGitAuthSecret`, `mutableSkillRefs`: false). `agent`, `helmRelease`, `ociRepository`, `chart` and `flux` are gone.
- Result fields: `created.{agentTemplate,toolsetCarrier}` (was `helmRelease`/`ociRepository`), `agentTemplateDeleted`/`toolsetCarrierDeleted`/`toolsetCarrierKept` (was `helmReleaseDeleted`/`ociRepositoryDeleted`/`ociRepositoryKept`), `manifests.{agentTemplate,toolsetCarrier}` (was `ociRepository`/`helmRelease`/`values`), `changed` paths without the `agent.` prefix (`description`, `toolset`, `skills.gitRefs`, …), `managed: agent-manager|gitops|external` (was `helmrelease|gitops|none`), `harnesses[]`, `toolsetCarrier`, `unmanagedFields`, `spec` on every agent.
- Skills pin immutable sources: `gitRefs[].ref` must be a full commit id, `refs[]` a digest-pinned OCI reference; `list_skills` resolves each branch to its `commit` and its `gitRefs` entries carry it as `ref`.
- `runtime`, `iconUrl` and `skills.gitAuthSecretName` are refused with the reason (not expressible on an AgentTemplate), like `toolNames`.
- Chart: `kagent.musterServer`, `kagent.defaultHarness`; `agentChart.*` and `flux.*` removed; the Role grants `agenttemplates`/`remotemcpservers` read-write and `harnesses`/`modelconfigs` read. The binary still parses the retired flags as deprecated no-ops, so the released chart can run this image unchanged.

### Added

- `toolset` (list of selectors) on `create_agent` (required), `validate_agent` and `update_agent` (replaces the whole list), composed as the agent chart's top-level `toolset` value; validated against the inline grammar (`preset:` | `server:` | `workflow:` | `tool:`, at most 32, non-empty, `toolset:` reserved, `label:` preset-only). `get_agent` / `list_agents` report `toolset`, or `implicitFullAccess: true` for an agent without one. (#27)

### Removed

- `toolNames` on the three tools and the REST surface: it never narrowed anything against muster (kagent filters muster's meta-tools only). A request still carrying it is refused with that explanation and pointed at `toolset`. `muster.toolNames` is never composed. (#27)

- Chart: the MCPServer CR carries `agent-platform.giantswarm.io/tool-group: agent-platform` next to `muster.giantswarm.io/type` — the Agent Platform tier of MCP servers (the portal's MCP servers page, the `agent-platform` toolset preset). `muster.mcpServer.labels` still adds or overrides labels. (#23)



[Unreleased]: https://github.com/giantswarm/agent-manager/tree/main
