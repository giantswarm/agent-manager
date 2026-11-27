package organizations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/gitops-commit/layout"
	orgv1alpha1 "github.com/giantswarm/organization-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/kube"
)

// orgKind is the kind of an organization's object, Organization of
// security.giantswarm.io.
const orgKind = "Organization"

// createCommit opens one pull request with the organization's file in the
// objects path, after the access review Create asked and a dry run of the
// object as the caller. No RoleBinding goes with it: agent-platform-operator
// binds spec.admins in the organization namespace.
func (s *Service) createCommit(ctx context.Context, c kube.Client, req CreateRequest, admins []Subject, res *CreateResult) (*CreateResult, error) {
	if err := segment("name", req.Name); err != nil {
		return nil, err
	}
	what := "Organization " + req.Name
	way := wayOrganizationsAdmin()
	cm, err := s.newCommit(ctx)
	if err != nil {
		return nil, err
	}
	objFile := cm.objectFile(req.Name)
	if ok, err := cm.exists(ctx, objFile); err != nil || ok {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s is in %s already and not applied yet: wait for Flux to apply it", agents.ErrConflict, objFile, cm.cfg.Objects.Repository)
	}
	content := cm.contentDir(req.Name)
	// A new organization starts from an empty content path: files an
	// earlier organization of that name left would come back with it.
	if left, err := cm.list(ctx, content.Path()); err != nil || len(left) > 0 {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: the content path %s in %s holds files (%s) an earlier organization left: remove them first, they would be applied into the new organization", agents.ErrConflict, content.Path(), cm.cfg.Objects.Repository, strings.Join(left, ", "))
	}

	obj := unstructuredOf(s.organizationObject(req, admins))
	// The file never leaves git into an erase: only an explicit delete of
	// the live object erases.
	setAnnotation(obj, PruneAnnotation, PruneDisabled)
	if err := apply(ctx, c.Dynamic(), []write{{gvr: GVR, kind: orgKind, op: opCreate, obj: obj.Object, name: req.Name, what: what, reason: way}}, true); err != nil {
		return nil, err
	}
	isSecret, err := cm.secretFiles(ctx)
	if err != nil {
		return nil, err
	}
	if err := refusePlainAsSecret(isSecret, objFile, orgKind, req.Name); err != nil {
		return nil, err
	}

	// The organization's own Kustomization applies the content path from the
	// operator's first reconcile on: an empty kustomization.yaml makes it a
	// path Flux can build before the organization has any content.
	contentKustomization := path.Join(content.Path(), layout.KustomizationFile)
	changes := []fileChange{
		{dir: cm.objectDir(), write: map[string][]byte{objFile: manifest(obj, false)}, create: []string{objFile}, objects: true},
		{dir: content, add: map[string][]byte{contentKustomization: []byte("resources: []\n")}, create: []string{contentKustomization}},
	}
	title := "feat(organizations): add organization " + req.Name
	body := fmt.Sprintf("Adds the organization %s with the Agent Platform enabled: its Organization object in %s. Its admins are spec.admins, which agent-platform-operator binds to %s in the organization namespace. Its content path %s starts with an empty kustomization.yaml, so its own Flux Kustomization builds it before it has content.", req.Name, objFile, RoleAdmin, content.Path())
	res.Commit, err = cm.open(ctx, changes, branch("create", req.Name), title, body, req.DryRun)
	if err != nil {
		return nil, err
	}
	planned, err := organizationOf(obj)
	if err != nil {
		return nil, err
	}
	res.Organization = s.summary(planned)
	res.Commit.LiveSteps = []string{"after the merge Flux applies the object; " + operatorNext}
	if req.DryRun {
		res.Next = "dry run: no pull request was opened; the files are in commit.files"
	} else {
		res.Next = "merge the pull request " + res.Commit.PullRequest + "; " + reviewNote
		s.log.Info("organization create committed", agents.UserAttr(ctx), "name", req.Name, "pullRequest", res.Commit.PullRequest)
	}
	return res, nil
}

