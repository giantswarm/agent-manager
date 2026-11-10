package organizations

import (
	"context"
	"fmt"
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

	changes := []fileChange{
		{dir: cm.objectDir(req.Parent), write: map[string][]byte{objFile: manifest(obj, true)}},
		{dir: content, write: map[string][]byte{contentFile(content, kindRoleBinding, FirstAdminsBinding): manifest(binding, false)}},
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
	res.Organization = summary(planned)
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
	res := &UpdateResult{Before: summary(org), DryRun: req.DryRun, RequestedBy: identity.Caller(ctx)}
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
	res.After = summary(after)
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
	// whether that merge removed the file is asked of its files once the
	// file is gone from the base.
	var (
		pr     commit.PullRequest
		merged bool
	)
	if prURL != "" {
		var ok bool
		if pr, ok = cm.pullRequest(prURL); ok {
			st, err := cm.remote.Status(ctx, pr)
			if err != nil {
				return false, commitError(err)
			}
			merged = st.Merged
		}
	}

	if file != nil {
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
			return false, commitError(err)
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
	case !merged:
		return false, fmt.Errorf("%w: %s is gone from git (%s), but its delete pull request %s is not merged: it is Missing from git, not pending erase, and is not erased; restore its file", agents.ErrConflict, what, p, prURL)
	}
	if err := s.removedBy(ctx, cm, pr, what, p); err != nil {
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
// addition is not detected. The object's file alone decides: the content
// path is applied by the organization's own Kustomization, which the erase
// removes, and a create refuses a content path an earlier organization
// left. A listing that fails erases nothing.
func (s *Service) removedBy(ctx context.Context, cm *orgCommit, pr commit.PullRequest, what, p string) error {
	changes, err := cm.remote.MergedFiles(ctx, pr)
	if err != nil {
		return fmt.Errorf("%w (nothing was erased: the files of the delete pull request %s could not be listed, so whether it removed %s is unknown; call delete_organization again with mode commit and confirm to retry, and if the listing keeps failing, restore %s and open a new delete with mode commit and no confirm)", commitError(err), pr.URL, p, p)
	}
	const next = "then call delete_organization again with mode commit and no confirm to open a new delete pull request"
	for _, c := range changes {
		if c.Kind == commit.FileRenamed && c.PreviousPath == p {
			return fmt.Errorf("%w: %s: its merged delete pull request %s moved its file to %s instead of removing it, so the organization may still be in git; it is not pending erase and is not erased; move the file back to %s, %s", agents.ErrConflict, what, pr.URL, c.Path, p, next)
		}
	}
	if !commit.Removes(changes, p) {
		return fmt.Errorf("%w: %s is gone from git (%s), but its merged delete pull request %s did not remove it: it is Missing from git, not pending erase, and is not erased; restore its file, %s", agents.ErrConflict, what, p, pr.URL, next)
	}
	return nil
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
