package organizations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/gitops-commit/layout"
	"github.com/giantswarm/gitops-commit/provenance"
	"github.com/giantswarm/gitops-commit/sopsenc"
	orgv1alpha1 "github.com/giantswarm/organization-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// Commit mode writes an organization into the installation's repository
// instead of live (epic giantswarm/giantswarm#38891, slice
// giantswarm/agent-manager#292). The layout:
//
//   - the Organization object is one file in the objects path,
//     <objects>/<name>.yaml, which the installation's Kustomization applies
//     (deletionPolicy: Orphan); the
//     file carries kustomize.toolkit.fluxcd.io/prune: disabled, so a file
//     that leaves git never erases the organization;
//   - its contents (members, models, connectors, egress policies and their
//     credential Secrets) are one file per object in its content path,
//     <content prefix>/<name>, which the organization's own Flux
//     Kustomization applies into the organization namespace
//     (agent-platform-operator's --organization-path);
//   - a Secret is one file of its own named by gitops-commit's secret-file
//     convention (<name>-secret.enc.yaml), encrypted for the recipients of
//     the repository's .sops.yaml and refused when no creation rule covers
//     its path; no other path ever carries a Secret.
//
// Every commit is dry-run live as the caller first, so a change the caller
// could not apply is refused before a pull request exists.
const (
	// DeletePullRequestAnnotation is set on the live object, as the caller,
	// when commit mode opens the pull request that removes the object's
	// file: agent-platform-operator reports the organization
	// AgentPlatformOrphaned with reason DeleteMerged instead of MissingFromGit
	// once it merged.
	DeletePullRequestAnnotation = apov1alpha1.DeletePullRequestAnnotation
	// PruneDisabled is the value of PruneAnnotation on the object's file:
	// Flux never prunes it.
	PruneDisabled = "disabled"
	// DefaultContentPrefix is agent-platform-operator's default
	// --organization-path without its {name}.
	DefaultContentPrefix = "agent-platform/organizations"
	// commitBranchPrefix starts every head branch agent-manager opens.
	commitBranchPrefix = "agent-manager"

	kindSecret      = "Secret"
	kindRoleBinding = "RoleBinding"
)

// GitHubRemote is what commit mode needs of GitHub, acting as the person:
// the agent tools' remote, whose listing removes a content path.
type GitHubRemote = agents.GitHubRemote

// RemoteFor builds the GitHub remote from the person's App user token.
type RemoteFor = agents.RemoteFor

// CommitConfig is where commit mode writes an installation's organizations.
type CommitConfig struct {
	PlatformModelsPath string
	User               func(context.Context) (*agents.GitHubUserAccessToken, error)
	// GitHub builds the caller's remote.
	GitHub RemoteFor
	// Objects is the installation's repository, its branch, and the
	// directory that holds the organization objects' files.
	Objects provenance.Location
	// ContentPrefix is the repository-relative directory of the content
	// paths, <ContentPrefix>/<name>.
	ContentPrefix string
}

// NewCommitConfig checks commit mode's configuration: repository as
// owner/name, the branch, the objects path and the content prefix, both
// repository-relative directories, neither inside the other, so the
// installation's Kustomization never applies an organization's contents.
func NewCommitConfig(gh RemoteFor, repository, branch, objectsPath, contentPrefix string) (*CommitConfig, error) {
	if gh == nil {
		return nil, errors.New("commit mode for organizations needs GitHub App user authorization through a pinned registration or token exchange")
	}
	objects, err := provenance.Explicit(repository, branch, objectsPath)
	if err != nil {
		return nil, fmt.Errorf("organizations repository: %w", err)
	}
	content, err := provenance.Explicit(repository, branch, orDefault(contentPrefix, DefaultContentPrefix))
	if err != nil {
		return nil, fmt.Errorf("organizations content prefix: %w", err)
	}
	if objects.Directory == "" || content.Directory == "" {
		return nil, errors.New("the organizations objects path and content prefix are directories below the repository root")
	}
	if within(objects.Directory, content.Directory) || within(content.Directory, objects.Directory) {
		return nil, fmt.Errorf("the organizations objects path %s and content prefix %s must not contain each other: the installation's Kustomization would apply an organization's contents", objects.Directory, content.Directory)
	}
	return &CommitConfig{GitHub: gh, Objects: objects, ContentPrefix: content.Directory}, nil
}

