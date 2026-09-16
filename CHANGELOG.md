# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- `agent-manager migrate`: the expand–contract migration of an installation's agents to Generic chart 1.x (kagent API v2) — every Generic-chart HelmRelease rendering into a managed namespace gets 1.x values (removed keys dropped, `muster.toolNames` → `muster.tools`, every skill pinned to a commit or a digest, validated against the 1.x schema before anything is written), GitOps-owned and external releases are never written but their rewrite is emitted as a diff, the namespace's `agent` OCIRepository moves to `1.x` last; once every release is on a 1.x chart and every AgentTemplate is Ready on the platform Harness, the leftover `kagent.dev/v1alpha2` Agent objects and the five removed CRDs are deleted. A report ConfigMap `agent-manager-migrate-report` per managed namespace (`phase`, `summary`, `report.yaml`); `--dry-run` writes nothing. Runs as the connectivity chart's Job and by hand. (#38)

- `toolset` (list of selectors) on `create_agent` (required), `validate_agent` and `update_agent` (replaces the whole list), composed as the agent chart's top-level `toolset` value; validated against the inline grammar (`preset:` | `server:` | `workflow:` | `tool:`, at most 32, non-empty, `toolset:` reserved, `label:` preset-only). `get_agent` / `list_agents` report `toolset`, or `implicitFullAccess: true` for an agent without one. (#27)

### Changed

- An untagged local build reports the toolchain's pseudo-version (`1.1.9-0.20260916142110-aef0725928df`: the next patch and the commit) instead of `dev`, as the other Agent Platform managers, muster and agentlab do; `dev` remains for a build without version-control information.
- The chart README no longer renders a version badge (`chart.badgesSection` removed from `README.md.gotmpl`): a release PR bumping `Chart.yaml`'s `version` no longer changes the checked-in `README.md`, so the helm-docs pre-commit hook no longer fails on it. ([giantswarm/devctl#2180](https://github.com/giantswarm/devctl/issues/2180))

### Fixed

- The MCP tool annotations now spell out all four hints. mcp-go pre-fills an unset hint with the spec default, so a hint the server never set was advertised rather than omitted: every tool claimed `destructiveHint: true`, `create_agent` included, although it only ever adds (it refuses a name that already exists). Reads are now `readOnly: true, destructive: false`; `create_agent` is `destructive: false`; `update_agent` is `destructive: true` (it overwrites values in place) and not idempotent (`refreshSkills`, and a skill given as a ref, re-resolve on every call); `delete_agent` keeps `destructive: true`. `openWorldHint` is true on the tools that resolve a caller-named GitHub repository or OCI reference (`list_skills`, `validate_agent`, `create_agent`, `update_agent`) and false on the ones confined to the installation's own resources.

- `update_agent`: a HelmRelease write that lands on a stale `resourceVersion` — helm-controller writes the release's status while it reconciles the previous change, so an update following another closely raced it and failed with `conflict: … the object has been modified` — is retried on a fresh read of the release, merging into its latest values; a Conflict that outlasts the attempts is still reported. `migrate` retries its HelmRelease and OCIRepository writes the same way and leaves a release whose values changed underneath to the next run. (#47)

### Fixed

- The released image reported `version=dev` (start-up log, `agent-manager version`, `get_info`): the generated release pipeline passes no `-ldflags -X`. The version, commit and build time are now resolved from the Go build info when the build left them at their defaults — the tag at HEAD (without `v`), the short `vcs.revision` (`-dirty` for a modified tree) and `vcs.time`; an untagged local build still says `dev`. The start-up log names the commit next to the version. (#53)

### Removed

- `toolNames` on the three tools and the REST surface: it never narrowed anything against muster (kagent filters muster's meta-tools only). A request still carrying it is refused with that explanation and pointed at `toolset`. `muster.toolNames` is never composed. (#27)

- Chart: the MCPServer CR carries `agent-platform.giantswarm.io/tool-group: agent-platform` next to `muster.giantswarm.io/type` — the Agent Platform tier of MCP servers (the portal's MCP servers page, the `agent-platform` toolset preset). `muster.mcpServer.labels` still adds or overrides labels. (#23)



[Unreleased]: https://github.com/giantswarm/agent-manager/tree/main
