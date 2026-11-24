# Model and organization credential operations

Models, connectors and EgressPolicies name credential Secrets by reference. The
organization tools accept no credential values. A missing credential prepares
an operation before writing any consumer. Ordinary edits with existing
credentials do not prepare an operation or change the Secret; a new value for
an existing credential goes through `replace_organization_credential`.

Model operations also accept the configured installation namespace, requiring
cluster-admin authorization again when an operation is read or saved. Its target
declares `scope: platform`, without a synthetic organization. Connector and egress
operations remain organization-scoped. Installation models require
`credentials.uiURL` with a `{namespace}` expression and no `{organization}`:
an installation model has none, so it would leave an empty path segment in
its link. OAuth and downstream authorization are
required. Without tenancy the template must be such a template, and the form and
model tools work for installation models only. Tenant management tools stay
disabled, and organization discovery is empty.

Native `AnthropicVertexAI` and `GeminiVertexAI` take nonsecret `projectID` and
`location`; their credential key defaults to `GOOGLE_APPLICATION_CREDENTIALS`.
Their confirmation declares `inputFormat: google-service-account-json`. Other
credentials use `text`; an omitted format also means text. The client presents
the uploader on the credential page. The server checks the service-account type,
RSA private key and Google OAuth token endpoint before writing. A key whose
`project_id` differs from the model's `projectID` is accepted, because a
central project's service account can hold Vertex AI access in the model's
project: the save's `note` names both projects so the person can check that
access. Only keys saved through the form are checked. A new Vertex model that
names an existing credential Secret opens the form to replace it, so its key
is checked too; an installation model may still reference a Secret managed
elsewhere, which is not checked.
`location` is `global`, `us`, `eu` or a Google region such as `europe-west1`;
`projectID` is a Google project ID. Vertex AI calls go from the agent to Google
directly: the LLM gateway's metering, rate limits and spend reporting do not
apply to them. Replacement validates every Vertex consumer; project or location
changes invalidate the confirmation. Model deletion retains its Secret.

`endpoint`, `insecureSkipTLSVerify` and `noCredential: true` (keyless
endpoints, excluding Vertex) apply to installation models only; an
organization model with one of them is refused. On an organization model
`insecureSkipTLSVerify: false` removes a `tls.disableVerify` written
elsewhere, and `endpoint: ""` changes nothing: admission holds its base URL
to the LLM listener. In the installation namespace
agent-manager writes only `tenant-credential-*` Secrets: a model may reference
any existing Secret, such as one GitOps or Helm manages, and its value is
replaced where it is managed. The installation form and model tools start only
with `--multi-tenant` or `--credential-ui-url`.

Installation commit mode requires an explicit `tenants.commit.platformModelsPath`
in the trusted repository and never derives a directory from an organization.

`set_organization_model`, `set_organization_connector` and
`set_organization_egress_policy` with `create: true` only create: a model,
connector or egress policy of that name that is live, or whose file is in git,
is refused with error code `already_exists` (`409`) and nothing is written, at
the call (a dry run included) and when the create itself meets an object made
since the read. Without `create` each tool creates or changes the object as
before, with the same answers. When the new object waits for its key, the
operation creates it at the save: a save of any operation that creates a model, connector or egress policy
answers `already_exists` when that object exists by then, live or in git. The
save checks before it writes; an object made in the moment between its dry
run and its create finds the Secret already written. In apply mode the save
then deletes the Secret it created, if nothing changed it since its write; it
never deletes a Secret that existed before the save. A refused delete leaves
the Secret (and is logged), and the other object can read the value in the
moment before the delete. The operation is uncertain; the person prepares
again.

`set_organization_model` refuses a change of the provider of an existing model
that keeps the model's credential Secret, with error code `conflict` (`409`),
in apply and commit mode (in commit mode the provider in the model's file in
git is compared), a dry run included; nothing is written. The Secret holds the
old provider's key, so either order of a provider change and a key change
sends one provider's key to the other for a time. To change the provider, give
a new `credentialSecret` that does not exist yet: the call prepares an
operation, and its save writes the new Secret first, then the provider and the
reference in one write of the model. A different `credentialSecret` that
exists already is used as it is: it must hold the new provider's key. Or create
a new model. A change of the
model id alone, with the same provider, is not affected.