func within(dir, p string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

// CommitResult is the pull request a write in commit mode opened, or, dry
// run, would open, in the installation's repository.
type CommitResult struct {
	// Repository is owner/name, Base the branch the change goes to.
	Repository string `json:"repository"`
	Base       string `json:"base"`
	// Branch is the pull request's head branch.
	Branch string              `json:"branch"`
	Files  []agents.CommitFile `json:"files"`
	// PullRequest is the URL of the pull request opened as the caller,
	// empty on a dry run and when nothing changes.
	PullRequest string `json:"pullRequest,omitempty"`
	Number      int    `json:"number,omitempty"`
	// Author is the GitHub login the pull request is opened as.
	Author string `json:"author,omitempty"`
	// Review says who reviews the pull request: the platform team, by the
	// repository's CODEOWNERS for the organization paths.
	Review string `json:"review,omitempty"`
	// LiveSteps are what the merge alone does not do.
	LiveSteps []string `json:"liveSteps,omitempty"`
	// Unchanged: git carries the change already, so no pull request was
	// opened.
	Unchanged bool `json:"unchanged,omitempty"`
}

// reviewNote is what every organization pull request says about its review.
const reviewNote = "the platform team reviews it: the repository's CODEOWNERS names them for the organization paths, and an organization's admin cannot approve it alone"

// mode checks a write's mode against what this installation offers.
func (s *Service) mode(m string) (string, error) {
	switch m {
	case "", agents.ModeApply:
		return agents.ModeApply, nil
	case agents.ModeCommit:
		if s.cfg.Commit == nil {
			return "", fmt.Errorf("%w: mode commit is not configured for organizations on this installation (no organizations repository): use mode apply", agents.ErrUnsupported)
		}
		return agents.ModeCommit, nil
	}
	return "", invalidf("mode %q: want %s or %s", m, agents.ModeApply, agents.ModeCommit)
}

// orgCommit is one write in commit mode: where it goes and as whom.
type orgCommit struct {
	cfg    *CommitConfig
	remote GitHubRemote
	gh     *agents.GitHubUserAccessToken
	log    *slog.Logger
}

func (s *Service) newCommit(ctx context.Context) (*orgCommit, error) {
	if s.cfg.Commit.User == nil {
		return nil, fmt.Errorf("%w: commit mode needs the GitHub grant exchange (github.tokenExchange), which this server is not configured with", agents.ErrUnsupported)
	}
	gh, err := s.cfg.Commit.User(ctx)
	if err != nil {
		return nil, err
	}
	if gh == nil || gh.Token == "" {
		return nil, errors.New("the GitHub grant exchange returned no grant")
	}
	remote, err := s.cfg.Commit.GitHub(gh.Token)
	if err != nil {
		return nil, commitError(gh, err)
	}
	return &orgCommit{cfg: s.cfg.Commit, remote: remote, gh: gh, log: s.log}, nil
}

// commitError answers a token GitHub refused with what renews it, and
// a secret file the repository cannot encrypt with what it lacks.
func commitError(gh *agents.GitHubUserAccessToken, err error) error {
	var auth *commit.AuthError
	if errors.As(err, &auth) {
		return agents.GitHubRefusal(gh, auth)
	}
	var secret *layout.SecretError
	if errors.As(err, &secret) {
		return invalidf("%s: the repository encrypts these paths and %s; a Secret is never committed in plaintext: add a creation rule for the organization paths to its .sops.yaml (docs/organizations-commit-mode.md)", strings.Join(secret.Paths, ", "), secret.Reason)
	}
	return fmt.Errorf("commit: %w", err)
}

// objectDir is the directory of the organization objects.
func (c *orgCommit) objectDir() layout.Directory { return c.cfg.objectDir() }

// objectFile is the file of the organization name.
func (c *orgCommit) objectFile(name string) string { return c.cfg.objectFile(name) }

// objectDir is the objects path as a layout directory: its own
// kustomization.yaml lists the organizations' files.
func (c *CommitConfig) objectDir() layout.Directory {
	loc := c.Objects
	loc.Directory = path.Dir(c.Objects.Directory)
	return layout.Directory{Location: loc, Name: path.Base(c.Objects.Directory)}
}

func (c *CommitConfig) objectFile(name string) string {
	return path.Join(c.Objects.Directory, name+".yaml")
}

// contentDir is the content path of the organization name.
func (c *orgCommit) contentDir(name string) layout.Directory {
	loc := c.cfg.Objects
	loc.Directory = c.cfg.ContentPrefix
	return layout.Directory{Location: loc, Name: name}
}

// contentFile is an object's file in a content path: a Secret in a secret
// file of its own (<name>-secret.enc.yaml), every other kind
// <kind>-<name>.yaml, so two kinds never share a file.
func contentFile(dir layout.Directory, kind, name string) string {
	if kind == kindSecret {
		return dir.ObjectFile(kindSecret, name)
	}
	return path.Join(dir.Path(), strings.ToLower(kind)+"-"+name+".yaml")
}

// segment refuses a name that is not one path segment; the API server's dry
// run refuses it too, this keeps it out of a path before.
func segment(what, name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return invalidf("%s %q is not a valid name", what, name)
	}
	return nil
}

