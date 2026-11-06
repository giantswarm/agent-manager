# AGENTS.md

## Upstream placement

agent-manager builds on kagent, Substrate and the agent-platform charts. Before changing it to work around one of them, place the change by the first class
that holds: drop (nothing consumes it), upstream already has it (name the mechanism and its
configuration), platform (a Giant Swarm product choice, here or in another platform repository),
upstream issue (a generic gap that needs design), upstream pull request (a small generic fix),
fork-only (a named reason and a dated exit). The full rule is Team Bumblebee's *Upstream
placement* section, kept in its internal plans repository.

- Do not copy kagent, Substrate or chart rules into this repository. Validate by a server-side
  dry-run of the rendered objects, so the API server's schema, CEL rules and admission policies
  give the answer.
- A workaround for an upstream gap names the upstream issue or pull request that removes it.

## Platform skills

- The Agent Platform's skills in [giantswarm/agent-platform `skills/`](https://github.com/giantswarm/agent-platform/tree/main/skills) describe agent-manager's MCP tools. A change that alters what an agent or a person can do, see or configure through it comes with a pull request there updating the affected skills, linked from this one.
