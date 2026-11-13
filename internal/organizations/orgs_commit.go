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

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/gitops-commit/layout"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kube"
)

const orgKind = "AgentPlatformOrganization"

// createCommit opens one pull request with the organization's file under its
// parent and the organization-admins RoleBinding in its content path, after
// the access reviews Create asked and a dry run of the object in its parent
// as the caller. The operator mirrors the caller's organizations-admin
// binding of the parent into the new namespace, so those reviews are the
// right to bind the first admins there.
func (s *Service) createCommit(ctx context.Context, c kube.Client, req CreateRequest, subjects []Subject, res *CreateResult) (*CreateResult, error) {
	if err := segment("namespace", req.Parent); err != nil {
		return nil, err
	}
	if err := segment("name", req.Name); err != nil {
		return nil, err
	}
	what := fmt.Sprintf("AgentPlatformOrganization %s/%s", req.Parent, req.Name)
	way := wayOrganizationsAdmin(req.Parent)
	_, err := c.Dynamic().Resource(GVR).Namespace(req.Parent).Get(ctx, req.Name, metav1.GetOptions{})
	switch {
	case err == nil:
		return nil, fmt.Errorf("%w: %s exists; update_organization changes it", agents.ErrConflict, what)
	case !apierrors.IsNotFound(err):
		return nil, kubeErr(err, "get "+what, way)
	}
	cm, err := s.newCommit(ctx)
	if err != nil {
		return nil, err
	}
	objFile := cm.objectFile(req.Parent, req.Name)
	if ok, err := cm.exists(ctx, objFile); err != nil || ok {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s is in %s already and not applied yet: wait for Flux to apply it", agents.ErrConflict, objFile, cm.cfg.Objects.Repository)
	}
	content := cm.contentDir(req.Parent, req.Name)
	// A new organization starts from an empty content path: files an
	// earlier organization of that name left would come back with it.
	if left, err := cm.list(ctx, content.Path()); err != nil || len(left) > 0 {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: the content path %s in %s holds files (%s) an earlier organization left: remove them first, they would be applied into the new organization", agents.ErrConflict, content.Path(), cm.cfg.Objects.Repository, strings.Join(left, ", "))
	}

	obj := unstructuredOf(s.organizationObject(req))
	// The file never leaves git into an erase: only an explicit delete of
	// the live object erases.
	setAnnotation(obj, PruneAnnotation, PruneDisabled)
	if err := apply(ctx, c.Dynamic(), []write{{gvr: GVR, kind: orgKind, op: opCreate, obj: obj.Object, ns: req.Parent, name: req.Name, what: what, reason: way}}, true); err != nil {
		return nil, err
	}
	binding := unstructuredOf(roleBinding("", FirstAdminsBinding, RoleAdmin, subjects))
	setAnnotation(binding, PruneAnnotation, PruneDisabled)
	isSecret, err := cm.secretFiles(ctx)
	if err != nil {
		return nil, err
	}
	if err := refusePlainAsSecret(isSecret, objFile, orgKind, req.Name); err != nil {
		return nil, err
	}
	if err := refusePlainAsSecret(isSecret, contentFile(content, kindRoleBinding, FirstAdminsBinding), kindRoleBinding, FirstAdminsBinding); err != nil {
		return nil, err
	}

	bindingFile := contentFile(content, kindRoleBinding, FirstAdminsBinding)
	changes := []fileChange{
		{dir: cm.objectDir(req.Parent), write: map[string][]byte{objFile: manifest(obj, true)}, create: []string{objFile}},
		{dir: content, write: map[string][]byte{bindingFile: manifest(binding, false)}, create: []string{bindingFile}},
	}
	title := fmt.Sprintf("feat(organizations): add organization %s/%s", req.Parent, req.Name)
	body := fmt.Sprintf("Adds the Agent Platform organization %s/%s: its object under its parent, and its first admins (%s, bound to %s) in its content path %s, which its own Flux Kustomization applies.", req.Parent, req.Name, FirstAdminsBinding, RoleAdmin, content.Path())
	res.Commit, err = cm.open(ctx, changes, branch("create", req.Parent, req.Name), title, body, req.DryRun)
	if err != nil {
		return nil, err
	}
	planned, err := organizationOf(obj)
	if err != nil {
		return nil, err
	}
	res.Organization = s.summary(planned)
	res.FirstAdmins = &Binding{Name: FirstAdminsBinding, Role: RoleAdmin, Subjects: subjects}
	res.Commit.LiveSteps = []string{fmt.Sprintf("after the merge Flux applies the object; agent-platform-operator creates the organization namespace and the organization's Kustomization, which applies %s from %s", FirstAdminsBinding, content.Path())}
	if req.DryRun {
		res.Pending = "dry run: no pull request was opened; the files are in commit.files"
	} else {
		res.Pending = "merge the pull request " + res.Commit.PullRequest + "; " + reviewNote
		s.log.Info("organization create committed", identity.LogAttr(ctx), "parent", req.Parent, "name", req.Name, "pullRequest", res.Commit.PullRequest)
	}
	return res, nil
}