// read reads and parses a file of the base branch; nil when it does not
// carry it.
func (c *orgCommit) read(ctx context.Context, p string) (*unstructured.Unstructured, error) {
	raw, err := c.remote.ReadFile(ctx, c.cfg.Objects.Repository, c.cfg.Objects.Branch, p)
	if errors.Is(err, commit.ErrFileNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, commitError(c.gh, err)
	}
	obj := map[string]any{}
	if err := yaml.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("%w: %s in %s does not parse as one object: %v", agents.ErrConflict, p, c.cfg.Objects.Repository, err)
	}
	return &unstructured.Unstructured{Object: obj}, nil
}

// exists reports whether the base branch carries p.
func (c *orgCommit) exists(ctx context.Context, p string) (bool, error) {
	_, err := c.remote.ReadFile(ctx, c.cfg.Objects.Repository, c.cfg.Objects.Branch, p)
	if errors.Is(err, commit.ErrFileNotFound) {
		return false, nil
	}
	if err != nil {
		return false, commitError(c.gh, err)
	}
	return true, nil
}

// list lists every file under dir on the base branch.
func (c *orgCommit) list(ctx context.Context, dir string) ([]string, error) {
	files, err := c.remote.ListFiles(ctx, c.cfg.Objects.Repository, c.cfg.Objects.Branch, dir)
	if err != nil {
		return nil, commitError(c.gh, err)
	}
	return files, nil
}

// fileChange is what one pull request changes in one directory.
type fileChange struct {
	dir     layout.Directory
	write   map[string][]byte
	remove  []string
	replace []string
	// drop are files removed beside layout.Build: the kustomization.yaml
	// files of the directory's subdirectories, which Build does not take.
	drop []string
	// create are files the change creates: each must still be absent on the
	// pull request's branch once it is cut from the base.
	create []string
	// add are files added beside layout.Build, which takes object files
	// only: the empty kustomization.yaml of a new content path.
	add map[string][]byte
	// keep keeps the directory's own kustomization.yaml, with an empty
	// list, when its last object goes: a Flux Kustomization builds the
	// directory, and would fail on a path that does not exist.
	keep bool
	// objects marks the objects path: it is kept, and the kustomization
	// above it, outside the objects path, is never changed.
	objects bool
}