// gitObject is the file of a git-owned organization: refused when the
// object was written live, or when another file defines it.
func (s *Service) gitObject(ctx context.Context, cm *orgCommit, u *unstructured.Unstructured) (string, *unstructured.Unstructured, error) {
	if !fromGit(u) {
		return "", nil, fmt.Errorf("%w: Organization %s was written live, not applied from git: use mode apply", agents.ErrConflict, u.GetName())
	}
	p := cm.objectFile(u.GetName())
	file, err := cm.read(ctx, p)
	if err != nil {
		return "", nil, err
	}
	return p, file, nil
}

// updateCommit changes the organization's file as an update would change
// the object, after a dry run of the same change live as the caller.
func (s *Service) updateCommit(ctx context.Context, c kube.Client, u *unstructured.Unstructured, org *orgv1alpha1.Organization, req UpdateRequest) (*UpdateResult, error) {
	cm, err := s.newCommit(ctx)
	if err != nil {
		return nil, err
	}
	p, file, err := s.gitObject(ctx, cm, u)
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, fmt.Errorf("%w: Organization %s is applied from git, but not from %s in %s: change it in the file that defines it", agents.ErrConflict, req.Name, p, cm.cfg.Objects.Repository)
	}
	res := &UpdateResult{Before: s.summary(org), DryRun: req.DryRun, RequestedBy: agents.RequestedBy(ctx)}
	if res.Changed, err = changeOrganization(file, req); err != nil {
		return nil, err
	}
	setAnnotation(file, PruneAnnotation, PruneDisabled)
	isSecret, err := cm.secretFiles(ctx)
	if err != nil {
		return nil, err
	}
	if err := refusePlainAsSecret(isSecret, p, orgKind, req.Name); err != nil {
		return nil, err
	}
	// The dry run is of what the file will say, on the live object; the
	// answer is the live object with the change.
	next := file.DeepCopy()
	withLive(next, u)
	live := u.DeepCopy()
	if _, err := changeOrganization(live, req); err != nil {
		return nil, err
	}
	after, err := organizationOf(live)
	if err != nil {
		return nil, err
	}
	res.After = s.summary(after)
	if len(res.Changed) == 0 {
		return res, nil
	}
	what := "Organization " + req.Name
	if err := apply(ctx, c.Dynamic(), []write{{gvr: GVR, kind: orgKind, op: opUpdate, obj: next.Object, name: req.Name, what: what, reason: wayOrganizationsAdmin()}}, true); err != nil {
		return nil, err
	}
	title := "chore(organizations): update organization " + req.Name
	body := fmt.Sprintf("Changes %s of the organization %s.", strings.Join(res.Changed, ", "), req.Name)
	res.Commit, err = cm.open(ctx, []fileChange{{dir: cm.objectDir(), write: map[string][]byte{p: manifest(file, false)}, objects: true}}, branch("update", req.Name), title, body, req.DryRun)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// deleteCommit is a delete in commit mode, in two steps. While the object's
