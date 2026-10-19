# The tools

Generated from the registered tools (`make tools-doc`); through muster every tool is `x_agent-manager_<name>`.

| Tool | Kind |
|---|---|
| `create_agent` | write |
| `delete_agent` | write, destructive |
| `delete_session` | write, destructive |
| `get_agent` | read-only |
| `get_agent_status` | read-only |
| `get_info` | read-only |
| `get_session` | read-only |
| `get_session_turn` | read-only |
| `list_agents` | read-only |
| `list_model_configs` | read-only |
| `list_sessions` | read-only |
| `list_skills` | read-only |
| `start_session` | write |
| `update_agent` | write, destructive |
| `validate_agent` | read-only |

## `create_agent`

```text
Create an agent: a HelmRelease of the Generic agent chart. It needs a toolset; confirm its name with the person.

name is the DNS-1123 name the person chose, never derived from displayName. modelConfig must exist in the namespace (list_model_configs; a refusal names the valid ones). Skills and plugins are pinned at write time (list_skills) and the values are checked against the chart schema; a failure writes nothing. Presets: preset:read-only, preset:none, preset:infrastructure, preset:agent-platform, preset:full. Poll get_agent_status until ready.
```

```json
{
  "properties": {
    "annotations": {
      "additionalProperties": {
        "type": "string"
      },
      "description": "Extra annotations on the Agent object (string values).",
      "properties": {},
      "type": "object"
    },
    "branch": {
      "description": "mode commit with repository: the base branch the pull request goes to.",
      "type": "string"
    },
    "cluster": {
      "description": "With organization: the workload cluster the agent runs on; its HelmRelease stays on the installation. namespace is on the cluster.",
      "type": "string"
    },
    "description": {
      "description": "What the agent is for",
      "type": "string"
    },
    "displayName": {
      "description": "Friendly Unicode name (max 63 chars), shown by the portal",
      "type": "string"
    },
    "dryRun": {
      "description": "Return what the write would do, the manifests and with mode commit the pull request's files, and write nothing (default false).",
      "type": "boolean"
    },
    "egress": {
      "description": "Extra HTTP(S) origins the agent may reach: <scheme>://<host>[:<port>] origins, at most 64; a wildcard replaces the leftmost label.",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "gitAuthSecretName": {
      "description": "Secret in the agent's namespace whose token key reads private git skills and plugins; omit for the installation's (get_info)",
      "type": "string"
    },
    "harness": {
      "description": "Harness that runs the agent; omit for the platform Harness. A coding Harness (claude, codex, …) fixes egress, refuses extra bindings.",
      "type": "string"
    },
    "iconUrl": {
      "description": "Avatar URL (chart agent.iconUrl, rendered as the ui.giantswarm.io/icon-url annotation); omit unless the installation serves avatars",
      "type": "string"
    },
    "labels": {
      "additionalProperties": {
        "type": "string"
      },
      "description": "Extra labels on the Agent object (string values).",
      "properties": {},
      "type": "object"
    },
    "mode": {
      "description": "apply (default): write live as you; a release applied from git is refused with gitops_owned. commit: open a pull request as you.",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "modelConfig": {
      "description": "Name of an existing kagent ModelConfig in the namespace (list_model_configs)",
      "type": "string"
    },
    "name": {
      "description": "DNS-1123 technical name (max 63 chars): the HelmRelease and Agent name",
      "type": "string"
    },
    "namespace": {
      "description": "Namespace of the agent; default: the installation's kagent namespace (get_info lists the managed ones).",
      "type": "string"
    },
    "organization": {
      "description": "With cluster: the organization owning the workload cluster the agent runs on. Omit both for the installation's own cluster.",
      "type": "string"
    },
    "path": {
      "description": "mode commit with repository: the directory a Flux Kustomization builds from; files go into its agent-manager/ subdirectory.",
      "type": "string"
    },
    "plugins": {
      "description": "Plugins the agent loads: {git: {url, ref | commit} or oci: <reference>, path, skills} entries, pinned at write time.",
      "items": {
        "properties": {
          "git": {
            "properties": {
              "commit": {
                "description": "Full commit id (40 or 64 hex characters) to pin",
                "type": "string"
              },
              "ref": {
                "description": "Branch or tag to resolve to its head commit at write time; omit for the default branch",
                "type": "string"
              },
              "url": {
                "description": "Repository URL (http or https)",
                "type": "string"
              }
            },
            "required": [
              "url"
            ],
            "type": "object"
          },
          "oci": {
            "description": "OCI reference: <registry>/<repository>:<tag> (resolved to its digest) or <registry>/<repository>@sha256:<digest>",
            "type": "string"
          },
          "path": {
            "description": "Directory of the plugin within the artifact, relative without '..'; empty for the root",
            "type": "string"
          },
          "skills": {
            "description": "Skills to enable from an Agent Plugins format package, unique, never a name skills or another plugin uses; omit for a Claude Code format plugin",
            "items": {
              "type": "string"
            },
            "type": "array"
          }
        },
        "type": "object"
      },
      "type": "array"
    },
    "repository": {
      "description": "mode commit: the repository (owner/name) to commit to where no Flux Kustomization owns the namespace yet. Needs branch.",
      "type": "string"
    },
    "skills": {
      "description": "Skills to mount: {name, path, git: {url, ref | commit}} or {name, oci} entries, pinned at write time.",
      "items": {
        "properties": {
          "git": {
            "properties": {
              "commit": {
                "description": "Full commit id (40 or 64 hex characters) to pin; list_skills reports it",
                "type": "string"
              },
              "ref": {
                "description": "Branch or tag to resolve to its head commit at write time; omit for the default branch",
                "type": "string"
              },
              "url": {
                "description": "Repository URL (http or https)",
                "type": "string"
              }
            },
            "required": [
              "url"
            ],
            "type": "object"
          },
          "name": {
            "description": "Name the skill mounts under; defaults to the last path segment, else the repository or image name",
            "type": "string"
          },
          "oci": {
            "description": "OCI reference: <registry>/<repository>:<tag> (resolved to its digest) or <registry>/<repository>@sha256:<digest>",
            "type": "string"
          },
          "path": {
            "description": "Skill directory (holds SKILL.md), relative without '..'; empty for the repository root (git skills only)",
            "type": "string"
          }
        },
        "type": "object"
      },
      "type": "array"
    },
    "systemMessage": {
      "description": "System prompt; omit for the chart's default prompt. At most 20000 characters; put long reference material in a skill.",
      "type": "string"
    },
    "toolset": {
      "description": "Gateway tools the agent may use: preset:<name>, server:<name>, workflow:<name> or tool:<name> selectors, at most 32; preset:none means no tools.",
      "items": {
        "type": "string"
      },
      "type": "array"
    }
  },
  "required": [
    "name",
    "modelConfig",
    "toolset"
  ],
  "type": "object"
}
```