// open decides every change against the base (layout.Build) and lands them
// as one pull request as the caller, or reports them on a dry run. Nothing
// to change opens none. object is the object's branch name (branch); the
// pull request's head is a branch of this write's own (head).
func (c *orgCommit) open(ctx context.Context, changes []fileChange, object, title, body string, dryRun bool) (*CommitResult, error) {
	branch := head(object)
	out := &CommitResult{Repository: c.cfg.Objects.Repository.String(), Base: c.cfg.Objects.Branch, Branch: branch, Author: c.gh.Login, Files: []agents.CommitFile{}, Review: reviewNote}
	var landed []commit.Change
	for _, ch := range changes {
		var opts []layout.Option
		if len(ch.replace) > 0 {
			opts = append(opts, layout.ReplaceSecrets(ch.replace...))
		}
		change := commit.Change{Location: ch.dir.Location, Files: map[string][]byte{}}
		// A change of added files alone takes no plan: layout.Build of a
		// directory without object files would remove its kustomization.yaml.
		if len(ch.write) > 0 || len(ch.remove) > 0 {
			plan, err := layout.Build(ctx, c.remote, ch.dir, ch.write, ch.remove, opts...)
			if err != nil {
				return nil, commitError(c.gh, err)
			}
			for _, f := range plan.Files {
				file := agents.CommitFile{Path: f.Path, Action: string(f.Action)}
				if dryRun {
					file.Content = string(f.Content)
				}
				out.Files = append(out.Files, file)
			}
			change = plan.Change()
		}
		if ch.objects {
			above := path.Join(ch.dir.Directory, layout.KustomizationFile)
			delete(change.Files, above)
			out.Files = slices.DeleteFunc(out.Files, func(f agents.CommitFile) bool { return f.Path == above })
		}
		if ch.keep || ch.objects {
			if err := c.keepKustomization(ctx, ch.dir, &change, out, dryRun); err != nil {
				return nil, err
			}
		}
		for p, content := range ch.add {
			old, err := c.remote.ReadFile(ctx, c.cfg.Objects.Repository, c.cfg.Objects.Branch, p)
			switch {
			case err == nil && bytes.Equal(old, content):
				// Git carries it already, as layout.Build leaves an
				// unchanged file.
				out.Files = append(out.Files, agents.CommitFile{Path: p, Action: string(layout.Unchanged)})
				continue
			case err != nil && !errors.Is(err, commit.ErrFileNotFound):
				return nil, commitError(c.gh, err)
			}
			change.Files[p] = content
			file := agents.CommitFile{Path: p, Action: string(layout.Add)}
			if dryRun {
				file.Content = string(content)
			}
			out.Files = append(out.Files, file)
		}
		for _, p := range ch.drop {
			change.Files[p] = nil
			i := slices.IndexFunc(out.Files, func(f agents.CommitFile) bool { return f.Path == p })
			if i < 0 {
				out.Files = append(out.Files, agents.CommitFile{Path: p})
				i = len(out.Files) - 1
			}
			out.Files[i] = agents.CommitFile{Path: p, Action: string(layout.Remove)}
		}
		if len(change.Files) > 0 {
			landed = append(landed, change)
		}
	}
	if dryRun {
		return out, nil
	}
	var creates []string
	for _, ch := range changes {
		creates = append(creates, ch.create...)
	}
	if len(landed) == 0 {
		// Git carries every file already. A file this write creates was
		// absent at its first check: another writer added it since, and the
		// create is refused as on the cut branch.
		if err := c.absent(ctx, creates); err != nil {
			return nil, err
		}
		out.Unchanged = true
		return out, nil
	}
	// The files are decided against the base: a second pull request of the
	// same object would undo the first once both merge.
	if open, has, err := c.openPullRequest(ctx, object); err != nil || has {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: the pull request %s for this object is open: merge or close it first, then call again", agents.ErrConflict, open.URL)
	}
	pr, err := c.land(ctx, landed, creates, branch, title, body+"\n\nThe platform team reviews this pull request (CODEOWNERS of the organization paths).\n\nOpened by agent-manager as @"+c.gh.Login+".")
	if err != nil {
		return nil, err
	}
	// The guard and land are not one step: another write of the object may
	// have opened its pull request since. The one with the lower number
	// stays; this write closes its own and answers as the guard would.
	if first, lost, err := agents.KeepFirst(ctx, c.log, c.remote, c.cfg.Objects.Repository, object, pr); lost {
		return nil, agents.RaceConflict("object", first, pr, err, "merge or close it first, then call again")
	}
	out.PullRequest, out.Number = pr.URL, pr.Number
	return out, nil
}

// keepKustomization keeps the directory's kustomization.yaml in change, the
// plan layout.Build made for it, with an empty list when the plan removes it
// with the directory's last object: a Flux Kustomization builds the
// directory (the installation's the objects path, the organization's its
// content path), and the merge would otherwise leave it a path that does
// not exist.
func (c *orgCommit) keepKustomization(ctx context.Context, dir layout.Directory, change *commit.Change, out *CommitResult, dryRun bool) error {
	own := path.Join(dir.Path(), layout.KustomizationFile)
	if content, present := change.Files[own]; !present || content != nil {
		return nil
	}
	old, err := c.remote.ReadFile(ctx, c.cfg.Objects.Repository, c.cfg.Objects.Branch, own)
	if err != nil {
		return commitError(c.gh, err)
	}
	empty, err := layout.WithResources(old, []string{})
	if err != nil {
		return commitError(c.gh, err)
	}
	change.Files[own] = empty
	for i := range out.Files {
		if out.Files[i].Path == own {
			out.Files[i].Action = string(layout.Update)
			if dryRun {
				out.Files[i].Content = string(empty)
			}
		}
	}
	return nil
}