// file is in git, it opens the pull request that removes the file and the
// whole content path, and marks the live object with the pull request
// (DeletePullRequestAnnotation): answered is true. Once that pull request is
// merged, the merge removed the file and the file is gone from the base
// branch, the organization is pending erase and Delete goes on to the live
// erase (answered false). A live delete is refused while the file is in
// git: Flux would re-create the object.
func (s *Service) deleteCommit(ctx context.Context, c kube.Client, u *unstructured.Unstructured, org *orgv1alpha1.Organization, req DeleteRequest, res *DeleteResult) (answered bool, err error) {
	cm, err := s.newCommit(ctx)
	if err != nil {
		return false, err
	}
	p, file, err := s.gitObject(ctx, cm, u)
	if err != nil {
		return false, err
	}
	what := "Organization " + req.Name
	object := branch("delete", req.Name)
	prURL := org.Annotations[DeletePullRequestAnnotation]
	// merged tells only that the pull request the object names merged;
	// whether that merge removed the file is asked of its files (changes)
	// once the file is gone from the base. Both come from the pull request
	// reads alone: its reviews, statuses and check runs are not read, which
	// the App's permissions do not cover. listErr is a listing that failed.
	var (
		pr      commit.PullRequest
		merged  bool
		changes []commit.FileChange
		listErr error
	)
	if prURL != "" {
		var ok bool
		if pr, ok = cm.pullRequest(prURL); ok {
			merged, changes, listErr = cm.mergedFiles(ctx, pr)
		}
	}

	if file != nil {
		// While the file is in git, merged only words the answer: a
		// listing that fails leaves it false, so restoring the file and
		// opening the delete again recovers from a listing that keeps
		// failing. A refused token is still answered.
		if _, refused := errors.AsType[*commit.AuthError](listErr); refused {
			return false, commitError(cm.gh, listErr)
		}
		if req.Confirm != "" {
			msg := fmt.Sprintf("%s is still in git (%s in %s): a live delete is refused while it is, since Flux would re-create it", what, p, cm.cfg.Objects.Repository)
			if merged {
				msg += fmt.Sprintf("; the merged pull request %s did not remove it, so it is not this organization's delete", prURL)
			}
			return false, fmt.Errorf("%w: %s. Call delete_organization with mode commit and no confirm to open the pull request that removes it", agents.ErrConflict, msg)
		}
		// The delete pull request agent-manager opened is open: the object
		// carries it, or a mark that failed after the pull request opened is
		// written now.
		open, has, err := cm.openPullRequest(ctx, object)
		if err != nil {
			return false, err
		}
		if has {
			if prURL != open.URL && !req.DryRun {
				if err := s.markDelete(ctx, c, req.Name, open.URL, false); err != nil {
					return false, err
				}
			}
			res.Next = fmt.Sprintf("the pull request that removes the organization from git is open: %s; %s. Once it is merged, call delete_organization again with mode commit and confirm %q to erase it", open.URL, reviewNote, req.Name)
			return true, nil
		}
		return true, s.openDelete(ctx, c, cm, req, res, p, prURL, merged)
	}

	// The file is gone from git: only this organization's merged delete
	// pull request makes it pending erase.
	switch {
	case prURL == "":
		return false, fmt.Errorf("%w: %s is gone from git (%s) but no delete pull request of agent-manager removed it: it is Missing from git, not pending erase, and is not erased; restore its file, or open the delete with mode commit once it is back", agents.ErrConflict, what, p)
	case listErr != nil:
		// A pull request of more than 3000 files never lists whole: GitHub
		// lists at most 3000. The way forward is a new delete that is small:
		// the merge removed the content path already, so a delete opened
		// once the object's file alone is back changes the objects path only.
		return false, fmt.Errorf("%w (nothing was erased: the files of the delete pull request %s could not be listed, so whether it removed %s is unknown. If the listing failed for a while, call delete_organization again with mode commit and confirm to retry. If it keeps failing, as it does for a pull request of more than 3000 files, which GitHub never lists whole, restore only %s, not the content path, and call delete_organization with mode commit and no confirm: the new delete pull request changes the objects path only, so its files list)", commitError(cm.gh, listErr), pr.URL, p, p)
	case !merged:
		return false, fmt.Errorf("%w: %s is gone from git (%s), but its delete pull request %s is not merged: it is Missing from git, not pending erase, and is not erased; restore its file", agents.ErrConflict, what, p, prURL)
	}
	if err := removedBy(changes, pr, what, p); err != nil {
		return false, err
	}
	if err := cm.stillInGit(ctx, pr, req.Name, what, p); err != nil {
		return false, err
	}
	if req.Confirm != req.Name {
		res.Next = fmt.Sprintf("pending erase: the delete pull request %s is merged and the file is gone from git. Nothing was deleted: show the person what is erased and, once they typed it, call delete_organization again with mode commit and confirm %q (Erase now)", prURL, req.Name)
		return true, nil
	}
	return false, nil
}

