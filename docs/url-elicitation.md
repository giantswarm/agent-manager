# URL elicitation through Muster

The manager relays URL interactions requested by Muster, including sign-in,
consent and credential setup. It preserves the producer's message and URL;
a URL elicitation alone does not identify an authentication challenge.

With a request-state key configured, a client declaring URL elicitation gets
an `input_required` result on MCP 2026-07-28, or JSON-RPC error `-32042` on
2025-11-25. Other clients get a tool error naming the URL. Without a key,
the existing tool-error behavior remains available.

A modern continuation is sealed for the validated issuer, subject, OAuth
client, original tool and arguments. Accept retries the same Muster call
with Muster's own state. It means consent to open the URL, not completion of
the browser interaction. Decline and cancel run nothing. Invalid state or a
sealing failure fails the call; it never falls back to an unsealed continuation.

Configure the chart's `requestStateSecret` under `oauth` (agent-manager) or
`auth` (toolchain-manager). `name` selects an existing Secret; `key` selects
its current key and `previousKey` optionally selects the previous key during
rotation. Each manager uses its own key. These mount the shared mcp-oauth
`OAUTH_REQUEST_STATE_KEY_FILE` and `OAUTH_REQUEST_STATE_PREVIOUS_KEY_FILE`
configuration. Keys contain 32 random bytes encoded as base64 or hex. Roll
all replicas after changing the Secret, retain the previous key for in-flight
continuations, then remove it after those continuations have expired.

The protocol contracts are [MCP multi round-trip requests](https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/mrtr)
and [URL elicitation](https://modelcontextprotocol.io/specification/2026-07-28/client/elicitation).

This relay does not replace endpoint authentication or per-operation
authorization. The existing private identity-forwarding paths need the
[per-backend access-token migration](https://github.com/giantswarm/giantswarm/issues/39102)
for standard MCP OAuth resource-server enforcement.