// land is commit.Open for the one repository and base of c, with the files
// creates must still be absent from the base once the branch is cut: a file
// another commit added to the base after the plan was decided, and so before
// the cut, is seen and refused (nameTaken) before anything is committed.
// branch is this write's own (head), so CreateBranch cuts it new from the
// base and no file of an earlier pull request of the object rides along. A
// refused branch stays as cut, without a commit: the remote deletes no
// branch, and no later write uses it.
func (c *orgCommit) land(ctx context.Context, changes []commit.Change, creates []string, branch, title, body string) (commit.PullRequest, error) {
	repo, base := c.cfg.Objects.Repository, c.cfg.Objects.Branch
	files := map[string][]byte{}
	for _, ch := range changes {
		if ch.Location.Repository != repo || ch.Location.Branch != base {
			return commit.PullRequest{}, commitError(c.gh, fmt.Errorf("%w: %s", commit.ErrConflictingBase, ch.Location.Repository))
		}
		for p, content := range ch.Files {
			if _, dup := files[p]; dup {
				return commit.PullRequest{}, commitError(c.gh, fmt.Errorf("%w: %s %s", commit.ErrDuplicatePath, repo, p))
			}
			files[p] = content
		}
	}
	if err := c.remote.CreateBranch(ctx, repo, branch, base); err != nil {
		return commit.PullRequest{}, commitError(c.gh, err)
	}
	if err := c.absent(ctx, creates); err != nil {
		return commit.PullRequest{}, err
	}
	if err := c.remote.Commit(ctx, repo, branch, title, files); err != nil {
		return commit.PullRequest{}, commitError(c.gh, err)
	}
	pr, err := c.remote.OpenPullRequest(ctx, repo, branch, base, title, body)
	if err != nil {
		return commit.PullRequest{}, commitError(c.gh, err)
	}
	return pr, nil
}

// absent refuses (nameTaken) when the base branch carries any of creates,
// files a write creates.
func (c *orgCommit) absent(ctx context.Context, creates []string) error {
	for _, p := range creates {
		taken, err := c.exists(ctx, p)
		if err != nil {
			return err
		}
		if taken {
			return nameTaken{fmt.Errorf("%w: %s is in %s already and not applied yet: wait for Flux to apply it, then change it", agents.ErrConflict, p, c.cfg.Objects.Repository)}
		}
	}
	return nil
}

// branch is the branch name of a verb on an organization: the name of the
// object a write changes, which every write of it shares. Each write's pull
// request has a head branch of its own (head); an older agent-manager used
// this name itself as the head.
func branch(verb, name string) string {
	return fmt.Sprintf("%s/%s-%s", commitBranchPrefix, verb, name)
}

// head is a write's own head branch (agents.Head): the object's branch
// name, agents.HeadSeparator and a random suffix.
func head(object string) string { return agents.Head(object) }

// openPullRequest finds the open pull request of the object's branch name
// with the lowest number (agents.PullRequestsOf): on a head of a write, or
// on the name itself, the head an older agent-manager opened.
func (c *orgCommit) openPullRequest(ctx context.Context, object string) (commit.PullRequest, bool, error) {
	prs, err := agents.PullRequestsOf(ctx, c.remote, c.cfg.Objects.Repository, object)
	if err != nil {
		return commit.PullRequest{}, false, commitError(c.gh, err)
	}
	if len(prs) == 0 {
		return commit.PullRequest{}, false, nil
	}
	return prs[0], true, nil
}

// manifest renders obj as the file Flux applies: no status, no
// server-written metadata, none of Flux's own labels, and no namespace when
// the file lands in a content path, whose Kustomization sets it.
func manifest(obj *unstructured.Unstructured, keepNamespace bool) []byte {
	out := map[string]any{}
	for k, v := range obj.Object {
		if k != "status" && k != fieldMetadata {
			out[k] = v
		}
	}
	meta := map[string]any{"name": obj.GetName()}
	if keepNamespace && obj.GetNamespace() != "" {
		meta["namespace"] = obj.GetNamespace()
	}
	labels := map[string]any{}
	for k, v := range obj.GetLabels() {
		if !strings.HasPrefix(k, fluxLabelPrefix) {
			labels[k] = v
		}
	}
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	annotations := map[string]any{}
	for k, v := range obj.GetAnnotations() {
		if k != lastAppliedAnnotation && k != DeletePullRequestAnnotation {
			annotations[k] = v
		}
	}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	out[fieldMetadata] = meta
	raw, err := yaml.Marshal(out)
	if err != nil {
		// A map of what the API server or a YAML file gave always marshals.
		panic(fmt.Sprintf("marshal %s %s: %v", obj.GetKind(), obj.GetName(), err))
	}
	return raw
}