// removedBy refuses the erase unless the merged pull request pr removed the
// organization's file p itself: another merge may have removed it, or the
// pull request may have been edited before its merge. A move of the file
// that GitHub reports as a rename is refused although commit.Removes counts
// it: the moved file may still define the organization in git. A move
// whose content changed so much that GitHub reports a removal and an
// addition is refused by stillInGit. The object's file alone decides:
// the content path is applied by the organization's own Kustomization,
// which the erase removes, and a create refuses a content path an earlier
// organization left. changes are the merged pull request's files; the
// caller erases nothing when they could not be listed.
func removedBy(changes []commit.FileChange, pr commit.PullRequest, what, p string) error {
	for _, c := range changes {
		if c.Kind == commit.FileRenamed && c.PreviousPath == p {
			return fmt.Errorf("%w: %s: its merged delete pull request %s moved its file to %s instead of removing it, so the organization may still be in git; it is not pending erase and is not erased; move the file back to %s, %s", agents.ErrConflict, what, pr.URL, c.Path, p, nextDelete)
		}
	}
	if !commit.Removes(changes, p) {
		return fmt.Errorf("%w: %s is gone from git (%s), but its merged delete pull request %s did not remove it: it is Missing from git, not pending erase, and is not erased; restore its file, %s", agents.ErrConflict, what, p, pr.URL, nextDelete)
	}
	return nil
}

// nextDelete is the way forward after a merged delete pull request that did
// not take the organization out of git.
const nextDelete = "then call delete_organization again with mode commit and no confirm to open a new delete pull request"

