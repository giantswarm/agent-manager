# Tool approval per agent

Slice 3 of [giantswarm/giantswarm#38902](https://github.com/giantswarm/giantswarm/issues/38902).
The concept is explained once, in the docs page *Governing tool calls*
(<https://docs.giantswarm.io/overview/agent-platform/governing-tool-calls/>);
this page says what agent-manager does.

## The tool list

An agent's tool list is the agent chart's `tools` value: toolset selectors
(`preset:`, `server:`, `workflow:`, `tool:`), each optionally with `Allow`,
`Ask` or `Block`, and an optional `default` for every listed tool without its
own setting.

```yaml
tools:
  - server:pagerduty
  - server:github: Ask
  - tool:x_github_merge_pull_request: Allow   # holds over GitHub's Ask only with a grant
  - tool:x_github_delete_repository: Block
default: Ask
```

- muster combines the settings with the connectors' own (strictest wins).
  An `Allow` over a connector's `Ask` holds only when a connector admin
  granted it (below); otherwise it changes nothing.
- agent-manager writes `tools`/`default` only when the resolved agent chart's
  schema declares `tools`; with an older chart it writes `toolset` and refuses
  any setting (`unsupported`).
- `toolset` is still accepted (a tool list without settings). `get_agent` and
  `list_agents` report `tools`, `defaultApproval`, `grants` and
  `approvalSettings`; `toolset` keeps the selectors for older clients, and the
  template's MCP bindings moved from `tools` to `toolBindings`.

## Which tool changes what

| Tool | Changes | Refuses |
|---|---|---|
| `create_agent` | everything | an `Allow` that selects a tool whose connector asks; `default: Allow` |
| `update_agent` | display name, tags, description, prompt, model, icon, labels, annotations | every other field, naming the tool that takes it |
| `update_agent_access` | `tools` (`toolset`), skills, plugins, egress, `egressPolicyRefs`, connectors, Harness, git credential, `refreshSkills`, default workspace and toolchains | a new or changed `Allow` over a connector's `Ask`; `default` |
| `set_agent_approval` | the settings of listed entries (`settings`) and `default` | an entry the agent does not list; an `Allow` over `Ask` the caller is no connector admin for; a grant in mode commit or on a release with `valuesFrom` |

Whether an `Allow` is over a connector's `Ask` is muster's to say: agent-manager
calls muster's `filter_tools` with the agent's list (`agentTools`) as the
caller, at `--muster-mcp-url`. A muster that cannot answer refuses such a
write with `approval_unavailable`. A request without an `Allow` never asks.

The **connector-admin check** is a `SelfSubjectAccessReview` as the caller for
`update` on `mcpservers.muster.giantswarm.io` in the connector's namespace,
per connector. It needs downstream OAuth: without it the review asks for
agent-manager's ServiceAccount, which holds no such right, so nobody is a
connector admin.

## Approval grants

A grant is one connector admin's `Allow` over one connector's `Ask`, for one
entry of one agent (or its `default`). Grants are agent-manager's own
records, never part of the agent's values, labels or annotations, so a value
written past agent-manager (GitOps, a hand edit) can never carry one.

- **Storage**: one ConfigMap per agent, `approval-grants-<agent uid>`, in
  agent-manager's namespace (`--approval-grants-namespace`), written and read
  with its ServiceAccount only. Labels
  `approval.agent-manager.giantswarm.io/grants: v1`,
  `.../agent-namespace`, `.../agent-name`; key `grants.json`:

  ```json
  {"apiVersion": "approval.agent-manager.giantswarm.io/v1", "kind": "ApprovalGrants",
   "agent": {"uid": "…", "namespace": "kagent", "name": "alert-review"},
   "grants": [{"entry": "tool:x_github_merge_pull_request", "server": "github",
               "namespace": "muster", "connectorUid": "…", "grantedBy": "dana@example.com",
               "grantedAt": "2026-11-26T10:00:00Z", "generation": 7}]}
  ```

- **Boundary**: package `internal/approvalgrants` and its `Store` interface
  are the only way agent-manager code reaches grants; the MCP surface is
  `set_agent_approval` (writes), `list_approval_grants` and the `grants` of
  `get_agent`/`list_agents` (reads). A later owner (a connector manager,
  giantswarm/bumblebee-plans#237) can take the records over by reading the
  same versioned documents, or by putting its own store behind the
  interface, without migrating agent values.
- **Generation**: a grant holds only for the HelmRelease `metadata.generation`
  the save that recorded it wrote. A release whose generation moved on (a
  write past agent-manager) loses it at the next bind or save.
- **Saving clears**: every live save (`update_agent`, `update_agent_access`,
  `set_agent_approval`, REST `PATCH`) keeps a grant only when its entry is
  still `Allow` and the saver is connector admin of its connector, and then
  re-records it with the saver and the new generation. A call muster marks as
  an agent's (`X-Muster-Caller-Kind: agent`) keeps none; on
  `set_agent_approval` it records only what the call names, after muster asked
  the person. A save to git (mode commit) drops every grant. `delete_agent`
  deletes the record. A recreated agent has a new uid and none.
- **Running Sessions**: every drop is followed by `PATCH
  /session-bindings/grants` on muster for the agent, which removes the dropped
  marks from its running Sessions' bindings. A Session started during a save
  re-reads the grants after its bind and removes a mark dropped meanwhile.

## Session bindings

With `--muster-approval-sender-token-file` (chart
`muster.toolApproval.enabled`), every bind of a Session that calls muster
itself carries the agent (`<namespace>/<name>`), the settings of its listed
entries, `default` and the marks of its live grants, sent with
agent-manager's projected ServiceAccount token (audience `muster`) in
`X-Muster-Approval-Sender`. muster takes them only after reviewing that token.
The release is read with agent-manager's ServiceAccount (the chart grants
`get` on HelmReleases in the managed namespaces under downstream OAuth); a
release that cannot be read refuses the start rather than binding the agent
without its `Ask` and `Block`.

## Logs

Every line carries `agent`, `tool`, `savedBy` and `callerKind`.

| `msg` | Extra fields |
|---|---|
| `agent tools changed` | `before`, `after` (`{tools, default}`) |
| `agent access changed` | `field`, `before`, `after` |
| `approval grant recorded` | `entry`, `server`, `connectorUid`, `generation`, `kept` |
| `approval grant dropped` | `entry`, `server`, `connectorUid`, `reason` |
| `approval grants removed from running sessions` | `kept` (and `error` at warn level) |