const (
	fluxLabelPrefix       = "kustomize.toolkit.fluxcd.io/"
	lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"
)

// setAnnotation sets one annotation on obj.
func setAnnotation(obj *unstructured.Unstructured, key, value string) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[key] = value
	obj.SetAnnotations(annotations)
}

// fromGit reports whether a Flux Kustomization applies obj.
func fromGit(obj *unstructured.Unstructured) bool {
	return obj.GetLabels()[agents.KustomizationNameLabel] != ""
}

// ---- contents in commit mode ----------------------------------------------------

// contentWriter lands the writes of a tool in an organization namespace:
// live in apply mode; in commit mode as files of the organization's content
// path, after the same dry run as the caller.
type contentWriter struct {
	mode   string
	org    Namespace
	commit *orgCommit
	dir    layout.Directory
}

// writable refuses a write into the organization of org while it is being
// deleted, or is in Pending erase: its delete pull request merged
// (AgentPlatformOrphaned, reason DeleteMerged) and Erase now removes what is
// in it. Every write tool of an organization's contents builds a
// contentWriter, so this is their one check; reads and delete_organization build none. It also
// refuses a write for an organization UID (org.OrganizationUID, a credential
// operation's recorded one) that is not the Organization's now: one deleted
// and created again under the same name has the same namespace name, and a
// write prepared for the first must not reach the second. A namespace that
// does not name its organization, or whose organization is gone, is not
// refused here.
func (s *Service) writable(ctx context.Context, org Namespace) error {
	if org.Organization == "" {
		return nil
	}
	o, ok, err := s.cfg.Namespaces.Organization(ctx, org.Organization)
	if err != nil {
		return fmt.Errorf("%w: %v", agents.ErrUnsupported, err)
	}
	if !ok {
		if org.prepared && org.OrganizationUID != "" {
			return fmt.Errorf("%w: organization %s was deleted since this write was prepared: nothing is written into namespace %s", agents.ErrConflict, org.Organization, org.Name)
		}
		return nil
	}
	if org.OrganizationUID != "" && string(o.UID) != org.OrganizationUID {
		return fmt.Errorf("%w: organization %s was deleted and created again since this write was prepared: nothing is written into namespace %s; prepare it again", agents.ErrConflict, o.Name, org.Name)
	}
	return s.erasing(o, org.Name)
}

// erasing refuses a write to the organization o, or into its namespace ns,
// while o is being deleted or is in Pending erase. writable reads o with
// agent-manager's ServiceAccount; update_organization passes the object the
// caller read.
func (s *Service) erasing(o *orgv1alpha1.Organization, ns string) error {
	what := "organization " + o.Name
	if o.DeletionTimestamp != nil {
		return fmt.Errorf("%w: %s is being erased (%s): nothing is written into namespace %s; get_organization reports its erasure step until it is gone", agents.ErrConflict, what, orDefault(erasureStepOf(o), "starting"), ns)
	}
	c := meta.FindStatusCondition(o.Status.Conditions, orgv1alpha1.AgentPlatformOrphaned)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != orgv1alpha1.AgentPlatformOrphanedDeleteMerged {
		return nil
	}
	file := "its file"
	if s.cfg.Commit != nil {
		file += " " + s.cfg.Commit.objectFile(o.Name)
	}
	return fmt.Errorf("%w: %s is in Pending erase: its delete pull request merged (%s, reason %s), so nothing is written into namespace %s. Erase it with delete_organization, mode commit and confirm %q (Erase now), or restore %s in git to keep it", agents.ErrConflict, what, orgv1alpha1.AgentPlatformOrphaned, orgv1alpha1.AgentPlatformOrphanedDeleteMerged, ns, o.Name, file)
}

func (s *Service) contentWriter(ctx context.Context, org Namespace, mode string) (*contentWriter, error) {
	mode, err := s.mode(mode)
	if err != nil {
		return nil, err
	}
	if err := s.writable(ctx, org); err != nil {
		return nil, err
	}
	w := &contentWriter{mode: mode, org: org}
	if mode != agents.ModeCommit {
		return w, nil
	}
	if org.Scope != scopePlatform && org.Organization == "" {
		return nil, fmt.Errorf("%w: namespace %s does not name its organization (label %s): its content path is unknown", agents.ErrConflict, org.Name, apov1alpha1.OrganizationLabel)
	}
	if w.commit, err = s.newCommit(ctx); err != nil {
		return nil, err
	}
	w.dir, err = w.commit.credentialDirectory(org)
	if err != nil {
		return nil, err
	}
	return w, nil
}

