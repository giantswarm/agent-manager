# Tool approval per agent

What agent-manager does for it. The concept (modes, floors, connector
settings) is in [Governing tool calls](https://github.com/giantswarm/agent-platform/blob/main/docs/tool-approval.md).

## The tool list

The agent chart's `tools`: selectors (`preset:`, `server:`, `workflow:`,
`tool:`), each optionally `Allow`, `Ask` or `Block`, plus an optional `default`
for entries without a setting.

```yaml
tools:
  - server:pagerduty
  - server:github: Ask
  - tool:x_github_merge_pull_request: Allow   # holds over GitHub's Ask only with a grant
  - tool:x_github_delete_repository: Block
default: Ask
```

- muster combines these with the connectors' settings (strictest wins). An
  `Allow` over a connector's `Ask` holds only with a grant (below).
- `tools`/`default` are written only where the chart the release gets takes
  them (on create: the namespace's agent chart source, per its
  `status.artifact.revision`; on update: the release runs such a chart or
  already has `tools`). Otherwise `toolset` is written and any setting is
  refused `unsupported`.
- Settings are refused `unsupported` while this installation does not bind
  them (`muster.toolApproval.enabled` off, or no bindings endpoint or sender
  token). The chart enables it by default.
- `toolset` stays accepted (a list without settings).
- `get_agent`/`list_agents` report `toolSettings`, `defaultApproval`, `grants`,
  `approvalSettings` and `approvalEnforcement`: `enforced`, `not-enforced`
  (no sender, no release, `spec.valuesFrom`, or an older chart without
  `tools`: sessions of an agent with settings are refused) or `pending` (the
  release has not run its current generation); `approvalEnforced` is the same
  as a boolean, omitted while pending.
- The chart also renders the settings as the header `X-Muster-Tool-Approval`
  on the agent's RemoteMCPServer, which muster applies to calls no Session
  binding covers. It carries no grants, so it only tightens.

## Which tool changes what

| Tool | Changes | Refuses |
|---|---|---|
| `create_agent` | everything | an `Allow` over a connector's `Ask`; `default: Allow` |
| `update_agent` | display name, tags, description, prompt, model, icon, labels, annotations | every other field, naming the tool that takes it |
| `update_agent_access` | `tools` (`toolset`), skills, plugins, egress, `egressPolicyRefs`, connectors, Harness, git credential, `refreshSkills`, default workspace, toolchains | a new or changed `Allow` over a connector's `Ask`; `default` |
| `set_agent_approval` | `settings` of listed entries, `default` | an unlisted entry; an `Allow` over `Ask` for a caller who is no connector admin; a grant in mode commit or on a release with `valuesFrom` |

- **Allow over Ask**: agent-manager asks muster's `filter_tools` with the
  agent's list (`agentTools`) as the caller, at `--muster-mcp-url`. A muster
  that cannot answer, or does not echo `agentTools`, refuses such a write with
  `approval_unavailable`. A request without an `Allow` never asks.
- **Connector admin**: a `SelfSubjectAccessReview` as the caller for `update`
  on `mcpservers.muster.giantswarm.io` in the connector's namespace. Only with
  downstream OAuth; otherwise nobody is one.
- The split tools take only their declared arguments, exactly as named.
- **Conflicts**: `get_agent`/`list_agents` report `generation`; pass it as
  `expectedGeneration` to `update_agent`, `update_agent_access`,
  `set_agent_approval` or REST `PATCH` and the write is refused `conflict` when
  the release moved since. `set_agent_approval` also answers `conflict` when
  the list changed under it.

## Approval grants

A grant is one connector admin's `Allow` over one connector's `Ask`, for one
entry (or the `default`) of one agent. Grants are agent-manager's own records,
never part of the agent's values, so a value written past agent-manager
(GitOps, a hand edit) cannot carry one.

- **Storage**: ConfigMap `approval-grants-<agent uid>` (key `grants.json`,
  versioned `approval.agent-manager.giantswarm.io/v1`) in a namespace that
  holds nothing else (`--approval-grants-namespace`, chart
  `muster.toolApproval.grantsNamespace`, default
  `<release name>-approval-grants`). Only agent-manager's ServiceAccount reaches
  it. Each grant records entry, server, connector UID, `grantedBy`, `grantedAt`,
  the release `generation` and the server's own `tools` covered when granted:
  a tool the connector starts asking for later is not covered.
