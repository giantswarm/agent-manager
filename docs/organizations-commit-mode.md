# Organizations in commit mode

On an installation whose configuration lives in git, the organization tools take
`mode: commit`. Every write is first dry-run on the API server as the caller, so a
change the caller could not apply live is refused before a pull request exists. Then
agent-manager opens a pull request as the caller (the GitHub App user token) in the
installation's repository. Each write's pull request has a head branch of its own, cut
from the base branch: the object's branch name (`agent-manager/<verb>-<name>`),
an underscore and a random suffix. A second write of the same object is refused while a
pull request of the object is open, also one on the name without a suffix that an older
agent-manager opened. When two writes of the same object arrive at almost the same time
and each opens a pull request, the pull request with the lower number stays: the write
that opened the other one closes it and answers the same conflict. When that close
fails, the answer names both pull requests and asks the caller to close the second one.
This page states the repository layout the tools write and
what the repository must have before an installation turns on
`tenants.commit.repository`.

Credential input uses a [prepared credential operation](organization-credentials.md).
Its HTTPS save opens one pull request containing both the consumer and encrypted
Secret. Commits, over MCP and REST, use the configured RFC 8693 manager App grant.

## The layout

| What | Path | Applied by |
|---|---|---|
| The `Organization` (`security.giantswarm.io`, with `spec.agentPlatform` and the admins in `spec.admins`) | `<objectsPath>/<name>.yaml` | the installation's Kustomization (`deletionPolicy: Orphan`) |
| A model, connector, egress policy or member binding (`editor`, `user`) | `<contentPrefix>/<name>/<kind>-<object>.yaml` (`modelconfig-claude.yaml`, `rolebinding-organization-editors.yaml`) | the organization's own Kustomization, which agent-platform-operator creates |
| A credential Secret | `<contentPrefix>/<name>/<secret>-secret.enc.yaml` | the organization's own Kustomization, decrypted with the installation's SOPS key |

- `objectsPath` (`--organizations-objects-path`) is a directory that the installation's
  Kustomization builds. Its `kustomization.yaml` lists one file per organization.
- `contentPrefix` (`--organizations-content-prefix`, default
  `agent-platform/organizations`) must be agent-platform-operator's `commitMode.path`
  without its `{name}`. The installation's Kustomization must never include it,
  so the two directories may not contain each other (agent-manager refuses to start
  with such a configuration).
- The admins are not a file of the content path: they are `spec.admins` of the
  organization's file, and agent-platform-operator binds them to `agent-platform:admin`
  in the organization namespace.
- The organization's file carries `kustomize.toolkit.fluxcd.io/prune: disabled`. A
  file that leaves git therefore never erases anything.
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

3. **"Automatically delete head branches"** turned on in the repository's settings.
   Every write opens its pull request on a new branch, and agent-manager deletes none:
   without the setting, the branch of each merged pull request stays. The branch of a
   pull request closed without a merge, and of a write refused after its branch was cut,
   stays in either case.

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
   merged pull request's files, found the object's file removed by it (a move that
   GitHub reports as a rename does not count), and found that no file under the objects
   path still defines the organization on the base branch. Without `confirm` the call
   answers what is erased. With `confirm: <name>` (Erase now) it sets
   `kustomize.toolkit.fluxcd.io/prune: enabled` on the live object and deletes it; the
   operator erases it.

While the operator reports the organization `Orphaned` with reason `DeleteMerged`
(Pending erase), and while it is being erased, `update_organization` and every write
tool of its contents (members, models, connectors, egress policies and credentials), in
both modes, answer `conflict`: Erase now, or restore its file to keep it. A caller who
may not read the organization gets `forbidden`, not its state. Reads and `delete_organization`
keep working.

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
  `mode: commit` and no `confirm`;