## `delete_agent`

```text
Delete an agent: its HelmRelease, which removes the Agent and its muster RemoteMCPServer.

The chart's shared OCIRepository goes too when nothing else in the namespace uses it; the answer says why it was kept. A bare Agent or a suspended release is refused unless force is true; a GitOps-owned one needs mode commit.
```

```json
{
  "properties": {
    "branch": {
      "description": "mode commit with repository: the base branch the pull request goes to.",
      "type": "string"
    },
    "cluster": {
      "description": "With organization: the workload cluster the agent runs on; its HelmRelease stays on the installation. namespace is on the cluster.",
      "type": "string"
    },
    "dryRun": {
      "description": "Return what the write would do, the manifests and with mode commit the pull request's files, and write nothing (default false).",
      "type": "boolean"
    },
    "force": {
      "description": "Also delete bare Agent objects and suspended releases (default false). A GitOps-owned release is never deleted live: use mode commit",
      "type": "boolean"
    },
    "mode": {
      "description": "apply (default): write live as you; a release applied from git is refused with gitops_owned. commit: open a pull request as you.",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "name": {
      "description": "Agent name",
      "type": "string"
    },
    "namespace": {
      "description": "Namespace of the agent; default: the installation's kagent namespace (get_info lists the managed ones).",
      "type": "string"
    },
    "organization": {
      "description": "With cluster: the organization owning the workload cluster the agent runs on. Omit both for the installation's own cluster.",
      "type": "string"
    },
    "path": {
      "description": "mode commit with repository: the directory a Flux Kustomization builds from; files go into its agent-manager/ subdirectory.",
      "type": "string"
    },
    "repository": {
      "description": "mode commit: the repository (owner/name) to commit to where no Flux Kustomization owns the namespace yet. Needs branch.",
      "type": "string"
    }
  },
  "required": [
    "name"
  ],
  "type": "object"
}
```