// stillInGit refuses the erase while a file under the objects path on the
// base branch still defines the organization name: a move that GitHub
// reports as a removal and an addition, a definition the delete pull request
// wrote into another file, or one a later commit wrote or moved. Both the
// erase and the answer pending erase take it. The installation's
// Kustomization applies the objects path, so a definition anywhere else
// re-creates the object only through a kustomization under the objects path
// that loads it: such an entry (leavingEntries) refuses the erase, since the
// scan does not read what it loads (giantswarm/agent-manager#437). Every
// kustomization under the objects path counts, also one that the build does
// not reach. Every file under the objects path is read, whatever its name,
// since Kustomize loads a resource file by any name: one read of the
// branch's head, one tree read and two reads for each file, all at that one
// commit, so a commit that lands during the scan cannot hide a definition.
// A resource file that does not parse defines nothing, since Kustomize
// cannot apply it either; a kustomization that does not parse counts as
// below. A read that fails, or a tree too large for one read, erases
// nothing.
//
// An Organization is cluster-scoped: its name alone is its identity, and a
// namespace a kustomization sets does not change it. The scan runs no
// Kustomize, so it fails closed on what a kustomization changes of a name,
// wherever that kustomization is under the objects path (a sibling overlay
// refers to a file by ../, giantswarm/agent-manager#399): every organization
// document that one may make name counts (renamingOf). A name prefix or
// suffix makes a document whose name is a part of name into it; a
// transformer, a patch, replacements, vars with configurations, a
// kustomization that does not parse, or a value the scan cannot read
// (${...}, $(...), SOPS) makes any organization document count. In a
// repository with such an overlay an erase can be refused although Flux's
// build no longer holds the organization: the answer names the overlay to
// remove or change.
func (c *orgCommit) stillInGit(ctx context.Context, pr commit.PullRequest, name, what, p string) error {
	repo, base, dir := c.cfg.Objects.Repository, c.cfg.Objects.Branch, c.cfg.Objects.Directory
	unknown := func(err error, step string) error {
		return fmt.Errorf("%w (nothing was erased: %s could not be read, so whether the organization is still defined in git is unknown; call delete_organization again with mode commit and confirm to retry)", commitError(c.gh, err), step)
	}
	// One commit for every read: a branch name in each read would resolve
	// the head again, and a commit between the list and the reads could move
	// a definition out of both.
	sha, err := c.remote.Head(ctx, repo, base)
	if err != nil {
		return unknown(err, "the head of "+base)
	}
	files, err := c.remote.ListFiles(ctx, repo, sha, dir)
	if err != nil {
		return unknown(err, "the objects path "+dir)
	}
	docs := map[string][]map[string]any{}
	var (
		r         renaming
		renamedBy []string
		// leaving are the kustomizations' entries that load what the scan
		// does not read.
		leaving []string
		tree    = treeOf(files)
	)
	for _, f := range files {
		raw, err := c.remote.ReadFile(ctx, repo, sha, f)
		if errors.Is(err, commit.ErrFileNotFound) {
			// Listed at the same commit, so not a file a later commit
			// moved: a symlink out of the repository, which Kustomize
			// cannot load either.
			continue
		}
		if err != nil {
			return unknown(err, f)
		}
		parsed, ok := documents(raw)
		// A file of a kustomization's name is read both ways: as a
		// kustomization, and as a resource file, since another
		// kustomization may list it as one. A kustomization's own document
		// never is an organization.
		if kustomizationFiles[path.Base(f)] {
			if k, why := renamingOf(parsed, ok); why != nil {
				r.affix = r.affix || k.affix
				r.name = r.name || k.name
				renamedBy = append(renamedBy, f+" ("+strings.Join(why, ", ")+")")
			}
			for _, e := range leavingEntries(parsed, ok, path.Dir(f), dir, tree) {
				leaving = append(leaving, f+" ("+e+")")
			}
		}
		docs[f] = parsed
	}
	for _, f := range files {
		for _, doc := range docs[f] {
			if !definesOrganization(doc, name, r) {
				continue
			}
			if !definesOrganization(doc, name, renaming{}) {
				// The overlay alone makes the match: the document may be
				// another organization, which must not be removed blindly.
				return fmt.Errorf("%w: %s: its merged delete pull request %s removed %s, but %s in %s may still define it: %s may change a name, so what the document says of its own name does not tell, and the scan, which runs no Kustomize, takes it as a definition; it is not pending erase and is not erased. Check the overlay first: if it does not make that document %s, remove that setting or the overlay from the objects path and call delete_organization again with mode commit and confirm %q; if it does, remove that definition", agents.ErrConflict, what, pr.URL, p, f, repo, strings.Join(renamedBy, ", "), name, name)
			}
			return fmt.Errorf("%w: %s: its merged delete pull request %s removed %s, but %s in %s still defines it (moved there, or written there by the pull request or a later commit), so the organization is still in git; it is not pending erase and is not erased; remove that definition from %s and call delete_organization again with mode commit and confirm %q, or move it back to %s, %s", agents.ErrConflict, what, pr.URL, p, f, repo, f, name, p, nextDelete)
		}
	}
	if len(leaving) > 0 {
		return fmt.Errorf("%w: %s: its merged delete pull request %s removed %s, but a kustomization under the objects path %s in %s loads files from outside it: %s. The scan reads only the objects path and runs no Kustomize, so whether what that entry loads still defines the organization is unknown; it is not pending erase and is not erased. Bring what the entry loads under the objects path, or remove the entry, then call delete_organization again with mode commit and confirm %q", agents.ErrConflict, what, pr.URL, p, dir, repo, strings.Join(leaving, ", "), name)
	}
	return nil
}

// loadFields are the kustomization fields whose entries name what
// Kustomize loads as resources: a file, a directory or a remote.
var loadFields = []string{"resources", "components", "bases"}

