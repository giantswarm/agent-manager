# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- `toolset` (list of selectors) on `create_agent` (required), `validate_agent` and `update_agent` (replaces the whole list), composed as the agent chart's top-level `toolset` value; validated against the inline grammar (`preset:` | `server:` | `workflow:` | `tool:`, at most 32, non-empty, `toolset:` reserved, `label:` preset-only). `get_agent` / `list_agents` report `toolset`, or `implicitFullAccess: true` for an agent without one. (#27)

### Removed

- `toolNames` on the three tools and the REST surface: it never narrowed anything against muster (kagent filters muster's meta-tools only). A request still carrying it is refused with that explanation and pointed at `toolset`. `muster.toolNames` is never composed. (#27)

- Chart: the MCPServer CR carries `agent-platform.giantswarm.io/tool-group: agent-platform` next to `muster.giantswarm.io/type` — the Agent Platform tier of MCP servers (the portal's MCP servers page, the `agent-platform` toolset preset). `muster.mcpServer.labels` still adds or overrides labels. (#23)



[Unreleased]: https://github.com/giantswarm/agent-manager/tree/main