`replace_organization_credential` takes the organization namespace, the Secret
and the key, and prepares an operation that writes that Secret and no consumer:
a rotation resends no model, connector or egress policy. At least one
ModelConfig, RemoteMCPServer or EgressPolicy of the namespace must read that key
of the Secret, else it answers `not_found`. An object reading another key of the
same Secret is refused with `409` naming that key: the Secret is written whole,
holding the one key, so the other key would be lost. Its result has `kind`
`credential` and `name` the Secret; the credential's state is `missing` when the
Secret does not exist yet, else `replace`. The operation keeps the objects that
read the Secret and where they send the value, as prepared, and its
confirmation lists those. At the preparation, in commit mode, the objects in
git must be the same as the live ones, else it answers `409` until Flux applies
git. The save checks the consumers again, live and, in commit mode, in git:
when they or where they send the value differ from the prepared ones (one
changed, was added, reads another key or is gone), it answers `409` with error
code `credential_consumers_changed` and does not write the credential. The
message names no object; the person starts the replacement again, and the
tool call that prepares it uses a fresh `requestId`. The save also refuses a
consumer written live in commit mode, or applied by Flux in apply mode, as the
preparation does. A dry run asks admission about the Secret write, as the real
call does.

## Client-owned UI

Configure an HTTPS URL template, for example:

```yaml
credentials:
  uiURL: https://portal.example.com/customize/models/credentials/graveler/{namespace}/{operation}
```

