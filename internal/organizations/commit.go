package organizations

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/gitops-commit/layout"
	"github.com/giantswarm/gitops-commit/provenance"
	"github.com/giantswarm/gitops-commit/sopsenc"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
)

// Commit mode writes an organization into the installation's repository
// instead of live (epic giantswarm/giantswarm#38891, slice
// giantswarm/agent-manager#292). The layout:
//
//   - the AgentPlatformOrganization object is one file under its parent in
//     the objects path, <objects>/<parent>/<name>.yaml, which the
//     installation's Kustomization applies (deletionPolicy: Orphan); the
//     file carries kustomize.toolkit.fluxcd.io/prune: disabled, so a file
//     that leaves git never erases the organization;
//   - its contents (members, models, connectors, egress policies and their
//     credential Secrets) are one file per object in its content path,
//     <content prefix>/<parent>/<name>, which the organization's own Flux
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
	// file: agent-platform-operator reports the organization Orphaned with
	// reason DeleteMerged instead of MissingFromGit once it merged.
	DeletePullRequestAnnotation = "agent-platform.giantswarm.io/delete-pull-request"
	// PruneDisabled is the value of PruneAnnotation on the object's file
	// and on the organization-admins file: Flux never prunes either.
	PruneDisabled = "disabled"
	// DefaultContentPrefix is agent-platform-operator's default
	// --organization-path without its {parent}/{name}.
	DefaultContentPrefix = "agent-platform/organizations"
	// commitBranchPrefix starts every head branch agent-manager opens.
	commitBranchPrefix = "agent-manager"

	kindSecret      = "Secret"
	kindRoleBinding = "RoleBinding"
)

// GitHubRemote is what commit mode needs of GitHub, acting as the person:
// the pull request seams, the reads of the base and the listing of a
// content path.
type GitHubRemote interface {
	commit.Remote
	commit.Reader
	commit.Lister
}

// RemoteFor builds the GitHub remote from the person's App user token.
type RemoteFor func(token string) (GitHubRemote, error)

// CommitConfig is where commit mode writes an installation's organizations.
type CommitConfig struct {
	// GitHub builds the caller's remote.
	GitHub RemoteFor
	// Objects is the installation's repository, its branch, and the
	// directory that holds one directory per parent namespace with the
	// organization objects' files.
	Objects provenance.Location
	// ContentPrefix is the repository-relative directory of the content
	// paths, <ContentPrefix>/<parent>/<name>.
	ContentPrefix string
}

// NewCommitConfig checks commit mode's configuration: repository as
// owner/name, the branch, the objects path and the content prefix, both
// repository-relative directories, neither inside the other, so the
// installation's Kustomization never applies an organization's contents.
func NewCommitConfig(gh RemoteFor, repository, branch, objectsPath, contentPrefix string) (*CommitConfig, error) {
	if gh == nil {
		return nil, errors.New("commit mode for organizations needs the GitHub App (--github-authorization-server)")
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
	gh     *identity.GitHub
}

func (s *Service) newCommit(ctx context.Context) (*orgCommit, error) {
	gh, ok := identity.GitHubFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("%w: commit mode opens the pull request with your GitHub authorization, and this call carries none: connect agent-manager in muster (core_auth_login server=agent-manager), then call again", agents.ErrAuthRequired)
	}
	remote, err := s.cfg.Commit.GitHub(gh.Token)
	if err != nil {
		return nil, commitError(err)
	}
	return &orgCommit{cfg: s.cfg.Commit, remote: remote, gh: gh}, nil
}

// commitError answers a token GitHub refused with the consent to renew, and
// a secret file the repository cannot encrypt with what it lacks.
func commitError(err error) error {
	var auth *commit.AuthError
	if errors.As(err, &auth) {
		return fmt.Errorf("%w: GitHub refused your token on %s (status %d): reconnect agent-manager in muster (core_auth_login server=agent-manager); the App must be installed on the repository and your account allowed to push to it", agents.ErrAuthRequired, auth.Op, auth.Status)
	}
	var secret *layout.SecretError
	if errors.As(err, &secret) {
		return invalidf("%s: the repository encrypts these paths and %s; a Secret is never committed in plaintext: add a creation rule for the organization paths to its .sops.yaml (docs/organizations-commit-mode.md)", strings.Join(secret.Paths, ", "), secret.Reason)
	}
	return fmt.Errorf("commit: %w", err)
}

// objectDir is the directory of a parent's organization objects.
func (c *orgCommit) objectDir(parent string) layout.Directory {
	return layout.Directory{Location: c.cfg.Objects, Name: parent}
}

// objectFile is the file of the organization parent/name.
func (c *orgCommit) objectFile(parent, name string) string {
	return path.Join(c.objectDir(parent).Path(), name+".yaml")
}

// contentDir is the content path of the organization parent/name.
func (c *orgCommit) contentDir(parent, name string) layout.Directory {
	loc := c.cfg.Objects
	loc.Directory = path.Join(c.cfg.ContentPrefix, parent)
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
		return nil, commitError(err)
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
		return false, commitError(err)
	}
	return true, nil
}

