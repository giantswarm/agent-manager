# Organizations in commit mode

On an installation whose configuration lives in git, the organization tools take
`mode: commit`. Every write is first dry-run on the API server as the caller, so a
change the caller could not apply live is refused before a pull request exists. Then
agent-manager opens a pull request as the caller (the GitHub App user token) in the
installation's repository. This page states the repository layout the tools write and
what the repository must have before an installation turns on
`tenants.commit.repository`.

Credential input uses a [prepared credential operation](organization-credentials.md).
Its HTTPS save opens one pull request containing both the consumer and encrypted
Secret. REST commits use the configured RFC 8693 manager App grant; the App-pinned
MCP authorization mode alone cannot authorize an HTTPS credential save.

## The layout

| What | Path | Applied by |
|---|---|---|
| The `AgentPlatformOrganization` | `<objectsPath>/<parent>/<name>.yaml` | the installation's Kustomization (`deletionPolicy: Orphan`) |
| A model, connector, egress policy or member binding | `<contentPrefix>/<parent>/<name>/<kind>-<object>.yaml` (`modelconfig-claude.yaml`, `rolebinding-organization-admins.yaml`) | the organization's own Kustomization, which agent-platform-operator creates |
| A credential Secret | `<contentPrefix>/<parent>/<name>/<secret>-secret.enc.yaml` | the organization's own Kustomization, decrypted with the installation's SOPS key |

- `objectsPath` (`--organizations-objects-path`) is a directory that the installation's
  Kustomization builds. Its `kustomization.yaml` lists one directory per parent
  namespace. agent-manager adds a parent's directory there and removes it with the
  parent's last organization.
- `contentPrefix` (`--organizations-content-prefix`, default
  `agent-platform/organizations`) must be agent-platform-operator's `commitMode.path`
  without its `{parent}/{name}`. The installation's Kustomization must never include it,
  so the two directories may not contain each other (agent-manager refuses to start
  with such a configuration).
- The organization's file and the `organization-admins` file carry
  `kustomize.toolkit.fluxcd.io/prune: disabled`. A file that leaves git therefore never
  erases anything; agent-platform's admission requires the annotation on
  `organization-admins` when the organization's Flux identity applies it.
- A Secret is never in any other file. gitops-commit treats the
  `-secret.enc.yaml` name as secret, encrypts the file for the age recipients of the
  repository's `.sops.yaml`, and refuses it when no creation rule covers its path. A
  non-Secret object whose file name would contain `secret` or `credential` is refused.
- The values the files carry are what git says: an update starts from the file in git,
  not from the live object, so the API server's defaults do not enter the repository.

## What the repository needs

1. **A SOPS creation rule** for the credential files, with the installation's age
   recipients, for example:

   ```yaml
   creation_rules:
     - path_regex: ^agent-platform/organizations/.*-secret\.enc\.yaml$
       encrypted_regex: ^(data|stringData)$
       age: age1...
   ```

   Do not write a rule that matches every file of the content path. gitops-commit then
   treats each file as secret: it encrypts it once and never updates it again.

2. **CODEOWNERS for the organization paths**, naming the platform team, and branch
   protection that requires a code owner's review:

   ```text
   /agent-platform/organizations/                               @giantswarm/team-bumblebee
   /management-clusters/<mc>/agent-platform-organizations/      @giantswarm/team-bumblebee
   /.github/CODEOWNERS                                          @giantswarm/team-bumblebee
   ```

   The platform team owns the CODEOWNERS file itself. An organization's admin can
   therefore neither change the rule nor approve a merge into an organization's path
   alone.

### Why the platform team reviews every organization pull request

Until giantswarm/agent-platform-operator#15 gives each organization its own Flux source
or SOPS key, every organization path is decrypted with the installation's one key. An
organization's `kustomization.yaml` can list a file of another organization's path
(`../billing/tenant-credential-x-secret.enc.yaml`), and its Kustomization would decrypt
and apply it in the organization's namespace. The organization's Flux identity cannot
write outside its namespace, but it can copy another organization's credential into its
own. Platform review of every merge into the organization paths is the control against
this. Two companions narrow it:

- giantswarm/giantswarm#39013: a CI check in the installation repositories that refuses
  `..`, absolute paths and remote bases in an organization's `kustomization.yaml`;
- giantswarm/flux-app#384: kustomize-controller without remote bases.

giantswarm/agent-platform-operator#15 is the long-term fix.

## Deleting an organization

`delete_organization` with `mode: commit` takes two steps:

1. Without `confirm`, it opens the pull request that removes the organization's file and
   its whole content path (every file listed under it, nested `kustomization.yaml`
   files included), and marks the live object with
   `agent-platform.giantswarm.io/delete-pull-request: <url>`. Nothing is erased.
   agent-platform-operator reports the organization `Orphaned` with reason
   `DeleteMerged` once the file has left the installation Kustomization's inventory.
2. After the merge, the organization is pending erase once agent-manager has read the
   merged pull request's files and found the object's file removed by it (a move that
   GitHub reports as a rename does not count). Without `confirm` the call
   answers what is erased. With `confirm: <parent>/<name>` (Erase now) it sets
   `kustomize.toolkit.fluxcd.io/prune: enabled` on the live object and deletes it; the
   operator erases it.

The erase is refused:

- while the object's file is still in git, also when the pull request the object names
  is merged (a merged pull request that did not remove the file is not the delete);
- when the file is gone from git without a merged delete pull request (Missing from
  git): restore the file instead. `lastAppliedRevisionUrl` on the organization links the
  last commit that held it (the operator's `status.lastAppliedRevision`), and
  `lastAppliedFileUrl` the file itself at that commit (`lastAppliedFile`, the path mode
  commit keeps it at);
- when the merged delete pull request did not remove the file itself: it was edited
  before its merge, and another merge removed the file (Missing from git), or it moved
  the file to another path (a rename on GitHub), which may still define the
  organization in git. Restore the file, or move it back, and open the delete again with
  `mode: commit` and no `confirm`. A move whose content changed so much that GitHub
  reports a removal and an addition is not detected;
- when GitHub cannot list the merged pull request's files (it lists at most 3000, or
  the call fails): whether it removed the file is unknown, so nothing is erased. Call
  again to retry; if the listing keeps failing, restore the file and open the delete
  again.

agent-manager reads the delete pull request with pull request reads only: whether it is
merged, and its files. It does not read its reviews, or the commit statuses and check
runs of its head, so the GitHub App needs no permission beyond contents and pull
requests. When GitHub refuses a step that only reads with 403, the answer names the
step and the App's permissions: a new sign-in does not change them.
