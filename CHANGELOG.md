# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- `agent-manager migrate`: the expand–contract migration of an installation's agents to Generic chart 1.x (kagent API v2) — every Generic-chart HelmRelease rendering into a managed namespace gets 1.x values (removed keys dropped, `muster.toolNames` → `muster.tools`, every skill pinned to a commit or a digest, validated against the 1.x schema before anything is written), GitOps-owned and external releases are never written but their rewrite is emitted as a diff, the namespace's `agent` OCIRepository moves to `1.x` last; once every release is on a 1.x chart and every AgentTemplate is Ready on the platform Harness, the leftover `kagent.dev/v1alpha2` Agent objects and the five removed CRDs are deleted. A report ConfigMap `agent-manager-migrate-report` per managed namespace (`phase`, `summary`, `report.yaml`); `--dry-run` writes nothing. Runs as the connectivity chart's Job and by hand. (#38)

- `toolset` (list of selectors) on `create_agent` (required), `validate_agent` and `update_agent` (replaces the whole list), composed as the agent chart's top-level `toolset` value; validated against the inline grammar (`preset:` | `server:` | `workflow:` | `tool:`, at most 32, non-empty, `toolset:` reserved, `label:` preset-only). `get_agent` / `list_agents` report `toolset`, or `implicitFullAccess: true` for an agent without one. (#27)

### Removed

- `toolNames` on the three tools and the REST surface: it never narrowed anything against muster (kagent filters muster's meta-tools only). A request still carrying it is refused with that explanation and pointed at `toolset`. `muster.toolNames` is never composed. (#27)

- Chart: the MCPServer CR carries `agent-platform.giantswarm.io/tool-group: agent-platform` next to `muster.giantswarm.io/type` — the Agent Platform tier of MCP servers (the portal's MCP servers page, the `agent-platform` toolset preset). `muster.mcpServer.labels` still adds or overrides labels. (#23)



[Unreleased]: https://github.com/giantswarm/agent-manager/tree/main