// list lists every file under dir on the base branch.
func (c *orgCommit) list(ctx context.Context, dir string) ([]string, error) {
	files, err := c.remote.ListFiles(ctx, c.cfg.Objects.Repository, c.cfg.Objects.Branch, dir)
	if err != nil {
		return nil, commitError(err)
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
}

// open decides every change against the base (layout.Build) and lands them
// as one pull request as the caller, or reports them on a dry run. Nothing
// to change opens none.
func (c *orgCommit) open(ctx context.Context, changes []fileChange, branch, title, body string, dryRun bool) (*CommitResult, error) {
	out := &CommitResult{Repository: c.cfg.Objects.Repository.String(), Base: c.cfg.Objects.Branch, Branch: branch, Author: c.gh.Login, Files: []agents.CommitFile{}, Review: reviewNote}
	var landed []commit.Change
	for _, ch := range changes {
		var opts []layout.Option
		if len(ch.replace) > 0 {
			opts = append(opts, layout.ReplaceSecrets(ch.replace...))
		}
		plan, err := layout.Build(ctx, c.remote, ch.dir, ch.write, ch.remove, opts...)
		if err != nil {
			return nil, commitError(err)
		}
		for _, f := range plan.Files {
			file := agents.CommitFile{Path: f.Path, Action: string(f.Action)}
			if dryRun {
				file.Content = string(f.Content)
			}
			out.Files = append(out.Files, file)
		}
		change := plan.Change()
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
	if dryRun || len(landed) == 0 {
		return out, nil
	}
	prs, err := commit.Open(ctx, c.remote, commit.Request{Branch: branch, Title: title, Body: body + "\n\nThe platform team reviews this pull request (CODEOWNERS of the organization paths).\n\nOpened by agent-manager as @" + c.gh.Login + "."}, landed)
	if err != nil {
		return nil, commitError(err)
	}
	out.PullRequest, out.Number = prs[0].URL, prs[0].Number
	return out, nil
}

// branch is a pull request's head branch for a verb on an organization.
func branch(verb, parent, name string) string {
	return fmt.Sprintf("%s/%s-%s-%s", commitBranchPrefix, verb, parent, name)
}

// manifest renders obj as the file Flux applies: no status, no
// server-written metadata, none of Flux's own labels, and no namespace when
// the file lands in a content path, whose Kustomization sets it.
func manifest(obj *unstructured.Unstructured, keepNamespace bool) []byte {
	out := map[string]any{}
	for k, v := range obj.Object {
		if k != "status" && k != "metadata" {
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
	out["metadata"] = meta
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

func (s *Service) contentWriter(ctx context.Context, org Namespace, mode string) (*contentWriter, error) {
	mode, err := s.mode(mode)
	if err != nil {
		return nil, err
	}
	w := &contentWriter{mode: mode, org: org}
	if mode != agents.ModeCommit {
		return w, nil
	}
	if org.Parent == "" || org.Organization == "" {
		return nil, fmt.Errorf("%w: namespace %s does not name its organization (labels %s and %s): its content path is unknown", agents.ErrConflict, org.Name, "agent-platform.giantswarm.io/parent", "agent-platform.giantswarm.io/organization")
	}
	if w.commit, err = s.newCommit(ctx); err != nil {
		return nil, err
	}
	w.dir = w.commit.contentDir(org.Parent, org.Organization)
	return w, nil
}

// owned refuses a write to an object this mode may not change:
// agent-platform-operator's copies always; in apply mode an object a Flux
// Kustomization applies from git, which Flux would revert; in commit mode an
// object written live, which no file in git defines.
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
	file.SetNamespace(obj.GetNamespace())
	file.SetResourceVersion(obj.GetResourceVersion())
	labels := file.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range obj.GetLabels() {
		if strings.HasPrefix(k, fluxLabelPrefix) {
			labels[k] = v
		}
	}
	file.SetLabels(labels)
	return file, nil
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
	for i := range writes {
		if writes[i].kind == kindRoleBinding && writes[i].name == FirstAdminsBinding && writes[i].obj != nil {
			// agent-platform's admission admits the organization's Flux
			// identity's apply of this binding only with prune disabled.
			obj := unstructuredOf(writes[i].obj)
			setAnnotation(obj, PruneAnnotation, PruneDisabled)
		}
	}
	if err := apply(ctx, c.Dynamic(), writes, true); err != nil {
		return nil, err
	}
	ch := fileChange{dir: w.dir, write: map[string][]byte{}}
	for _, wr := range writes {
		if err := segment(wr.kind+" name", wr.name); err != nil {
			return nil, err
		}
		p := contentFile(w.dir, wr.kind, wr.name)
		if wr.kind != kindSecret && sopsenc.IsSecretFile(p) {
			// gitops-commit would encrypt it and never rewrite it again.
			return nil, invalidf("%s %s: its file %s would be a secret file in git (a name containing \"secret\" or \"credential\"), which is encrypted and never updated: pick another name", wr.kind, wr.name, p)
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
				return nil, fmt.Errorf("%w: %s is in %s already and not applied yet: wait for the organization's Kustomization to apply it, then change it", agents.ErrConflict, p, w.commit.cfg.Objects.Repository)
			}
			ch.write[p] = manifest(unstructuredOf(wr.obj), false)
		default:
			ch.write[p] = manifest(unstructuredOf(wr.obj), false)
		}
	}
	body := fmt.Sprintf("%s in organization %s/%s (namespace %s), in its content path %s.", title, w.org.Parent, w.org.Organization, w.org.Name, w.dir.Path())
	return w.commit.open(ctx, []fileChange{ch}, branch(verb, w.org.Parent, w.org.Organization), title, body, dryRun)
}