// gitObject is the file of a git-owned organization: refused when the
// object was written live, or when another file defines it.
func (s *Service) gitObject(ctx context.Context, cm *orgCommit, u *unstructured.Unstructured) (string, *unstructured.Unstructured, error) {
	if !fromGit(u) {
		return "", nil, fmt.Errorf("%w: AgentPlatformOrganization %s/%s was written live, not applied from git: use mode apply", agents.ErrConflict, u.GetNamespace(), u.GetName())
	}
	p := cm.objectFile(u.GetNamespace(), u.GetName())
	file, err := cm.read(ctx, p)
	if err != nil {
		return "", nil, err
	}
	return p, file, nil
}

// updateCommit changes the organization's file as an update would change
// the object, after a dry run of the same change live as the caller.
func (s *Service) updateCommit(ctx context.Context, c kube.Client, u *unstructured.Unstructured, org *apov1alpha1.AgentPlatformOrganization, req UpdateRequest) (*UpdateResult, error) {
	cm, err := s.newCommit(ctx)
	if err != nil {
		return nil, err
	}
	p, file, err := s.gitObject(ctx, cm, u)
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, fmt.Errorf("%w: AgentPlatformOrganization %s/%s is applied from git, but not from %s in %s: change it in the file that defines it", agents.ErrConflict, req.Parent, req.Name, p, cm.cfg.Objects.Repository)
	}
	res := &UpdateResult{Before: s.summary(org), DryRun: req.DryRun, RequestedBy: identity.Caller(ctx)}
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
	what := fmt.Sprintf("AgentPlatformOrganization %s/%s", req.Parent, req.Name)
	if err := apply(ctx, c.Dynamic(), []write{{gvr: GVR, kind: orgKind, op: opUpdate, obj: next.Object, ns: req.Parent, name: req.Name, what: what, reason: wayOrganizationsAdmin(req.Parent)}}, true); err != nil {
		return nil, err
	}
	title := fmt.Sprintf("chore(organizations): update organization %s/%s", req.Parent, req.Name)
	body := fmt.Sprintf("Changes %s of the Agent Platform organization %s/%s.", strings.Join(res.Changed, ", "), req.Parent, req.Name)
	res.Commit, err = cm.open(ctx, []fileChange{{dir: cm.objectDir(req.Parent), write: map[string][]byte{p: manifest(file, true)}}}, branch("update", req.Parent, req.Name), title, body, req.DryRun)
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
func (s *Service) deleteCommit(ctx context.Context, c kube.Client, u *unstructured.Unstructured, org *apov1alpha1.AgentPlatformOrganization, req DeleteRequest, res *DeleteResult) (answered bool, err error) {
	cm, err := s.newCommit(ctx)
	if err != nil {
		return false, err
	}
	p, file, err := s.gitObject(ctx, cm, u)
	if err != nil {
		return false, err
	}
	what := fmt.Sprintf("AgentPlatformOrganization %s/%s", req.Parent, req.Name)
	head := branch("delete", req.Parent, req.Name)
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
		open, has, err := cm.remote.FindPullRequest(ctx, cm.cfg.Objects.Repository, head)
		if err != nil {
			return false, commitError(cm.gh, err)
		}
		if has {
			if prURL != open.URL && !req.DryRun {
				if err := s.markDelete(ctx, c, req.Parent, req.Name, open.URL, false); err != nil {
					return false, err
				}
			}
			res.Next = fmt.Sprintf("the pull request that removes the organization from git is open: %s; %s. Once it is merged, call delete_organization again with mode commit and confirm %q to erase it", open.URL, reviewNote, req.Parent+"/"+req.Name)
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
	if err := cm.stillInGit(ctx, pr, req.Parent, req.Name, what, p); err != nil {
		return false, err
	}
	if req.Confirm != req.Parent+"/"+req.Name {
		res.Next = fmt.Sprintf("pending erase: the delete pull request %s is merged and the file is gone from git. Nothing was deleted: show the person what is erased and, once they typed it, call delete_organization again with mode commit and confirm %q (Erase now)", prURL, req.Parent+"/"+req.Name)
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
// base branch still defines the organization parent/name: a move that
// GitHub reports as a removal and an addition, a definition the delete pull
// request wrote into another file, or one a later commit wrote or moved.
// The installation's Kustomization applies the objects path alone, so a
// definition anywhere else does not re-create the object. Every file under
// the objects path is read, whatever its name, since Kustomize loads a
// resource file by any name: one tree read, and two reads for each file. A
// file that does not parse defines nothing, since Kustomize cannot apply
// it either. A read that fails, or a tree too large for one read, erases
// nothing. The scan gives an early, clear answer; it runs no Kustomize, so
// a sibling overlay that sets the namespace, a resource file named
// kustomization.yaml (giantswarm/agent-manager#399) and a move between its
// reads (giantswarm/gitops-commit#70) pass it. The admission policy
// agent-platform-organization-delete-intent still refuses those erases: it
// needs Orphaned DeleteMerged, which the operator sets from the inventory
// of Flux's own build (docs/organizations-commit-mode.md).
func (c *orgCommit) stillInGit(ctx context.Context, pr commit.PullRequest, parent, name, what, p string) error {
	repo, base, dir := c.cfg.Objects.Repository, c.cfg.Objects.Branch, c.cfg.Objects.Directory
	unknown := func(err error, step string) error {
		return fmt.Errorf("%w (nothing was erased: %s could not be read, so whether the organization is still defined in git is unknown; call delete_organization again with mode commit and confirm to retry)", commitError(c.gh, err), step)
	}
	files, err := c.remote.ListFiles(ctx, repo, base, dir)
	if err != nil {
		return unknown(err, "the objects path "+dir)
	}
	docs := map[string][]map[string]any{}
	renames := map[string]renaming{}
	for _, f := range files {
		raw, err := c.remote.ReadFile(ctx, repo, base, f)
		if errors.Is(err, commit.ErrFileNotFound) {
			continue
		}
		if err != nil {
			return unknown(err, f)
		}
		parsed := documents(raw)
		if kustomizationFiles[path.Base(f)] {
			if len(parsed) > 0 {
				renames[path.Dir(f)] = renamingOf(parsed[0])
			}
			continue
		}
		docs[f] = parsed
	}
	for _, f := range files {
		// A kustomization above the file that sets a namespace, a name
		// prefix or a name suffix changes what its documents say: their
		// raw namespace or name then does not tell.
		var r renaming
		for d := path.Dir(f); within(dir, d); d = path.Dir(d) {
			r.namespace = r.namespace || renames[d].namespace
			r.name = r.name || renames[d].name
		}
		for _, doc := range docs[f] {
			if definesOrganization(doc, parent, name, r) {
				return fmt.Errorf("%w: %s: its merged delete pull request %s removed %s, but %s in %s still defines it (moved there, or written there by the pull request or a later commit), so the organization is still in git; it is not pending erase and is not erased; remove that definition from %s and call delete_organization again with mode commit and confirm %q, or move it back to %s, %s", agents.ErrConflict, what, pr.URL, p, f, repo, f, parent+"/"+name, p, nextDelete)
			}
		}
	}
	return nil
}

// kustomizationFiles are the names Kustomize reads a kustomization from.
var kustomizationFiles = map[string]bool{"kustomization.yaml": true, "kustomization.yml": true, "Kustomization": true}

// renaming is what a kustomization changes of its resources' identity.
type renaming struct{ namespace, name bool }

func renamingOf(k map[string]any) renaming {
	set := func(field string) bool { s, _ := k[field].(string); return s != "" }
	return renaming{namespace: set("namespace"), name: set("namePrefix") || set("nameSuffix")}
}

// documents parses raw's YAML or JSON documents; nil when one does not
// parse, since Kustomize cannot apply such a file.
func documents(raw []byte) []map[string]any {
	dec := kubeyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var out []map[string]any
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			return nil
		}
		if doc != nil {
			out = append(out, doc)
		}
	}
}

// definesOrganization reports whether doc is, or as a list (kind *List)
// holds in its items, the AgentPlatformOrganization parent/name, as
// Kustomize applies it. A document without a namespace counts, and so does
// one whose namespace or name r says a kustomization changes. A value SOPS
// encrypted (ENC[...]) is not known, so it counts too.
func definesOrganization(doc map[string]any, parent, name string, r renaming) bool {
	u := unstructured.Unstructured{Object: doc}
	if items, ok := doc["items"].([]any); ok && strings.HasSuffix(u.GetKind(), "List") {
		for _, it := range items {
			if m, ok := it.(map[string]any); ok && definesOrganization(m, parent, name, r) {
				return true
			}
		}
		return false
	}
	sealed := func(s string) bool { return strings.HasPrefix(s, "ENC[") }
	kind := u.GetKind() == orgKind && u.GroupVersionKind().Group == GVR.Group
	if _, sops := doc["sops"]; sops && (sealed(u.GetKind()) || sealed(u.GetAPIVersion())) {
		kind = true
	}
	ns := u.GetNamespace()
	return kind &&
		(r.name || u.GetName() == name || sealed(u.GetName())) &&
		(r.namespace || ns == "" || ns == parent || sealed(ns))
}

// openDelete opens the pull request that removes the organization's file
// and its whole content path, then marks the live object with it, as the
// caller: the mark is dry-run before the pull request exists.
func (s *Service) openDelete(ctx context.Context, c kube.Client, cm *orgCommit, req DeleteRequest, res *DeleteResult, p, earlier string, earlierMerged bool) error {
	if err := s.markDelete(ctx, c, req.Parent, req.Name, "https://github.com/"+cm.cfg.Objects.Repository.String()+"/pull/0", true); err != nil {
		return err
	}
	content := cm.contentDir(req.Parent, req.Name)
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
	changes := []fileChange{{dir: cm.objectDir(req.Parent), remove: []string{p}}}
	if len(ch.remove) > 0 || len(ch.drop) > 0 {
		changes = append(changes, ch)
	}
	title := fmt.Sprintf("feat(organizations): remove organization %s/%s", req.Parent, req.Name)
	body := fmt.Sprintf("Removes the Agent Platform organization %s/%s from git: its object's file and its whole content path %s (agents, models, connectors, egress policies, members and credentials), so nothing in git re-creates them under its name. The merge erases nothing: the organization shows Pending erase, and Erase now (delete_organization with mode commit and the typed confirmation) erases it.", req.Parent, req.Name, content.Path())
	res.Commit, err = cm.open(ctx, changes, branch("delete", req.Parent, req.Name), title, body, req.DryRun)
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
	if err := s.markDelete(ctx, c, req.Parent, req.Name, res.Commit.PullRequest, false); err != nil {
		return fmt.Errorf("%w (the pull request %s is open; call delete_organization again with mode commit and no confirm to mark the object)", err, res.Commit.PullRequest)
	}
	step := fmt.Sprintf("after the merge the organization shows Pending erase (Orphaned, reason DeleteMerged); then call delete_organization with mode commit and confirm %q to erase it (Erase now). Until then nothing is erased", req.Parent+"/"+req.Name)
	res.Commit.LiveSteps = []string{step}
	res.Next += "the pull request " + res.Commit.PullRequest + " removes the organization from git; " + reviewNote + "; " + step
	s.log.Info("organization delete committed", identity.LogAttr(ctx), "parent", req.Parent, "name", req.Name, "pullRequest", res.Commit.PullRequest)
	return nil
}

// markDelete marks the live organization with its delete pull request, as
// the caller, on the object as it is now: dry run first, and only the dry
// run when dryRun.
func (s *Service) markDelete(ctx context.Context, c kube.Client, parent, name, url string, dryRun bool) error {
	what := fmt.Sprintf("AgentPlatformOrganization %s/%s", parent, name)
	way := wayOrganizationsAdmin(parent)
	u, err := c.Dynamic().Resource(GVR).Namespace(parent).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return kubeErr(err, "get "+what, way)
	}
	setAnnotation(u, DeletePullRequestAnnotation, url)
	return apply(ctx, c.Dynamic(), []write{{gvr: GVR, kind: orgKind, op: opUpdate, obj: u.Object, ns: parent, name: name, what: what + " (" + DeletePullRequestAnnotation + ")", reason: way}}, dryRun)
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