## `delete_session`

```text
Delete one of your conversations and its state for good; it cannot be read back.

Another person's session is refused as not_found. While another operation on it runs, it is refused with conflict: retry shortly. A workspace session is unbound in muster; warning says when that failed.
```

```json
{
  "properties": {
    "sessionId": {
      "description": "Session id (a UUID, from start_session or list_sessions)",
      "type": "string"
    }
  },
  "required": [
    "sessionId"
  ],
  "type": "object"
}
```

## `get_agent`

```text
Get one agent: its HelmRelease values, Harness, pinned skills and plugins, toolset and Agent status.
```

```json
{
  "properties": {
    "cluster": {
      "description": "With organization: the workload cluster the agent runs on; its HelmRelease stays on the installation. namespace is on the cluster.",
      "type": "string"
    },
    "name": {
      "description": "Agent name",
      "type": "string"
    },
    "namespace": {
      "description": "Namespace of the agent; default: the installation's kagent namespace (get_info lists the managed ones).",
      "type": "string"
    },
    "organization": {
      "description": "With cluster: the organization owning the workload cluster the agent runs on. Omit both for the installation's own cluster.",
      "type": "string"
    }
  },
  "required": [
    "name"
  ],
  "type": "object"
}
```

## `get_agent_status`

```text
One readiness verdict for an agent: ready, progressing, failed or unknown, with a one-line summary.

From the Agent's conditions and warnings, the HelmRelease's conditions and history, and recent Warning events. Poll it after a create or update until ready. No Deployment or pod is involved: agents run as Substrate actors.
```

```json
{
  "properties": {
    "cluster": {
      "description": "With organization: the workload cluster the agent runs on; its HelmRelease stays on the installation. namespace is on the cluster.",
      "type": "string"
    },
    "name": {
      "description": "Agent name",
      "type": "string"
    },
    "namespace": {
      "description": "Namespace of the agent; default: the installation's kagent namespace (get_info lists the managed ones).",
      "type": "string"
    },
    "organization": {
      "description": "With cluster: the organization owning the workload cluster the agent runs on. Omit both for the installation's own cluster.",
      "type": "string"
    }
  },
  "required": [
    "name"
  ],
  "type": "object"
}
```

## `get_info`

```text
Where you can create agents and what this installation offers: managed namespaces, chart, Harness, capabilities.

Call it first: capabilities says whether this installation offers mode commit, the session tools and workload clusters. Also the service version, the served API versions, the muster URL and Flux settings composed into every agent, and how writes are authenticated.
```

```json
{
  "properties": {},
  "required": [],
  "type": "object"
}
```

## `get_session`

```text
Read a conversation's turns, oldest first. To poll, use lastTurns: 1 until the last turn's state is final.

Final states: COMPLETED, FAILED, CANCELED, REJECTED, INPUT_REQUIRED. detail: summary (default) gives per turn the messages, state and one stub per tool call; transcript every message and artifact, tool results as stubs; full the raw A2A tasks. The oldest turns are dropped to fit maxBytes (default 100000; omittedTurns counts them). One turn or tool call whole: get_session_turn.
```

```json
{
  "properties": {
    "detail": {
      "enum": [
        "summary",
        "transcript",
        "full"
      ],
      "type": "string"
    },
    "lastTurns": {
      "description": "Only the last n turns",
      "type": "number"
    },
    "maxBytes": {
      "description": "Max answer size in bytes, 4096-1048576, default 100000",
      "type": "number"
    },
    "sessionId": {
      "description": "Session id (from start_session or list_sessions)",
      "type": "string"
    }
  },
  "required": [
    "sessionId"
  ],
  "type": "object"
}
```

## `get_session_turn`

```text
Read one turn of a conversation, or with toolCall one tool call's arguments and full response.

detail: transcript (default) clips tool arguments and gives tool results as stubs (read one with toolCall); full gives the raw A2A objects. If the answer is cut to fit maxBytes, call again with fromMessage set to nextMessage.
```