- **Trust boundary**: RBAC. Whoever may write ConfigMaps in the grants
  namespace may lift any connector's `Ask` for any agent. A record whose name,
  labels and agent disagree, or that does not decode, is skipped and logged.
- **Reads**: `list_approval_grants` (optional `namespace`, `server`) and the
  `grants` of `get_agent`/`list_agents`. It answers `{grants, unreadable,
  unreadableCount}`: a namespace the caller may not list is named in
  `unreadable`, an organization the caller is no member of is skipped, other
  read failures are only counted.
- **Generation**: a grant holds only for the HelmRelease `metadata.generation`
  its save wrote. Any spec change (a chart bump, `suspend`) moves it and drops
  the grant at the next bind or save.
- **Saving**: every live save keeps a grant only while its entry is `Allow` and
  the saver is admin of its connector, and re-records it. A call muster marks
  as an agent's (`X-Muster-Caller-Kind: agent`) keeps none; on
  `set_agent_approval` it records only what it names, after muster asked the
  person. A save to git (mode commit) drops every grant; `delete_agent` drops
  the record. A recreated agent has a new uid and no grants.
- **Running Sessions**: after every save or drop of an agent with grant
  history, agent-manager sends `PATCH /session-bindings/grants` to muster with
  the grants to `keep`, removing the other marks from running Sessions. The
  record is marked `pendingSessionSync` until muster confirms (three tries
  with backoff); a mark left set is resent on the agent's next start, save or
  read and swept at agent-manager start (sender token alone). When muster was
  not told, the save result carries `sessionsUpdated: false` and a warning, and
  running Sessions keep their grants until the resend.

## Session bindings

With `--muster-approval-sender-token-file` (chart `muster.toolApproval.enabled`)
every bind of a Session carries the agent (`<namespace>/<name>`), `agentDisplayName`
(its `ui.giantswarm.io/display-name`, at most 80 characters), the settings,
`default` and live grant marks, sent with agent-manager's projected
ServiceAccount token (audience `muster`) in `X-Muster-Approval-Sender`, which
muster reviews. `delete_session` unbinds with the same token.

- **Which release**: the one rendering the agent, found by its Flux labels.
  Agents listed in `muster.toolApproval.platformReleases` (the default agent)
  are bound by that release only; the start is refused when their labels name
  another. Any other agent is bound by the release `create_agent` made
  (agent's namespace, agent's name); a label naming another release is refused.
  The release must render that very Agent. Reads use agent-manager's
  ServiceAccount, else the person starting the Session.
- **Release on another cluster**: `muster.toolApproval.platformAgents` carries
  the release's `tools`, `default` and `muster.requireApproval`, bound without
  grants. The start is refused when the Agent is unreadable, its labels name
  another release, or its RemoteMCPServer's `X-Muster-Tool-Approval` differs.
- **Refused** (`approval_unavailable`) rather than run without the agent's
  `Ask` and `Block`: the release is unreadable; a Flux-rendered agent has no
  release or one rendering another Agent; the release takes `spec.valuesFrom`
  with settings (only inline values are bound); this installation does not bind
  settings; muster did not take them (`X-Muster-Approval-Settings` not
  `bound`; the binding is removed again).
- A muster without per-agent settings refuses the bind's new fields: agents
  without settings still bind, agents with settings are refused.
- The bindings URL defaults to muster's in-cluster Service over http (a log
  warning for http outside the cluster). For https, trust comes from the
  system CAs and `--muster-bindings-ca-file` (chart `muster.bindingsCA`). The
  path shares muster's MCP port, so a NetworkPolicy cannot narrow it: muster's
  sender review is the boundary.

## Logs

Every line carries `agent`, `tool`, `savedBy`, `callerKind`.

| `msg` | Extra fields |
|---|---|
| `agent tools changed` | `before`, `after` |
| `agent access changed` | `field`, `before`, `after` |
| `approval grant recorded` | `entry`, `server`, `connectorUid`, `generation`, `tools`, `kept` |
| `approval grant dropped` | `entry`, `server`, `connectorUid`, `reason` |
| `approval grants removed from running sessions` | `kept`, `bindings`, `dropped` |
| `approval grants record skipped` (warn) | `configmap`, `reason` |
| `pending approval grant syncs swept` | `sent`, `failed` |

Skill and plugin URLs with credentials (`user:password@`) are refused; one
written before is redacted (`***@`) in `agent access changed`.