- when a file under the objects path, whatever its name, still defines the
  organization on the base branch: a move whose content changed so much that GitHub
  reports a removal and an addition, or a definition that the pull request or a later
  commit wrote there. A document defines it when it is an `Organization` of its name,
  whatever namespace it carries (the kind is cluster-scoped), also as an item of a
  `List`. Where
  a kustomization under the objects path could make another document the organization
  (the table in "What the scan is for" below gives the rule), that document is taken
  as a match too. The answer names the file, and each kustomization that made the
  match and why: remove that definition and erase again, or move the file back and
  open the delete again. A resource file that does not parse defines nothing, since
  Kustomize cannot apply it either; a kustomization that does not parse makes every
  organization document a match. When the objects path cannot
  be read (a read fails, or the repository's tree is too large for one read), nothing
  is erased (see "What the scan is for" below);
- when a kustomization under the objects path loads files from outside it: a
  `resources`, `components` or `bases` entry that is a remote (it holds `://` or `//`),
  a path that leaves the objects path, or one that names no file or directory under it.
  The scan does not read what such an entry loads, so whether it still defines the
  organization is unknown. The answer names the kustomization and the entry: bring
  what it loads under the objects path, or remove the entry, and erase again;
- when GitHub cannot list the merged pull request's files: whether it removed the file
  is unknown, so nothing is erased. Call again to retry. A pull request of more than
  3000 files never lists, since GitHub lists at most 3000: restore only the object's
  file, not the content path, and open the delete again. The merge removed the content
  path already, so the new delete pull request changes the objects path only, and its
  files list.

### What the scan is for

The scan of the objects path gives an early, clear answer for the usual mistake: a
delete pull request that moved the organization's file instead of removing it, or a
later commit that wrote it again. A file named `kustomization.yaml` (`.yml`,
`Kustomization`) is read both as its directory's kustomization and as a resource file,
since another kustomization may list it as one. The answer "pending erase" and the
erase take the same scan.

The scan reads the whole objects path at one commit: it reads the head of the base
branch once, then the tree and every file at that commit. A commit that lands during
the scan does not change what it reads, so a move of the definition between the tree
read and the file reads cannot hide it (giantswarm/agent-manager#420).

The scan reads the files as they are and does not run Kustomize. So it fails closed on
what a kustomization changes. Every kustomization file under the objects path counts,
wherever it is: an overlay in a sibling directory refers to the file by a path such as
`../../base` (giantswarm/agent-manager#399). An `Organization` is cluster-scoped, so a
`namespace` a kustomization sets changes nothing of it. For the erase of `<name>`, an
`Organization` document counts as still in git when a kustomization could make it that
organization:

| The kustomization has | The documents that count |
| --- | --- |
| `namePrefix` or `nameSuffix` | a document whose own name is a part of `<name>`: a prefix or a suffix only adds text |
| a non-empty `transformers`, `patches`, `patchesJson6902`, `patchesStrategicMerge` or `replacements` | every document: these can set any name |
| a non-empty `vars` and a non-empty `configurations` | every document: a `varReference` in a configurations file can substitute `$(VAR)` in a name |
| a value of a form the scan does not know (a `nameSuffix` that is not a string, for one) | every document |
| content that does not parse | every document |

A document whose own name is `<name>` always counts, and so does one whose name the
scan cannot read: SOPS-encrypted, or with a variable (`${...}` of Flux, `$(...)` of a
Kustomize var). The scan reads a
kustomization's keys in any case (`Patches:` is `patches:`), as Kustomize does.

The scan reads only the files under the objects path. A kustomization there whose
`resources`, `components` or `bases` entry loads files from elsewhere refuses the
erase, whatever those files hold: a remote entry (it holds `://` or `//`, or it has
a remote form such as `github.com/owner/repo/path` or `git@host:owner/repo`), an
absolute path, a path that is not under the objects path once joined with the
kustomization's directory, or a
path that names no file or directory under the objects path, which Kustomize then
loads as a remote or fails on. An entry that stays under the objects path, such as
`../../base` from a sibling overlay, is read as above and refuses nothing by itself
(giantswarm/agent-manager#437).

The cost: in a repository that uses such an overlay, an erase can be refused although
Flux's build no longer holds the organization. The answer names each kustomization and
the field that made it count; a person then removes that setting, or the overlay, and
erases again. The objects paths that agent-manager writes use no such overlay
(giantswarm/agent-manager#423).

The cluster has a second guard, which follows Flux's own Kustomize build. The
admission policy `agent-platform-organization-delete-intent`
(giantswarm/agent-platform, `helm/agent-platform-connectivity/templates/organizations/admission.yaml`)
permits the delete of an organization that carries an `Orphaned` condition (one that
agent-platform-operator found applied from git) only when that condition is `True`
with reason `DeleteMerged`. The operator sets `Orphaned=True` only when the
installation's Flux Kustomization is settled (its generation observed, `Ready` true,
not reconciling) and its `status.inventory` has no entry for the organization; with
the delete annotation the reason is `DeleteMerged`. That inventory is what Flux applied
from the Kustomization's last applied revision. So while a revision that Flux applied
still builds the organization, by whichever path, the condition is `False`
(`InInventory`) and the API server refuses the erase. The limit is the same as Flux's:
a definition written after Flux applied the delete counts only once Flux has applied
that later revision. While Flux applies no new revision (the Kustomization is
suspended), the condition follows the last applied one, and while the Kustomization is
not settled (reconciling, or not `Ready`) the operator keeps the condition it had. In
both cases an earlier `DeleteMerged` still permits the erase.

### Reads

To erase, agent-manager reads the delete pull request with pull request reads only:
whether it is merged (one read), and its files (one read for each 100 files). It does
not read its reviews, or the commit statuses and check runs of its head. Then it reads
the objects path on the base branch: one read of the branch's ref (its head commit),
then, at that commit, one read of the repository's tree and two contents reads for
each file under the objects path (about two for each organization). The erase is rare, so these reads are
taken on each erase and on each answer of pending erase. The GitHub App needs no
permission beyond contents and pull requests. When GitHub refuses a step that only reads with 403, the answer
names the step and the App's permissions: a new sign-in does not change them.