```json
{
  "properties": {
    "detail": {
      "enum": [
        "transcript",
        "full"
      ],
      "type": "string"
    },
    "fromMessage": {
      "description": "First message index, default 0",
      "type": "number"
    },
    "maxBytes": {
      "description": "Max answer size in bytes, 4096-1048576, default 100000",
      "type": "number"
    },
    "sessionId": {
      "description": "Session id",
      "type": "string"
    },
    "toolCall": {
      "description": "Index or id of a tool call in this turn",
      "type": "string"
    },
    "turn": {
      "description": "taskId, index (from 0) or last",
      "type": "string"
    }
  },
  "required": [
    "sessionId",
    "turn"
  ],
  "type": "object"
}
```

## `list_agents`

```text
List the agents of a namespace, one summary each: model, Harness, toolset, readiness and how it is managed.

managed: helmrelease is writable here, gitops changes only with mode commit, none is a bare Agent object. implicitFullAccess: true means the agent sees every gateway tool and still needs a toolset (update_agent). An agent's prompt, skills, plugins and values come from get_agent.
```

```json
{
  "properties": {
    "cluster": {
      "description": "With organization: the workload cluster the agent runs on; its HelmRelease stays on the installation. namespace is on the cluster.",
      "type": "string"
    },
    "details": {
      "description": "Report every agent in full, as get_agent does (default false). Large: prefer get_agent for the agents you need.",
      "type": "boolean"
    },
    "namespace": {
      "description": "Namespace of the agent; default: the installation's kagent namespace (get_info lists the managed ones).",
      "type": "string"
    },
    "organization": {
      "description": "With cluster: the organization owning the workload cluster the agent runs on. Omit both for the installation's own cluster.",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `list_model_configs`

```text
List the ModelConfigs of a namespace: the values create_agent accepts for modelConfig.