The template is an [RFC 6570](https://www.rfc-editor.org/rfc/rfc6570) URI
template. One `{operation}` expression is required in the path or query
(`{?operation}` works too). `{organization}` and `{namespace}` are optional, but installation
model links need `{namespace}` and no `{organization}`, and
without tenancy such a template is required; the scheme and host are fixed,
and no other variable is accepted. The chart refuses the same templates as the
server. Routes and the
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
   `references` lists every object of the namespace that names a requested
   Secret, read as the person when the confirmation is read, each planned
   consumer in place of its live object: the key it reads, a ModelConfig's
   `provider`, a RemoteMCPServer's URL and an EgressPolicy's origins.
3. Submit `POST` to that same path with `Content-Type: application/json`, the
   confirmation's strong `ETag` in `If-Match`, and
   `{"values":[{"secret":"tenant-credential-example","key":"apiKey","value":"<entered value>"}]}`.
   Supply exactly the requested pairs. `DELETE` with `If-Match` cancels a pending
   operation. Responses carry `Cache-Control: no-store`.
   The credential POST accepts JSON bodies up to 2 MiB, including escaped values;
   other REST request bodies retain their existing limit.
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
  enabled: true
  tokenExchange:
    tokenEndpoint: https://muster.example.com/oauth/token
    audience: <muster provider App>
    clientID: ""
    existingSecret: agent-manager-github-token-exchange
    caSecretName: ""
```

`audience` is the RFC 8693 target and muster's provider App (its
`aggregator.providerApps` key) whose grant it releases: a GitHub App
installed with contents and pull-request write on every repository a commit
targets. The Secret contains `client-secret`, and `client-id` when `clientID`
is empty; the chart mounts the whole Secret. With `clientSecret` instead of
`existingSecret` the chart renders `<fullname>-github-token-exchange`. An
optional CA Secret contains `ca.crt`. The endpoint and audience are
operator-controlled; requests cannot change them. This mode requires
`github.enabled`, Dex OAuth and downstream authorization. It also supplies
ordinary agent and organization commits with the person's grant.

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
commits; this feature performs no grant migration. Missing consent answers
the MCP tool with `{"status": "grant_required", "apps": ["<audience>"]}` in
`structuredContent`; the MCPServer lists `github.tokenExchange.audience`
under `auth.providerApps`, so muster answers it with the App's sign-in link.
REST returns `auth_required`; the client owns the provider authorization UX.

The App must be installed with contents and pull-request write on the target
repository. GitHub documents the distinction between [user authorization and
installation authorization](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-with-a-github-app-on-behalf-of-a-user).

## Storage and write boundaries

The chart creates a dedicated namespace, a ConfigMap-only Role and a quota of
256 ConfigMaps. Records are limited to 128 KiB and expire after 15 minutes, with
minute-based cleanup. They hold nonsecret intent, verified issuer/subject,
the SHA-256 digest of the verified email address (see
[Who owns an operation](#who-owns-an-operation)), authenticated audience and
MCP session context, revisions, file hashes and the receipt. Values, ciphertext, ID tokens and App tokens never enter them. The
manager's ServiceAccount gains no tenant Secret or application ConfigMap access.

Reads, saves and cancellation independently verify issuer/subject ownership
([Who owns an operation](#who-owns-an-operation)) and current organization
permissions. Saves recheck consumers, Git destination,
file hashes and encryption policy, then server-side dry-run all intended writes
as the person. Stale operation versions return `412`, a save or cancellation
without `If-Match` returns `428`, and changed resources or Git inputs return
`409`: code `credential_consumers_changed` for the consumers of a replacement,
`conflict` otherwise. A consumer tool refuses a Secret another object names;
`replace_organization_credential` lists every consumer and replaces a shared
Secret for all of them, and refuses at preparation and at save an object that
reads another key of it. Backend refusals are sanitized before
entering errors or logs.

Direct credential saves tolerate consumer status updates. The saved confirmation
binds the consumer's UID, configuration and credential references; the final
update rereads that object and retries resource-version conflicts only while
that confirmed configuration remains unchanged. Status and managed fields are
preserved. A changed URL, Secret binding, UID or other configuration still
requires a new preparation. This does not relax the operation's `If-Match` or
current permission checks.

Credentials are write-only. Neither the person nor agent-manager reads a
credential Secret. `get`, `list` and `watch` return its data, and so does a
`patch` (its response is the stored object), so the organization admin role
must grant none of them on Secrets. Whether a Secret exists is the answer to a
server-side dry-run create as the person: `AlreadyExists` means it exists, and
nothing is written. A missing credential is created. One the person asked to
replace is written whole with an unconditional update (no `resourceVersion`):
the Secret then holds the one key its consumer names, the type `Opaque`, the
`app.kubernetes.io/managed-by` label and the operation marker, and any other
key, label or annotation is dropped. A Secret that appears between the
confirmation and the save fails the save's create with `409`. Two replacements
of one Secret at once leave the last one written. In apply mode a replacement is
dry-run as an update when it is prepared, so admission, which sees the stored
Secret, refuses it before a value is entered; the platform refuses the update
of a Secret an organization's Kustomization applied
(`agent-platform-organization-git-secrets`), which is changed in Git.

Without a read, a Secret that exists counts as `present` whatever keys it
holds, except where its consumer reports the key missing: a ModelConfig whose
`ResolvedRefs` condition is `False` with reason `APIKeySecretKeyNotFound` for
its current generation names a `missing` credential, and the save writes the
Secret whole with the entered value. RemoteMCPServers and EgressPolicies report
no such condition.

HTTP intermediaries and Kubernetes audit policy must also exclude
request/response bodies for credentials.
Several headers of one connector may reference the same Secret/key pair; its
confirmation requests that value once.

Apply mode writes Secrets before consumers; Kubernetes provides no transaction
across those objects. Commit mode uses gitops-commit's SOPS layout and
`ReplaceSecrets` for explicit replacement, preparing one PR with consumers and
encrypted Secrets. Encryption must cover `data` and `stringData`, with the
Secret's kind and name inspectable without decryption. Consumer files contain
one named Kubernetes object each; lists and multiple YAML documents are refused.
`replace_organization_credential` in commit mode commits the encrypted Secret
alone, whatever Git consumers name it; an encrypted Secret file holding another
key is refused. In apply mode it refuses a Secret whose consumer Flux applies.
Creating a consumer whose file already exists in Git is refused until Flux
applies it, so a create cannot replace configuration awaiting reconciliation.
The base is checked again once the pull request's branch is cut from it: a
file another commit added after the first check is refused there too
(`already_exists` with `create`, else `conflict`), before anything is
committed. The branch stays, without a commit, and the next write brings it up
to date. When another commit added the identical files and their
kustomization entries before the change was planned, so that Git already
carries the whole change, the create is refused the same way, and no branch
is cut. This holds for every commit-mode create, the organization's own
object included (see
[repository requirements](organizations-commit-mode.md)).
The receipt's credential state `committed` means a PR exists, not that Flux has
applied it. See [repository requirements](organizations-commit-mode.md).

## Who owns an operation

An operation belongs to the person who prepared it, as the token's issuer and
subject (`sub`) name them. Reads, saves and cancellations of every other caller
are refused.

Dex gives one person a different subject for each connector. A person who
prepares an operation from an MCP client signed in through one connector and
opens the link in the Dev Portal with a session from another connector is,
for this check, another caller. The answer to that caller names the cause:

| Caller | Answer |
|---|---|
| The owner: same issuer and subject | The operation |
| Same issuer, same verified email address, another subject | `403`, code `credential_operation_other_login`: the operation was started with another login; sign in the same way as when it was started, then open the link again |
| Another email address, an email address that is not verified, another issuer, or a delegated token | `404`, code `not_found` |

An expired or cancelled operation answers as before for every caller: the
owner reads a cancelled operation's state and gets `409` for an expired one,
every other caller gets `404`. The other-login answer
holds no organization, namespace, model, Secret, key or other content of the
operation. Its words are the error code and a fixed message.

The email address is verified when the identity provider's token says
`email_verified: true`; an absent claim counts as not verified. At preparation
the operation records the SHA-256 digest of the verified address, with its
ASCII letters in lower case, and no digest when the address is not verified.
The digest is a pseudonym, not a secret: whoever reads the operation records
can test a guessed address against it. An operation
prepared before agent-manager recorded the digest has none: every caller but
its owner gets `404` for it.

**Why the email address does not become the owner.** A person who can sign in
through two connectors could open their own operation with either login if the
verified email address were the owner. That is safe only when every sign-in
connector of every installation proves the address: a connector that passes
on an address that the person does not control (a self-asserted address, an
upstream identity provider with weak checks, a misconfigured claim mapping)
would then hand a stranger the key entry form, which is the most sensitive
page of the platform. agent-manager cannot check that property of each
installation's Dex configuration. The subject is what the identity provider
issues for one login, so it stays the owner, and the person signs in again
with the same connector.

**What the other-login answer tells.** It tells the caller that an operation
with this id exists and that it was prepared with the caller's own verified
email address. The caller already holds the operation id (48 hex characters
of a digest of the preparing login, its client session and the request, sent
only in that person's link), has proven the same issuer, and has a token that
says the identity provider verified the same address. The answer gives the
caller nothing they could not find out by signing in with their other login,
and it grants no read, save or cancellation. It is the same for a pending,
saving or completed operation, so it does not tell those states apart. A
connector with a weak email check reaches only this sentence, never the
operation.

## Upgrading credential UI configuration

Replace `--dev-portal-url` and chart `devPortal.url` with `--credential-ui-url`
and `credentials.uiURL`. Supply an HTTPS RFC 6570 URI template containing `{operation}`.
Enable OAuth and downstream authorization and configure a dedicated metadata
namespace with `--credential-operations-namespace`; the chart derives that
namespace when `credentials.operationsNamespace` is empty. The client form
must use the authenticated operation API and its `If-Match` confirmation.

## Recovery

Resource-version comparisons of the operation record serialize saves across replicas.
Completed retries return the receipt without another write. After a known
partial apply, refresh the confirmation and retry with the value: a Secret write
the operation recorded is not repeated, so the first value stays; one it did not
record is written again: created when the Secret does not exist, otherwise
updated with the retried value, whoever created it in between. Consumers are
recognized by their marker and recorded configuration hash, including UID;
status-only changes do not prevent recovery. Records without a configuration
hash require their exact recorded resource version. Only direct writes
carry the `agent-manager.giantswarm.io/credential-operation`
marker; committed files hold the desired state alone. An uncertain Git save
verifies the same private branch against stored encrypted-file hashes and
opens or reuses its PR. It never creates a second branch automatically.

A Git save runs all its checks before it creates the branch: the Git files and
the encryption policy against the preparation, the objects that use the key, a
name already taken, and the encryption of each value. A refusal there writes
nothing: the operation stays `pending`, a retry gets the same refusal, and a
fresh preparation is what is needed. Only a failure after the branch exists
leaves the operation `uncertain`.

Once the checks pass, the branch, the commit and the pull request run to their
end also when the caller goes away (its request is cancelled or times out):
the operation ends `complete`, its credentials `committed`, with its pull
request, as for a caller that waited, and a repeated call gives the receipt.
These writes have their own time limit of two minutes, and recording their
outcome has 15 seconds more; the operation does not expire before they end. A
restart of agent-manager during the writes still stops them: the operation
stays `saving` or `uncertain`, and a repeated save recovers it as above.

A failed save logs `credential save failed` with the `step` that failed (for
example `dry run`, `prepare commit`, `create branch`, `commit` or
`open pull request`) and its `cause`, at `ERROR` for a failure and at `WARN`
for a refusal the caller can act on. A Kubernetes API answer is logged by its
reason and code only, since it may quote the refused object. Any other cause
is logged with every value the save received removed, as typed, quoted and
base64 encoded.

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