// leavingEntries lists the entries of the kustomization docs, in directory
// kdir, that load what is not under the objects path dir, each with its
// field and why: a remote (it holds :// or //), a path that leaves dir once
// joined with kdir, or one that names no file or directory of the tree
// under dir, which Kustomize then loads as a remote or fails on. A field of
// a form the scan does not know counts too. A kustomization that does not
// parse lists nothing: Kustomize cannot build it, and renamingOf already
// counts it.
func leavingEntries(docs []map[string]any, ok bool, kdir, dir string, tree map[string]bool) []string {
	if !ok || len(docs) == 0 {
		return nil
	}
	k := foldKeys(docs[0])
	dir = path.Clean(dir)
	var out []string
	for _, field := range loadFields {
		v, set := k[field]
		if !set || v == nil {
			continue
		}
		entries, isList := v.([]any)
		if !isList {
			out = append(out, field+" of a form the scan does not know")
			continue
		}
		for _, e := range entries {
			s, isString := e.(string)
			switch {
			case !isString:
				out = append(out, field+" entry of a form the scan does not know")
			case strings.Contains(s, "//") || remoteForm(s):
				// :// holds // too.
				out = append(out, fmt.Sprintf("%s entry %q is a remote", field, s))
			case path.IsAbs(s):
				out = append(out, fmt.Sprintf("%s entry %q leaves the objects path", field, s))
			default:
				j := path.Join(kdir, s)
				switch {
				case j != dir && !strings.HasPrefix(j, dir+"/"):
					out = append(out, fmt.Sprintf("%s entry %q leaves the objects path", field, s))
				case !tree[j]:
					out = append(out, fmt.Sprintf("%s entry %q names nothing under the objects path, so Kustomize loads it as a remote or fails on it", field, s))
				}
			}
		}
	}
	return out
}

// remoteForm reports whether the entry s has a form that Kustomize parses
// as a remote although it holds no //: git@host:owner/repo, gh:owner/repo,
// or github.com/owner/repo/path, which Kustomize may load as a remote
// before a local directory of that path.
func remoteForm(s string) bool {
	first, _, _ := strings.Cut(s, "/")
	return strings.HasPrefix(s, "git@") || strings.HasPrefix(s, "gh:") || first == "github.com"
}

// treeOf is the set of the files and of every directory above them, up to
// the repository's root.
func treeOf(files []string) map[string]bool {
	out := make(map[string]bool, 2*len(files))
	for _, f := range files {
		for p := f; p != "." && p != "/" && !out[p]; p = path.Dir(p) {
			out[p] = true
		}
	}
	return out
}

// kustomizationFiles are the names Kustomize reads a kustomization from.
var kustomizationFiles = map[string]bool{"kustomization.yaml": true, "kustomization.yml": true, "Kustomization": true}

// rewriteFields are the kustomization fields that can set a resource's
// name to any value: the scan runs no Kustomize, so it does not read what
// they write (giantswarm/agent-manager#423).
var rewriteFields = []string{"transformers", "patches", "patchesJson6902", "patchesStrategicMerge", "replacements"}

// renaming is what kustomizations may change of a resource's name on its
// way to the organization name: affix, a document whose own name is a part
// of name may become it (a prefix or a suffix only adds text); name, a
// document of any name may become it.
type renaming struct{ affix, name bool }

// renamingOf reads what the kustomization docs (ok false: it does not
// parse) may change of a resource's name, and why, nil when nothing. It
// fails closed: a kustomization it cannot parse, a field of a form it does
// not know, a value it cannot read (opaque) or one of rewriteFields counts
// as changing what it may. A namespace does not count: an Organization is
// cluster-scoped.
func renamingOf(docs []map[string]any, ok bool) (renaming, []string) {
	if !ok {
		return renaming{name: true}, []string{"it does not parse"}
	}
	if len(docs) == 0 {
		return renaming{}, nil
	}
	k := foldKeys(docs[0])
	var (
		r   renaming
		why []string
	)
	for _, field := range []string{"namePrefix", "nameSuffix"} {
		switch v := k[strings.ToLower(field)].(type) {
		case nil:
		case string:
			if v != "" {
				r.affix = true
				why = append(why, field)
			}
		default:
			r.name = true
			why = append(why, field+" of a form the scan does not know")
		}
	}
	for _, field := range rewriteFields {
		if nonEmpty(k[strings.ToLower(field)]) {
			r.name = true
			why = append(why, field)
		}
	}
	// A var substitutes $(NAME) in the fields a configurations file's
	// varReference names, metadata/name among them.
	if nonEmpty(k["vars"]) && nonEmpty(k["configurations"]) {
		r.name = true
		why = append(why, "vars with configurations")
	}
	return r, why
}

