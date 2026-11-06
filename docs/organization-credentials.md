# Organization credential operations

Models, connectors and EgressPolicies name credential Secrets by reference. The
organization tools accept no credential values. Missing credentials and explicit
`replaceCredential: true` prepare an operation before writing any consumer.
Ordinary edits with existing credentials do not prepare an operation or change
the Secret.

## Client-owned UI

Configure an HTTPS URL template, for example:

```yaml
credentials:
  uiURL: https://portal.example.com/organizations/graveler/{parent}/{organization}/credentials?operation={operation}
```

`{operation}` is required once in the path or query. `{parent}` and
`{organization}` are optional; the scheme and host are fixed. Routes and the
installation name belong to the UI configuration. Agent-manager neither hosts a
form nor constructs a Backstage route. The operation ID grants no authority.

The UI must use its existing authenticated backend path to agent-manager, keep
values out of browser storage, analytics, URL parameters and request logs, and
present the confirmation before saving. Agent-manager exposes no browser CORS
exception or token in the link. This API uses the installation's existing ID-token
authentication profile and Kubernetes downstream authorization.

1. Invoke the organization tool with nonsecret arguments. Optional `requestId`
   distinguishes repeated intentional changes with otherwise identical arguments.
   A dry run performs validation without creating an operation.
2. Open the elicited URL with the person's consent. The UI authenticates the
   person independently and calls `GET /api/v1/credential-operations/{operation}`.
   It displays the organization, intended consumer change, requested Secret/key
   pairs, references and, in commit mode, repository, base and directory.
3. Submit `POST` to that same path with `Content-Type: application/json`, the
   confirmation's strong `ETag` in `If-Match`, and
   `{"values":[{"secret":"tenant-credential-example","key":"apiKey","value":"<entered value>"}]}`.
   Supply exactly the requested pairs. `DELETE` with `If-Match` cancels a pending
   operation. Responses carry `Cache-Control: no-store`.
4. Retry the original MCP call with identical arguments and client/session
   context. It returns the saved receipt. Opening or accepting the URL alone
   never confirms a save. A cancelled operation requires a new `requestId`.

URL elicitation uses the upstream [MCP 2025-11-25 mechanism](https://modelcontextprotocol.io/specification/2025-11-25/client/elicitation)
and [2026-07-28 input requests](https://modelcontextprotocol.io/specification/2026-07-28/client/elicitation).
Older sessions receive `notifications/elicitation/complete` after saving; newer
clients retry the `input_required` result. Clients without URL elicitation receive
the same authenticated UI link in `credentials[].next`. No credential value
enters an MCP argument or result.

## GitHub authorization

REST commit mode needs a GitHub App user grant in addition to the person's
platform identity. Configure a confidential [RFC 8693 client](https://www.rfc-editor.org/rfc/rfc8693.html):

```yaml
github:
  enabled: false
githubTokenExchange:
  endpoint: https://muster.example.com/oauth/token
  clientID: agent-manager
  audience: agent-manager-github
  secretName: agent-manager-broker-client
  caSecretName: ""
```

The existing Secret contains `client-secret`. An optional CA Secret contains
`ca.crt`. The endpoint and output audience are operator-controlled; requests
cannot change them. This mode requires Dex OAuth and downstream authorization,
and replaces the App-pinned MCP registration. It also supplies ordinary agent
and organization commits with the person's grant.

The authorization server must independently validate the subject ID token and
confidential client, allow that client only the manager App's target audience,
and release only that person's prior consented grant. The manager rejects
actor-bearing identities, refresh-token responses and redirects, verifies the
returned user grant through GitHub's `/user`, and keeps it request-local. No
installation token substitutes for the person. No incoming bearer is sent to
GitHub. The exchanged App token has its configured GitHub permissions; RFC 8693
does not downscope that existing bearer.

Released muster supports this through provider Apps and
`tokenExchangeBroker.targets.<audience>.grantIssuer`. Configure its confidential
client allowlist and an explicit, nonempty input-audience allowlist. Its accepted
subject-token issuer must be the same canonical platform issuer under which the
person's App consent is stored. Where grant ownership is keyed by raw `sub`, do
not enable this profile with multiple independent identity issuers or ambiguous
existing grants. Verify this deployment condition before enabling credential
commits; this feature performs no grant migration. Missing consent returns
`auth_required`; the client owns the provider authorization UX.

The App must be installed with contents and pull-request write on the target
repository. GitHub documents the distinction between [user authorization and
installation authorization](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-with-a-github-app-on-behalf-of-a-user).
The existing App-pinned mode remains available for MCP commits, but alone cannot
authorize the credential HTTPS save.

## Storage and write boundaries

The chart creates a dedicated namespace, a ConfigMap-only Role and a quota of
256 ConfigMaps. Records are limited to 128 KiB and expire after 15 minutes, with
minute-based cleanup. They hold nonsecret intent, verified issuer/subject,
authenticated audience and MCP session context, revisions, file hashes and the
receipt. Values, ciphertext, ID tokens and App tokens never enter them. The
manager's ServiceAccount gains no tenant Secret or application ConfigMap access.

Reads, saves and cancellation independently verify issuer/subject ownership and
current organization permissions. Saves recheck resource versions, existing
Secret keys and consumers, Git destination, file hashes and encryption policy,
then server-side dry-run all intended writes as the person. Stale operation
versions return `412`; changed resources or Git inputs return `409`. Unknown
credential state, shared Secrets and multi-key Secrets are refused. Backend
refusals are sanitized before entering errors or logs. HTTP intermediaries and
Kubernetes audit policy must also exclude request/response bodies for credentials.

Apply mode writes Secrets before consumers; Kubernetes provides no transaction
across those objects. Commit mode uses gitops-commit's SOPS layout and
`ReplaceSecrets` for explicit replacement, preparing one PR with consumers and
encrypted Secrets. Encryption must cover `data` and `stringData`, with the
Secret's kind and name inspectable without decryption. Consumer files contain
one named Kubernetes object each; lists and multiple YAML documents are refused.
The receipt's credential state `committed` means a PR exists, not that Flux has
applied it. See [repository requirements](organizations-commit-mode.md).

## Recovery

Resource-version comparisons serialize saves across replicas. Completed retries
return the receipt without another write. After a known partial apply, refresh
the confirmation and retry: markers and recorded API-server resource versions
identify unchanged successful writes, preserving the first Secret value. An uncertain Git save
verifies the same private branch against stored encrypted-file hashes and
opens or reuses its PR. It never creates a second branch automatically.

A crash before a write, or after a write before recording its version, can leave
an operation without enough evidence to complete it. Inspect the recorded branch or marked live resources;
do not assume an error means nothing was written. Conflicting or missing evidence
fails closed. After expiry, inspect and resolve any partial resources or branch
before preparing a new operation; metadata expiry does not roll back changes.

## Verification

The opt-in `TestCredentialLab` requires the separate
`agentlab-credential-route` lab and its explicitly named proof ServiceAccount,
metadata namespace and candidate chart RBAC. It uses synthetic users and values,
real Dex verification and real Kubernetes admission. Set
`AGENT_MANAGER_CREDENTIAL_LAB_DIR` to that lab's state directory and run
`go test -race ./internal/server -run '^TestCredentialLab$' -count=1`.
Its listener runs the candidate locally against the lab API. This proves neither
a deployed candidate image nor real GitHub App consent and PR creation; those
require a separately configured deployment and a dedicated test repository.