// owned protects operator copies and enforces where an object's content lives.
// Apply mode cannot change objects applied by Flux; commit mode requires a Git source.
func (w *contentWriter) owned(obj *unstructured.Unstructured, kind string) error {
	if obj == nil {
		return nil
	}
	if scope, ok := obj.GetLabels()[agents.OperatorObjectLabel]; ok {
		return fmt.Errorf("%w: %s %s/%s is agent-platform-operator's (%s: %s), a copy of what the platform curates: it is changed where the platform curates it, not here", agents.ErrConflict, kind, obj.GetNamespace(), obj.GetName(), agents.OperatorObjectLabel, scope)
	}
	switch {
	case w.mode == agents.ModeApply && fromGit(obj):
		return fmt.Errorf("%w: %s %s/%s is applied from git by the Flux Kustomization %s, which would revert a live change: use mode commit to change it with a pull request", agents.ErrGitOpsOwned, kind, obj.GetNamespace(), obj.GetName(), obj.GetLabels()[agents.KustomizationNameLabel])
	case w.mode == agents.ModeCommit && !fromGit(obj):
		return fmt.Errorf("%w: %s %s/%s was written live, not applied from git, so no file in the organization's content path defines it: change it with mode apply", agents.ErrConflict, kind, obj.GetNamespace(), obj.GetName())
	}
	return nil
}

// source is the object a write of an existing obj changes: obj itself in
// apply mode; in commit mode its file in the content path, carrying the live
// namespace, resourceVersion and Flux labels for the dry run, so the commit
// keeps what the file says and never copies what the API server defaulted.
func (w *contentWriter) source(ctx context.Context, obj *unstructured.Unstructured, kind string) (*unstructured.Unstructured, error) {
	if obj == nil {
		return nil, nil
	}
	if err := w.owned(obj, kind); err != nil {
		return nil, err
	}
	if kind == kubeKindModelConfig {
		owner := obj.GetLabels()[agents.ManagedByLabel]
		for _, key := range []string{"meta.helm.sh/release-name", "meta.helm.sh/release-namespace"} {
			if obj.GetAnnotations()[key] != "" {
				owner = "Helm"
			}
		}
		if obj.GetLabels()["helm.toolkit.fluxcd.io/name"] != "" || obj.GetLabels()["helm.toolkit.fluxcd.io/namespace"] != "" {
			owner = "Helm"
		}
		if owner != "" && owner != agents.ManagedByValue && owner != agents.PortalManagedByValue {
			return nil, fmt.Errorf("%w: %s %s/%s is managed by %s: change it through that owner", agents.ErrGitOpsOwned, kind, obj.GetNamespace(), obj.GetName(), owner)
		}
	}
	if w.mode != agents.ModeCommit {
		return obj, nil
	}
	p := contentFile(w.dir, kind, obj.GetName())
	file, err := w.commit.read(ctx, p)
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, fmt.Errorf("%w: %s %s/%s is applied from git, but not from %s in %s: change it in the file that defines it", agents.ErrConflict, kind, obj.GetNamespace(), obj.GetName(), p, w.commit.cfg.Objects.Repository)
	}
	withLive(file, obj)
	return file, nil
}

// withLive gives a file's object the live object's namespace,
// resourceVersion and Flux labels, so a dry run updates the live object to
// what the file says; manifest drops them again.
func withLive(file, live *unstructured.Unstructured) {
	file.SetNamespace(live.GetNamespace())
	file.SetResourceVersion(live.GetResourceVersion())
	// What no file carries stays as it is live: an apply from git does
	// not touch it either.
	file.SetFinalizers(live.GetFinalizers())
	for _, k := range []string{DeletePullRequestAnnotation, lastAppliedAnnotation} {
		if v, ok := live.GetAnnotations()[k]; ok {
			setAnnotation(file, k, v)
		}
	}
	labels := file.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range live.GetLabels() {
		if strings.HasPrefix(k, fluxLabelPrefix) {
			labels[k] = v
		}
	}
	file.SetLabels(labels)
}

