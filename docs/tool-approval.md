# Tool approval per agent

This page says what agent-manager does. The concept (modes, floors, connector
settings) is explained once in the docs page *Governing tool calls*, for
further reading: <https://docs.giantswarm.io/overview/agent-platform/governing-tool-calls/>.

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
- agent-manager writes `tools`/`default` only when the chart the release gets
  takes them: on a create, the chart the namespace's agent chart source
  (OCIRepository) serves, as its `status.artifact.revision` says (an
  organization's source the operator created once may serve an older one:
  the agent then gets `toolset`, and settings are refused naming the source);
  otherwise the resolved agent chart's schema declares `tools` and, on an update, the release already runs that
  chart (its `status.history[0].chartVersion`) or already carries `tools`;
  otherwise it writes `toolset` and refuses any setting (`unsupported`, naming
  the chart version the release runs).
- Settings are refused (`unsupported`) while this installation does not bind
  them to sessions (`muster.toolApproval.enabled` off, no bindings endpoint or
  sender token): nothing would enforce them. The chart turns it on by default.
- `toolset` is still accepted (a tool list without settings). `get_agent` and
  `list_agents` report the list as **`toolSettings`**, with `defaultApproval`,
  `grants`, `approvalSettings` (true only where the chart takes settings and
  they are bound), `approvalEnforcement` (`enforced`; `not-enforced` without
  the sender, without a release, with `spec.valuesFrom`, or on a release
  without `tools` that runs an older chart, where a session of an agent with
  settings is refused; `pending` until the release ran its current
  generation) and `approvalEnforced` (the same as a boolean, left out while
  pending); `set_agent_approval` answers with the same
  names (`toolSettings`, `defaultApproval`, `before.toolSettings`); `tools` is unchanged: the template's kagent MCP
  bindings; `toolset` keeps the selectors for older clients.
- The agent chart also renders the settings as the static header
  `X-Muster-Tool-Approval` on the agent's RemoteMCPServer, which muster applies
  to calls of the agent that no Session binding carries settings for (a
  Session not started through `start_session`). It carries no grants, so it
  can only make tools stricter than the connectors' settings.

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

muster's answer must echo the list (`agentTools` in the `filter_tools`
result); one without it does not evaluate agent lists and is refused the same
way, so no `Allow` is taken as plain by mistake.

The **connector-admin check** is a `SelfSubjectAccessReview` as the caller for
`update` on `mcpservers.muster.giantswarm.io` in the connector's namespace,
per connector. It is answered only with downstream OAuth (agent-manager talks
to Kubernetes as the caller): otherwise nobody is a connector admin, however
agent-manager's ServiceAccount is bound.

The split tools take only the arguments they declare, exactly as named: an
argument in another case (`Toolset`, `EGRESS`) is refused, never bound.
`set_agent_approval` writes its settings only over the list it checked: when
another save changed the list meanwhile it answers `conflict` and writes
nothing. `get_agent` and `list_agents` report the release's `generation`;
`update_agent`, `update_agent_access`, `set_agent_approval` and REST `PATCH`
take it as `expectedGeneration` and refuse a write when the release moved
since it was read: `conflict: agent <ns>/<name> changed since it was read
(generation <N> now, expectedGeneration <M>): read it again with get_agent and
apply the change to what it holds now` (checked against the live release on
every attempt of the write; omitted, the write goes ahead).

## Approval grants

A grant is one connector admin's `Allow` over one connector's `Ask`, for one
entry of one agent (or its `default`). Grants are agent-manager's own
records, never part of the agent's values, labels or annotations, so a value
written past agent-manager (GitOps, a hand edit) can never carry one.

- **Storage**: one ConfigMap per agent, `approval-grants-<agent uid>`, in a
  namespace that holds nothing else (`--approval-grants-namespace`; chart
  `muster.toolApproval.grantsNamespace`, default
  `<release name>-approval-grants`, created by the chart), written and read
  with agent-manager's ServiceAccount only, whose Role there reaches nothing
  else. Labels `approval.agent-manager.giantswarm.io/grants: v1`,
  `.../agent-namespace`, `.../agent-name`; key `grants.json`:

  ```json
  {"apiVersion": "approval.agent-manager.giantswarm.io/v1", "kind": "ApprovalGrants",
   "agent": {"uid": "…", "namespace": "kagent", "name": "alert-review"},
   "grants": [{"entry": "tool:x_github_merge_pull_request", "server": "github",
               "namespace": "muster", "connectorUid": "…", "grantedBy": "dana@example.com",
               "grantedAt": "2026-11-26T10:00:00Z", "generation": 7,
               "tools": ["merge_pull_request"]}],
   "pendingSessionSync": false}
  ```

  `tools` are the server's own names of the tools the grant covered when it
  was given: muster lifts the connector's `Ask` for those only, so a tool the
  connector starts asking for later is not covered. `grantedBy` is the
  saver's email where the identity provider verified it, else the subject.
- **Trust boundary**: RBAC is the boundary. Whoever may write ConfigMaps in
  the grants namespace may lift any connector's `Ask` for any agent; keep
  that right with agent-manager's ServiceAccount. A record is checked on read
  (its object name, labels and the agent it names agree); one that does not,
  or does not decode, is skipped and logged, never trusted.