Each with provider, model, its Accepted condition and who manages it; agent-manager never writes them. In a tenant namespace without agents yet, the platform default ModelConfig comes with the first agent.
```

```json
{
  "properties": {
    "cluster": {
      "description": "With organization: the workload cluster the agent runs on; its HelmRelease stays on the installation. namespace is on the cluster.",
      "type": "string"
    },
    "namespace": {
      "description": "Namespace of the agent; default: the installation's kagent namespace (get_info lists the managed ones).",
      "type": "string"
    },
    "organization": {
      "description": "With cluster: the organization owning the workload cluster the agent runs on. Omit both for the installation's own cluster.",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `list_sessions`

```text
List your conversations with one agent: id, name, state, failure and timestamps.

Read through agentgateway as you. allCreators includes every person's sessions, when the controller authorizes you for it.
```

```json
{
  "properties": {
    "allCreators": {
      "description": "Include every person's sessions of the agent, when the controller authorizes the caller for it (default false)",
      "type": "boolean"
    },
    "name": {
      "description": "Agent name",
      "type": "string"
    },
    "namespace": {
      "description": "Namespace of the agent; default: the installation's kagent namespace (get_info lists the managed ones).",
      "type": "string"
    }
  },
  "required": [
    "name"
  ],
  "type": "object"
}
```

## `list_skills`

```text
List the skills agents can mount, from the configured skill repositories, with each repository's head commit.

Per repository: the ref read, the commit it resolved to (what a skills entry pins) and each SKILL.md's name, description and path. Cached briefly; refresh re-reads GitHub.
```

```json
{
  "properties": {
    "ref": {
      "description": "Git ref to read; default: the default branch (the ref refreshSkills follows)",
      "type": "string"
    },
    "refresh": {
      "description": "Bypass the cache (default false)",
      "type": "boolean"
    },
    "repository": {
      "description": "Only this repository (https://github.com/<owner>/<repo>); default: every configured one",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

## `start_session`

```text
Start a conversation with an agent as you, optionally sending the first message. Read replies with get_session.

Refused with conflict while the agent has no ready revision (get_agent_status). With message it answers once the agent accepted the turn; the agent keeps working. Without it, send the first message yourself over A2A (tenant <namespace>/<name>, the session's contextId). The agent runs its tools as you. Pass a requestId and reuse it on a retry: it never starts a second turn.
```

```json
{
  "properties": {
    "message": {
      "description": "The first user message, text only, at most 32000 characters; omit it to send the first message yourself.",
      "type": "string"
    },
    "name": {
      "description": "Agent name",
      "type": "string"
    },
    "namespace": {
      "description": "Namespace of the agent; default: the installation's kagent namespace (get_info lists the managed ones).",
      "type": "string"
    },
    "requestId": {
      "description": "Idempotency key (at most 128 characters): pass one and reuse it on a retry; without it a retry starts a second session.",
      "type": "string"
    },
    "sessionName": {
      "description": "Display name of the conversation (at most 200 characters); default: unnamed",
      "type": "string"
    },
    "workspaceGrant": {
      "description": "The grant authorize_workspace returned to you: the session works in that workspace, its sandbox bound to you in muster. Others' are refused.",
      "type": "string"
    }
  },
  "required": [
    "name"
  ],
  "type": "object"
}
```

## `update_agent`

```text
Change an agent: only the fields passed change; a list, labels or annotations replaces the whole value.

Lists are skills, plugins, egress and toolset. "" clears a text field back to the chart default; gitAuthSecretName "" goes back to the installation's. Presets: preset:read-only, preset:none, preset:infrastructure, preset:agent-platform, preset:full. refreshSkills re-pins every git skill and git plugin. harness re-runs the create-time checks on the new Harness. Returns the values before and after. A suspended release is refused unless force is true; a GitOps-owned one needs mode commit.
```

```json
{
  "properties": {
    "annotations": {
      "additionalProperties": {
        "type": "string"
      },
      "description": "Replaces all extra annotations on the Agent ({} clears them; the creator annotation always stays); pass every one to keep.",
      "properties": {},
      "type": "object"
    },
    "branch": {
      "description": "mode commit with repository: the base branch the pull request goes to.",
      "type": "string"
    },
    "cluster": {
      "description": "With organization: the workload cluster the agent runs on; its HelmRelease stays on the installation. namespace is on the cluster.",
      "type": "string"
    },
    "description": {
      "description": "New description; \"\" clears it",
      "type": "string"
    },
    "displayName": {
      "description": "New friendly name; \"\" clears it",
      "type": "string"
    },
    "dryRun": {
      "description": "Return what the write would do, the manifests and with mode commit the pull request's files, and write nothing (default false).",
      "type": "boolean"
    },
    "egress": {
      "description": "Replaces the whole egress list ([] clears it): <scheme>://<host>[:<port>] origins, at most 64; a wildcard replaces the leftmost label.",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "force": {
      "description": "Write even when the HelmRelease is suspended (default false). A GitOps-owned release is never written live: use mode commit",
      "type": "boolean"
    },
    "gitAuthSecretName": {
      "description": "Secret in the agent's namespace whose token key reads private git skills and plugins; omit for the installation's (get_info); \"\" goes back to it",
      "type": "string"
    },
    "harness": {
      "description": "Move the agent to another Harness of the namespace; \"\" selects the platform Harness. Existing Sessions keep their runtime",
      "type": "string"
    },
    "iconUrl": {
      "description": "New avatar URL; \"\" clears it",
      "type": "string"
    },
    "labels": {
      "additionalProperties": {
        "type": "string"
      },
      "description": "Replaces all extra labels on the Agent object ({} clears them); pass every label to keep.",
      "properties": {},
      "type": "object"
    },
    "mode": {
      "description": "apply (default): write live as you; a release applied from git is refused with gitops_owned. commit: open a pull request as you.",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "modelConfig": {
      "description": "Name of an existing ModelConfig in the namespace",
      "type": "string"
    },
    "name": {
      "description": "Agent name",
      "type": "string"
    },
    "namespace": {
      "description": "Namespace of the agent; default: the installation's kagent namespace (get_info lists the managed ones).",
      "type": "string"
    },
    "organization": {
      "description": "With cluster: the organization owning the workload cluster the agent runs on. Omit both for the installation's own cluster.",
      "type": "string"
    },
    "path": {
      "description": "mode commit with repository: the directory a Flux Kustomization builds from; files go into its agent-manager/ subdirectory.",
      "type": "string"
    },
    "plugins": {
      "description": "Replaces the whole plugin list ([] clears it): {git: {url, ref | commit} or oci: <reference>, path, skills} entries, pinned at write time.",
      "items": {
        "properties": {
          "git": {
            "properties": {
              "commit": {
                "description": "Full commit id (40 or 64 hex characters) to pin",
                "type": "string"
              },
              "ref": {
                "description": "Branch or tag to resolve to its head commit at write time; omit for the default branch",
                "type": "string"
              },
              "url": {
                "description": "Repository URL (http or https)",
                "type": "string"
              }
            },
            "required": [
              "url"
            ],
            "type": "object"
          },
          "oci": {
            "description": "OCI reference: <registry>/<repository>:<tag> (resolved to its digest) or <registry>/<repository>@sha256:<digest>",
            "type": "string"
          },
          "path": {
            "description": "Directory of the plugin within the artifact, relative without '..'; empty for the root",
            "type": "string"
          },
          "skills": {
            "description": "Skills to enable from an Agent Plugins format package, unique, never a name skills or another plugin uses; omit for a Claude Code format plugin",
            "items": {
              "type": "string"
            },
            "type": "array"
          }
        },
        "type": "object"
      },
      "type": "array"
    },
    "refreshSkills": {
      "description": "Re-pin every git skill and git plugin to its default-branch head and change nothing else on the release (default false).",
      "type": "boolean"
    },
    "repository": {
      "description": "mode commit: the repository (owner/name) to commit to where no Flux Kustomization owns the namespace yet. Needs branch.",
      "type": "string"
    },
    "skills": {
      "description": "Replaces the whole skill list: {name, path, git: {url, ref | commit}} or {name, oci} entries, pinned at write time.",
      "items": {
        "properties": {
          "git": {
            "properties": {
              "commit": {
                "description": "Full commit id (40 or 64 hex characters) to pin; list_skills reports it",
                "type": "string"
              },
              "ref": {
                "description": "Branch or tag to resolve to its head commit at write time; omit for the default branch",
                "type": "string"
              },
              "url": {
                "description": "Repository URL (http or https)",
                "type": "string"
              }
            },
            "required": [
              "url"
            ],
            "type": "object"
          },
          "name": {
            "description": "Name the skill mounts under; defaults to the last path segment, else the repository or image name",
            "type": "string"
          },
          "oci": {
            "description": "OCI reference: <registry>/<repository>:<tag> (resolved to its digest) or <registry>/<repository>@sha256:<digest>",
            "type": "string"
          },
          "path": {
            "description": "Skill directory (holds SKILL.md), relative without '..'; empty for the repository root (git skills only)",
            "type": "string"
          }
        },
        "type": "object"
      },
      "type": "array"
    },
    "systemMessage": {
      "description": "New system prompt; \"\" restores the chart default. At most 20000 characters; put long reference material in a skill.",
      "type": "string"
    },
    "toolset": {
      "description": "Replaces the whole toolset: preset:<name>, server:<name>, workflow:<name> or tool:<name> selectors, at most 32; [] is refused: use preset:none.",
      "items": {
        "type": "string"
      },
      "type": "array"
    }
  },
  "required": [
    "name"
  ],
  "type": "object"
}
```

## `validate_agent`

```text
Dry run of create_agent, or of update_agent with update: true: the manifests and every violation. Writes nothing.

Checks the name, modelConfig, harness and toolset (required for a create), pins the skills and plugins and validates the values against the chart schema. mode answers as the write would. With update: true, lists, labels and annotations replace the whole value, as in update_agent.
```

```json
{
  "properties": {
    "annotations": {
      "additionalProperties": {
        "type": "string"
      },
      "description": "Extra annotations on the Agent object (string values).",
      "properties": {},
      "type": "object"
    },
    "cluster": {
      "description": "With organization: the workload cluster the agent runs on; its HelmRelease stays on the installation. namespace is on the cluster.",
      "type": "string"
    },
    "description": {
      "description": "Description",
      "type": "string"
    },
    "displayName": {
      "description": "Friendly name",
      "type": "string"
    },
    "egress": {
      "description": "Extra HTTP(S) origins the agent may reach: <scheme>://<host>[:<port>] origins, at most 64; a wildcard replaces the leftmost label.",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "force": {
      "description": "With update: validate a change to a suspended HelmRelease anyway (default false). A GitOps-owned release is never written live: use mode commit",
      "type": "boolean"
    },
    "gitAuthSecretName": {
      "description": "Secret in the agent's namespace whose token key reads private git skills and plugins; omit for the installation's (get_info)",
      "type": "string"
    },
    "harness": {
      "description": "Harness that runs the agent; omit for the platform Harness. A coding Harness (claude, codex, …) fixes egress, refuses extra bindings.",
      "type": "string"
    },
    "iconUrl": {
      "description": "Avatar URL",
      "type": "string"
    },
    "labels": {
      "additionalProperties": {
        "type": "string"
      },
      "description": "Extra labels on the Agent object (string values).",
      "properties": {},
      "type": "object"
    },
    "mode": {
      "description": "apply (default): write live as you; a release applied from git is refused with gitops_owned. commit: open a pull request as you.",
      "enum": [
        "apply",
        "commit"
      ],
      "type": "string"
    },
    "modelConfig": {
      "description": "ModelConfig name (required for a create)",
      "type": "string"
    },
    "name": {
      "description": "Agent name",
      "type": "string"
    },
    "namespace": {
      "description": "Namespace of the agent; default: the installation's kagent namespace (get_info lists the managed ones).",
      "type": "string"
    },
    "organization": {
      "description": "With cluster: the organization owning the workload cluster the agent runs on. Omit both for the installation's own cluster.",
      "type": "string"
    },
    "plugins": {
      "description": "Plugins the agent loads: {git: {url, ref | commit} or oci: <reference>, path, skills} entries, pinned at write time.",
      "items": {
        "properties": {
          "git": {
            "properties": {
              "commit": {
                "description": "Full commit id (40 or 64 hex characters) to pin",
                "type": "string"
              },
              "ref": {
                "description": "Branch or tag to resolve to its head commit at write time; omit for the default branch",
                "type": "string"
              },
              "url": {
                "description": "Repository URL (http or https)",
                "type": "string"
              }
            },
            "required": [
              "url"
            ],
            "type": "object"
          },
          "oci": {
            "description": "OCI reference: <registry>/<repository>:<tag> (resolved to its digest) or <registry>/<repository>@sha256:<digest>",
            "type": "string"
          },
          "path": {
            "description": "Directory of the plugin within the artifact, relative without '..'; empty for the root",
            "type": "string"
          },
          "skills": {
            "description": "Skills to enable from an Agent Plugins format package, unique, never a name skills or another plugin uses; omit for a Claude Code format plugin",
            "items": {
              "type": "string"
            },
            "type": "array"
          }
        },
        "type": "object"
      },
      "type": "array"
    },
    "refreshSkills": {
      "description": "Re-pin every git skill and git plugin to its default-branch head and change nothing else on the release (default false).",
      "type": "boolean"
    },
    "skills": {
      "description": "Skills to mount: {name, path, git: {url, ref | commit}} or {name, oci} entries, pinned at write time.",
      "items": {
        "properties": {
          "git": {
            "properties": {
              "commit": {
                "description": "Full commit id (40 or 64 hex characters) to pin; list_skills reports it",
                "type": "string"
              },
              "ref": {
                "description": "Branch or tag to resolve to its head commit at write time; omit for the default branch",
                "type": "string"
              },
              "url": {
                "description": "Repository URL (http or https)",
                "type": "string"
              }
            },
            "required": [
              "url"
            ],
            "type": "object"
          },
          "name": {
            "description": "Name the skill mounts under; defaults to the last path segment, else the repository or image name",
            "type": "string"
          },
          "oci": {
            "description": "OCI reference: <registry>/<repository>:<tag> (resolved to its digest) or <registry>/<repository>@sha256:<digest>",
            "type": "string"
          },
          "path": {
            "description": "Skill directory (holds SKILL.md), relative without '..'; empty for the repository root (git skills only)",
            "type": "string"
          }
        },
        "type": "object"
      },
      "type": "array"
    },
    "systemMessage": {
      "description": "System prompt. At most 20000 characters; put long reference material in a skill.",
      "type": "string"
    },
    "toolset": {
      "description": "Gateway tools the agent may use: preset:<name>, server:<name>, workflow:<name> or tool:<name> selectors, at most 32; preset:none means no tools.",
      "items": {
        "type": "string"
      },
      "type": "array"
    },
    "update": {
      "description": "Validate as an update of the existing agent instead of a create (default false)",
      "type": "boolean"
    }
  },
  "required": [
    "name"
  ],
  "type": "object"
}
```