// secretFiles is how the repository's .sops.yaml decides which files
// gitops-commit encrypts: by name, and by any creation rule's path_regex.
func (c *orgCommit) secretFiles(ctx context.Context) (func(string) bool, error) {
	raw, err := c.remote.ReadFile(ctx, c.cfg.Objects.Repository, c.cfg.Objects.Branch, layout.SOPSConfigFile)
	if errors.Is(err, commit.ErrFileNotFound) {
		return sopsenc.IsSecretFile, nil
	}
	if err != nil {
		return nil, commitError(c.gh, err)
	}
	enc, err := sopsenc.New(raw)
	if err != nil {
		return sopsenc.IsSecretFile, nil
	}
	return enc.IsSecretFile, nil
}

// refusePlainAsSecret refuses a file of an object that is no Secret where
// gitops-commit would encrypt it: it would never be updated again.
func refusePlainAsSecret(isSecret func(string) bool, p, kind, name string) error {
	if isSecret(p) {
		return invalidf("%s %s: its file %s is a secret file in git (by its name, or a .sops.yaml path_regex), which gitops-commit encrypts once and never updates: pick another name, or narrow the creation rule to the -secret.enc.yaml files", kind, name, p)
	}
	return nil
}

// land dry-runs every write as the caller, then applies them in apply mode,
// or commits them as files of the content path in commit mode. Nothing lands
// when any dry run is refused.
func (w *contentWriter) land(ctx context.Context, s *Service, writes []write, dryRun bool, verb, title string) (*CommitResult, error) {
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	if w.mode != agents.ModeCommit {
		return nil, apply(ctx, c.Dynamic(), writes, dryRun)
	}
	if err := apply(ctx, c.Dynamic(), writes, true); err != nil {
		return nil, err
	}
	isSecret, err := w.commit.secretFiles(ctx)
	if err != nil {
		return nil, err
	}
	ch := fileChange{dir: w.dir, write: map[string][]byte{}, keep: w.org.Scope != scopePlatform}
	for _, wr := range writes {
		if err := segment(wr.kind+" name", wr.name); err != nil {
			return nil, err
		}
		p := contentFile(w.dir, wr.kind, wr.name)
		if wr.op != opDelete {
			if err := refusePlainAsSecret(isSecret, p, wr.kind, wr.name); err != nil {
				return nil, err
			}
		}
		switch wr.op {
		case opDelete:
			ch.remove = append(ch.remove, p)
		case opCreate:
			ok, err := w.commit.exists(ctx, p)
			if err != nil {
				return nil, err
			}
			if ok {
				return nil, nameTaken{fmt.Errorf("%w: %s is in %s already and not applied yet: wait for the organization's Kustomization to apply it, then change it", agents.ErrConflict, p, w.commit.cfg.Objects.Repository)}
			}
			ch.write[p] = manifest(unstructuredOf(wr.obj), false)
			ch.create = append(ch.create, p)
		default:
			ch.write[p] = manifest(unstructuredOf(wr.obj), false)
		}
	}
	body := fmt.Sprintf("%s in organization %s (namespace %s), in its content path %s.", title, w.org.Organization, w.org.Name, w.dir.Path())
	return w.commit.open(ctx, []fileChange{ch}, branch(verb, w.org.Organization), title, body, dryRun)
}

// commitURL is the GitHub page of the commit a Flux GitRepository revision
// names (<branch>@sha1:<hash>, or sha1:<hash> without a branch), or empty for
// any other revision.
func commitURL(repository provenance.Repository, revision string) string {
	hash, ok := revisionCommit(revision)
	if !ok {
		return ""
	}
	return "https://github.com/" + repository.String() + "/commit/" + hash
}

// fileURL is the GitHub page of file, a repository-relative path, as it was
// at the commit revision names, or empty when revision names no commit.
func fileURL(repository provenance.Repository, revision, file string) string {
	hash, ok := revisionCommit(revision)
	if !ok {
		return ""
	}
	return "https://github.com/" + repository.String() + "/blob/" + hash + "/" + file
}

// revisionCommit is the commit hash of a Flux GitRepository revision.
func revisionCommit(revision string) (string, bool) {
	digest := revision[strings.LastIndex(revision, "@")+1:]
	hash, ok := strings.CutPrefix(digest, "sha1:")
	if !ok || len(hash) != 40 || strings.Trim(hash, "0123456789abcdef") != "" {
		return "", false
	}
	return hash, true
}