- **Boundary**: package `internal/approvalgrants` and its `Store` interface
  are the only way agent-manager code reaches grants; the MCP surface is
  `set_agent_approval` (writes), `list_approval_grants` and the `grants` of
  `get_agent`/`list_agents` (reads). `list_approval_grants` answers
  `{grants, unreadable, unreadableCount}`: with `namespace` that namespace
  (refused when the caller may not list its HelmReleases); without it every
  managed namespace, organization namespaces included, the caller may list
  them in; a configured namespace the caller may not list is named in
  `unreadable`, an organization the caller is no member of is skipped
  silently, and one that could not be read for another reason is only
  counted in `unreadableCount` (never named, as `toolchain_users` does); `server`
  narrows to one connector; each grant carries its `agent` and
  `agentDisplayName` (empty when the agent has none). A later owner (for example a dedicated
  connector manager) can take the records over by reading the same versioned
  documents, or by putting its own store behind the interface, without
  migrating agent values.
- **Generation**: a grant holds only for the HelmRelease `metadata.generation`
  the save that recorded it wrote. The generation moves with any change of the
  release's spec, not only its values (a chart version bump by automation,
  `suspend`), and a release whose generation moved past the grant loses it at
  the next bind or save. A grant at a higher generation than a Session start
  read (a save that ran meanwhile) is not bound by that start and never
  dropped by it.
- **Saving clears**: every live save (`update_agent`, `update_agent_access`,
  `set_agent_approval`, REST `PATCH`) keeps a grant only when its entry is
  still `Allow` and the saver is connector admin of its connector, and then
  re-records it with the saver and the new generation. A call muster marks as
  an agent's (`X-Muster-Caller-Kind: agent`) keeps none; on
  `set_agent_approval` it records only what the call names, after muster asked
  the person. A save to git (mode commit) drops every grant. `delete_agent`
  tells muster to drop every mark of the agent and then deletes the record. A
  recreated agent has a new uid and none.
- **Running Sessions**: every live save of an agent with a grant history, and
  every drop, is followed by `PATCH /session-bindings/grants` on muster with
  the grants left (`keep`, idempotent), which removes the other marks from the
  agent's running Sessions' bindings. The record is first marked
  `pendingSessionSync`; the call is tried three times with backoff; only
  muster's confirmation clears the mark. A mark left set is sent again on the
  agent's next start, save or read (`get_agent`, `list_agents`,
  `list_approval_grants`) and swept once when agent-manager starts, a deleted
  agent's empty record included, which is then removed. The sweep covers the
  organization namespaces too. A gone agent's record is sent with the grants
  of an agent recreated under the same name (another release UID), if any,
  so it never drops that agent's marks. Where no person's
  token is at hand (the sweep) the call goes with agent-manager's sender
  token alone, which muster accepts for grant drops. The result of the save (`update_agent`,
  `update_agent_access`, `set_agent_approval`, `delete_agent`) carries
  `sessionsUpdated: false` and a `warnings` entry when muster was not told;
  running Sessions then keep the grants they were bound with until the
  resend. A Session started during a save re-reads the grants after its bind
  and has muster drop a mark dropped meanwhile.

## Session bindings

With `--muster-approval-sender-token-file` (chart
`muster.toolApproval.enabled`), every bind of a Session that calls muster
itself carries the agent (`<namespace>/<name>`), the settings of its listed
entries, `default` and the marks of its live grants, sent with
agent-manager's projected ServiceAccount token (audience `muster`) in
`X-Muster-Approval-Sender`. muster takes them only after reviewing that token.
The release is read with agent-manager's ServiceAccount (the chart grants
`get` on HelmReleases in the configured namespaces under downstream OAuth);
where it may not (an organization namespace) the release is read as the
person starting the Session, and the start is refused only when neither may
read it. With
settings, the toolset bound is the release's own list, not the live Agent and
RemoteMCPServer, so editing those never widens what a grant covers.

A start is refused (`approval_unavailable`) rather than run without the
agent's `Ask` and `Block` when: the release cannot be read; an agent rendered
by a HelmRelease (label `helm.toolkit.fluxcd.io/name`) has none; the release
takes `spec.valuesFrom` and carries settings (agent-manager binds settings
from the inline values only); this installation does not bind settings; or
muster did not take them (its `X-Muster-Approval-Settings` answer is not
`bound`; the binding is removed again). The bindings URL defaults to muster's in-cluster Service over plain http on
the cluster network (no warning; one is logged for plain http to a host
outside the cluster). An https URL is trusted through the system's CAs and
`--muster-bindings-ca-file` (chart `muster.bindingsCA.configMapName`/`key`).
The session-bindings path shares muster's MCP port, which every agent
runtime must reach, so a NetworkPolicy cannot narrow it to agent-manager:
muster's sender review (TokenReview and the `bind toolapprovals` check) is
the boundary there.

Unbinding a Session (`delete_session`) sends the sender token too: muster
deletes a binding agent-manager wrote with settings only for it. A muster that predates per-agent
settings refuses the bind's `agent` and `approval` as unknown fields: an agent
without settings is then bound without them, one with settings is refused.
The bind body may be up to 64 KiB, matching muster.

## Logs

Every line carries `agent`, `tool`, `savedBy` and `callerKind`.

| `msg` | Extra fields |
|---|---|
| `agent tools changed` | `before`, `after` (`{tools, default}`) |
| `agent access changed` | `field`, `before`, `after` |
| `approval grant recorded` | `entry`, `server`, `connectorUid`, `generation`, `tools`, `kept` |
| `approval grant dropped` | `entry`, `server`, `connectorUid`, `reason` (also when a regrant leaves a connector out) |
| `approval grants removed from running sessions` | `kept`, `bindings` and `dropped` (muster's answer; `attempts`, `error` at warn level) |
| `approval grants record skipped` (warn) | `configmap`, `reason` |
| `pending approval grant syncs swept` | `sent`, `failed` (at start) |

Skill and plugin URLs carrying a credential (`user:password@`) are refused;
one written before is redacted (`***@`) in `agent access changed`.