// nonEmpty reports whether a kustomization field's value v is set: an
// empty string, list or map is not; a value of another form counts.
func nonEmpty(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return true
	}
}

// foldKeys is the kustomization k with its keys in lower case: Kustomize
// decodes a kustomization with encoding/json, which matches a field's key
// whatever its case. Two keys that fold to one give a value of a form the
// scan does not know.
func foldKeys(k map[string]any) map[string]any {
	out := make(map[string]any, len(k))
	for key, v := range k {
		key = strings.ToLower(key)
		if _, twice := out[key]; twice {
			v = twiceSet{}
		}
		out[key] = v
	}
	return out
}

// twiceSet is the value of a kustomization field whose key is set twice,
// in two cases.
type twiceSet struct{}

// sealed reports whether s is a value SOPS encrypted (ENC[...]).
func sealed(s string) bool { return strings.HasPrefix(s, "ENC[") }

// opaque reports whether the scan cannot read the value s: SOPS encrypted
// it, or it holds a variable that Flux's post-build substitution (${...}) or
// a Kustomize var ($(...)) may replace.
func opaque(s string) bool {
	return sealed(s) || strings.Contains(s, "${") || strings.Contains(s, "$(")
}

// documents parses raw's YAML or JSON documents; ok is false when one does
// not parse, since Kustomize cannot apply such a file.
func documents(raw []byte) (out []map[string]any, ok bool) {
	dec := kubeyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, true
		}
		if err != nil {
			return nil, false
		}
		if doc != nil {
			out = append(out, doc)
		}
	}
}

// definesOrganization reports whether doc is, or as a list (kind *List)
// holds in its items, the Organization name, as Kustomize applies it: one
// whose name r says a kustomization may change counts. A value the scan
// cannot read (opaque) is not known, so it counts too. Its namespace does
// not count: an Organization is cluster-scoped.
func definesOrganization(doc map[string]any, name string, r renaming) bool {
	u := unstructured.Unstructured{Object: doc}
	if items, ok := doc["items"].([]any); ok && strings.HasSuffix(u.GetKind(), "List") {
		for _, it := range items {
			if m, ok := it.(map[string]any); ok && definesOrganization(m, name, r) {
				return true
			}
		}
		return false
	}
	kind := u.GetKind() == orgKind && u.GroupVersionKind().Group == GVR.Group
	if _, sops := doc["sops"]; sops && (sealed(u.GetKind()) || sealed(u.GetAPIVersion())) {
		kind = true
	}
	own := u.GetName()
	return kind && (r.name || own == name || opaque(own) || (r.affix && strings.Contains(name, own)))
}

// openDelete opens the pull request that removes the organization's file
// and its whole content path, then marks the live object with it, as the
// caller: the mark is dry-run before the pull request exists.
func (s *Service) openDelete(ctx context.Context, c kube.Client, cm *orgCommit, req DeleteRequest, res *DeleteResult, p, earlier string, earlierMerged bool) error {
	if err := s.markDelete(ctx, c, req.Name, "https://github.com/"+cm.cfg.Objects.Repository.String()+"/pull/0", true); err != nil {
		return err
	}
	content := cm.contentDir(req.Name)
	files, err := cm.list(ctx, content.Path())
	if err != nil {
		return err
	}
	ch := fileChange{dir: content}
	for _, f := range files {
		if path.Base(f) == layout.KustomizationFile {
			// layout.Build takes no kustomization.yaml; the whole path
			// goes, so each one goes too, the content path's own whatever
			// Build made of it.
			ch.drop = append(ch.drop, f)
			continue
		}
		ch.remove = append(ch.remove, f)
	}
	changes := []fileChange{{dir: cm.objectDir(), remove: []string{p}, objects: true}}
	if len(ch.remove) > 0 || len(ch.drop) > 0 {
		changes = append(changes, ch)
	}
	title := "feat(organizations): remove organization " + req.Name
	body := fmt.Sprintf("Removes the organization %s from git: its Organization file and its whole content path %s (agents, models, connectors, egress policies, members and credentials), so nothing in git re-creates them under its name. The merge erases nothing: the organization shows Pending erase, and Erase now (delete_organization with mode commit and the typed confirmation) erases it.", req.Name, content.Path())
	res.Commit, err = cm.open(ctx, changes, branch("delete", req.Name), title, body, req.DryRun)
	if err != nil {
		return err
	}
	if earlier != "" && earlierMerged {
		res.Next = fmt.Sprintf("the merged pull request %s did not remove the organization's file, so it did not count as its delete; ", earlier)
	}
	if req.DryRun {
		res.Next += "dry run: no pull request was opened and the object was not marked; the files are in commit.files"
		return nil
	}
	if err := s.markDelete(ctx, c, req.Name, res.Commit.PullRequest, false); err != nil {
		return fmt.Errorf("%w (the pull request %s is open; call delete_organization again with mode commit and no confirm to mark the object)", err, res.Commit.PullRequest)
	}
	step := fmt.Sprintf("after the merge the organization shows Pending erase (AgentPlatformOrphaned, reason DeleteMerged); then call delete_organization with mode commit and confirm %q to erase it (Erase now). Until then nothing is erased", req.Name)
	res.Commit.LiveSteps = []string{step}
	res.Next += "the pull request " + res.Commit.PullRequest + " removes the organization from git; " + reviewNote + "; " + step
	s.log.Info("organization delete committed", agents.UserAttr(ctx), "name", req.Name, "pullRequest", res.Commit.PullRequest)
	return nil
}

// markDelete marks the live organization with its delete pull request, as
// the caller, on the object as it is now: dry run first, and only the dry
// run when dryRun.
func (s *Service) markDelete(ctx context.Context, c kube.Client, name, url string, dryRun bool) error {
	what := "Organization " + name
	way := wayOrganizationsAdmin()
	u, err := c.Dynamic().Resource(GVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return kubeErr(err, "get "+what, way)
	}
	setAnnotation(u, DeletePullRequestAnnotation, url)
	return apply(ctx, c.Dynamic(), []write{{gvr: GVR, kind: orgKind, op: opUpdate, obj: u.Object, name: name, what: what + " (" + DeletePullRequestAnnotation + ")", reason: way}}, dryRun)
}

// mergedFiles reads whether pr is merged and, when it is, the files it
// changed, with the pull request reads alone (commit.MergedFilesLister):
// never its reviews or the statuses and check runs of its head, which the
// App's permissions do not cover. One not merged is merged false, no error.
func (c *orgCommit) mergedFiles(ctx context.Context, pr commit.PullRequest) (bool, []commit.FileChange, error) {
	changes, err := c.remote.MergedFiles(ctx, pr)
	switch {
	case errors.Is(err, commit.ErrNotMerged):
		return false, nil, nil
	case err != nil:
		return false, nil, err
	}
	return true, changes, nil
}

// pullRequest reads a pull request URL of the organizations repository:
// https://github.com/<owner>/<name>/pull/<number>. Any other URL is not
// agent-manager's delete.
func (c *orgCommit) pullRequest(raw string) (commit.PullRequest, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return commit.PullRequest{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" || parts[0]+"/"+parts[1] != c.cfg.Objects.Repository.String() {
		return commit.PullRequest{}, false
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return commit.PullRequest{}, false
	}
	return commit.PullRequest{Repository: c.cfg.Objects.Repository, Number: n, URL: raw, Base: c.cfg.Objects.Branch}, true
}
