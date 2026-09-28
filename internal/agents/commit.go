package agents

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/giantswarm/gitops-commit/layout"
	"github.com/giantswarm/gitops-commit/provenance"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/agent-manager/internal/identity"
)

// The Flux objects commit mode follows from a namespace to the repository
// that owns it.
var (
	kustomizationGVR = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	gitRepositoryGVR = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}
	namespaceGVR     = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
)

// KustomizationNamespaceLabel is the namespace of the Kustomization named by
// KustomizationNameLabel.
const KustomizationNamespaceLabel = "kustomize.toolkit.fluxcd.io/namespace"

// CommitDirectory is the directory agent-manager writes an agent's files to,
// under the directory the owning Kustomization builds from (gitops-commit's
// layout, the one cluster-manager and model-manager write theirs to).
const CommitDirectory = "agent-manager"

// GitHubRemote is what commit mode needs of GitHub: the pull request seams
// and the reads of the base, acting as the person.
type GitHubRemote interface {
	commit.Remote
	commit.Reader
}

// RemoteFor builds the GitHub remote from the person's App user token.
type RemoteFor func(token string) (GitHubRemote, error)

// CommitAvailable reports whether this server offers commit mode.
func (s *Service) CommitAvailable() bool { return s.cfg.GitHub != nil }

// CommitResult is the pull request a write in commit mode opened — or, dry
// run, would open — in the repository that owns the agent. The shape is
// cluster-manager's and model-manager's.
type CommitResult struct {
	// Repository is owner/name, Base the branch the change goes to.
	Repository string `json:"repository"`
	Base       string `json:"base"`
	// Directory is where the files go: CommitDirectory under the
	// Kustomization's path (or the path the caller named).
	Directory string `json:"directory"`
	// Kustomization is the Flux Kustomization (namespace/name) that lands
	// the files after the merge, and Prune its spec.prune. Empty for a target
	// the caller named.
	Kustomization string `json:"kustomization,omitempty"`
	Prune         bool   `json:"prune"`
	// Branch is the pull request's head branch.
	Branch string       `json:"branch"`
	Files  []CommitFile `json:"files"`
	// PullRequest is the URL of the pull request opened as the caller, empty
	// on a dry run and when nothing changes.
	PullRequest string `json:"pullRequest,omitempty"`
	Number      int    `json:"number,omitempty"`
	// Author is the GitHub login the pull request is opened as.
	Author string `json:"author,omitempty"`
	// LiveSteps are what the merge alone does not do on the installation.
	LiveSteps []string `json:"liveSteps,omitempty"`
}

// CommitFile is one file of the commit; Content is shown on a dry run.
type CommitFile struct {
	Path    string `json:"path"`
	Action  string `json:"action"`
	Content string `json:"content,omitempty"`
}

// commitLocation is where an agent's files go in git and how Flux lands them.
type commitLocation struct {
	provenance.Location
	kustomization string
	prune         bool
}

func (l commitLocation) directory() layout.Directory {
	return layout.Directory{Location: l.Location, Name: CommitDirectory}
}

// describe names the target in a refusal.
func (l commitLocation) describe() string {
	return fmt.Sprintf("%s, directory %s on %s", l.Repository, l.directory().Path(), l.Branch)
}

// checkMode validates the write options against what this server offers.
func (s *Service) checkMode(w WriteOptions) (string, error) {
	switch w.Mode {
	case "", ModeApply:
		if w.Repository != "" || w.Branch != "" || w.Path != "" {
			return "", invalidf("repository, branch and path name a commit target: pass them with mode commit")
		}
		return ModeApply, nil
	case ModeCommit:
		if !s.CommitAvailable() {
			return "", fmt.Errorf("%w: mode commit is not configured on this installation (get_info capabilities.commit is false): use mode apply", ErrUnsupported)
		}
		if w.Repository == "" && (w.Branch != "" || w.Path != "") {
			return "", invalidf("branch and path need repository (owner/name)")
		}
		return ModeCommit, nil
	default:
		return "", invalidf("mode %q: want %s or %s", w.Mode, ModeApply, ModeCommit)
	}
}

// errNotFromGit is a target no Flux Kustomization owns.
var errNotFromGit = errors.New("not reconciled from git")

// locationFromLabels follows the Flux Kustomization labels of obj to the
// repository and directory it is built from. Read as the caller.
func locationFromLabels(ctx context.Context, dyn dynamic.Interface, obj *unstructured.Unstructured) (commitLocation, error) {
	ksName := obj.GetLabels()[KustomizationNameLabel]
	if ksName == "" {
		return commitLocation{}, errNotFromGit
	}
	ksNamespace := orDefault(obj.GetLabels()[KustomizationNamespaceLabel], obj.GetNamespace())
	ks, err := dyn.Resource(kustomizationGVR).Namespace(ksNamespace).Get(ctx, ksName, metav1.GetOptions{})
	if err != nil {
		return commitLocation{}, wrapKube(err, fmt.Sprintf("get Kustomization %s/%s", ksNamespace, ksName))
	}
	nested := func(o *unstructured.Unstructured, path ...string) string {
		v, _, _ := unstructured.NestedString(o.Object, path...)
		return v
	}
	src := provenance.SourceRef{
		Kind:      nested(ks, "spec", "sourceRef", "kind"),
		Name:      nested(ks, "spec", "sourceRef", "name"),
		Namespace: nested(ks, "spec", "sourceRef", "namespace"),
	}
	flux := provenance.Flux{Kustomizations: []provenance.Kustomization{{Name: ksName, Namespace: ksNamespace, SourceRef: src, Path: nested(ks, "spec", "path")}}}
	if src.Kind == provenance.KindGitRepository {
		srcNamespace := orDefault(src.Namespace, ksNamespace)
		repo, err := dyn.Resource(gitRepositoryGVR).Namespace(srcNamespace).Get(ctx, src.Name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return commitLocation{}, wrapKube(err, fmt.Sprintf("get GitRepository %s/%s", srcNamespace, src.Name))
		}
		if err == nil {
			flux.GitRepositories = []provenance.GitRepository{{Name: src.Name, Namespace: srcNamespace, URL: nested(repo, "spec", "url"), Branch: nested(repo, "spec", "ref", "branch")}}
		}
	}
	loc, err := flux.Resolve(ksNamespace, ksName)
	if err != nil {
		return commitLocation{}, invalidf("Kustomization %s/%s: %v", ksNamespace, ksName, err)
	}
	prune, _, _ := unstructured.NestedBool(ks.Object, "spec", "prune")
	return commitLocation{Location: loc, kustomization: ksNamespace + "/" + ksName, prune: prune}, nil
}

// explicitLocation is the target the caller named.
func explicitLocation(w WriteOptions) (commitLocation, error) {
	loc, err := provenance.Explicit(w.Repository, w.Branch, w.Path)
	if err != nil {
		return commitLocation{}, invalidf("commit target: %v", err)
	}
	return commitLocation{Location: loc}, nil
}

// namespaceLocation is where a new agent of ns goes: the caller's target, else
// the repository that owns the namespace's agents — the shared chart source,
// then any agent release applied from git, then the Namespace itself.
func (s *Service) namespaceLocation(ctx context.Context, dyn dynamic.Interface, ns string, w WriteOptions) (commitLocation, error) {
	if w.Repository != "" {
		return explicitLocation(w)
	}
	var candidates []*unstructured.Unstructured
	if src, err := s.getObject(ctx, dyn, s.ociRepositoryGVR(), ns, s.cfg.Compose.ChartName, "ocirepository"); err != nil {
		return commitLocation{}, err
	} else if src != nil {
		candidates = append(candidates, src)
	}
	hrs, err := s.agentHelmReleases(ctx, dyn, ns)
	if err != nil {
		return commitLocation{}, err
	}
	for _, name := range sortedKeys(hrs) {
		candidates = append(candidates, hrs[name])
	}
	if nsObj, err := dyn.Resource(namespaceGVR).Get(ctx, ns, metav1.GetOptions{}); err == nil {
		candidates = append(candidates, nsObj)
	}
	for _, obj := range candidates {
		loc, err := locationFromLabels(ctx, dyn, obj)
		if errors.Is(err, errNotFromGit) {
			continue
		}
		return loc, err
	}
	return commitLocation{}, invalidf("no Flux Kustomization owns the agents of namespace %s (neither its agent chart source, an agent release nor the Namespace carries %s): name the target with repository, branch and path, or use mode apply", ns, KustomizationNameLabel)
}

// releaseLocation is where an existing agent's release lives in git.
func (s *Service) releaseLocation(ctx context.Context, dyn dynamic.Interface, hr *unstructured.Unstructured, w WriteOptions) (commitLocation, error) {
	if w.Repository != "" {
		return explicitLocation(w)
	}
	loc, err := locationFromLabels(ctx, dyn, hr)
	if errors.Is(err, errNotFromGit) {
		return loc, conflictf("HelmRelease %s/%s is not applied from git: use mode apply", hr.GetNamespace(), hr.GetName())
	}
	return loc, err
}

// gitOpsRefusal is apply mode's answer for a release applied from git: it
// names mode commit and, when it can be read, the target.
func (s *Service) gitOpsRefusal(ctx context.Context, dyn dynamic.Interface, hr *unstructured.Unstructured) error {
	msg := fmt.Sprintf("HelmRelease %s/%s is applied from git by Flux Kustomization %q: a live write would be undone on the next reconciliation", hr.GetNamespace(), hr.GetName(), hr.GetLabels()[KustomizationNameLabel])
	if loc, err := locationFromLabels(ctx, dyn, hr); err == nil {
		msg += ". Use mode commit to change it with a pull request in " + loc.describe()
	} else {
		msg += ". Use mode commit to change it with a pull request in the repository that owns it"
	}
	return gitOpsOwnedf("%s", msg)
}

// gitHubFor is the caller's GitHub remote, or the refusal naming the consent
// that gives one.
func (s *Service) gitHubFor(ctx context.Context) (GitHubRemote, *identity.GitHub, error) {
	gh, ok := identity.GitHubFromContext(ctx)
	if !ok {
		return nil, nil, authRequiredf("commit mode opens the pull request with your GitHub authorization, and this call carries none: connect agent-manager in muster (core_auth_login server=agent-manager), then call again")
	}
	remote, err := s.cfg.GitHub(gh.Token)
	if err != nil {
		return nil, nil, commitError(err)
	}
	return remote, gh, nil
}

// commitError answers a token GitHub refused with the consent to renew.
func commitError(err error) error {
	var auth *commit.AuthError
	if errors.As(err, &auth) {
		return authRequiredf("GitHub refused your token on %s (status %d): reconnect agent-manager in muster (core_auth_login server=agent-manager); the App must be installed on the repository and your account allowed to push to it", auth.Op, auth.Status)
	}
	var secret *layout.SecretError
	if errors.As(err, &secret) {
		return invalidf("%s: the repository encrypts these paths and %s", strings.Join(secret.Paths, ", "), secret.Reason)
	}
	return fmt.Errorf("commit: %w", err)
}

// agentCommit is one write in commit mode: where it goes and as whom.
type agentCommit struct {
	loc    commitLocation
	remote GitHubRemote
	gh     *identity.GitHub
}

func (s *Service) newCommit(ctx context.Context, loc commitLocation) (*agentCommit, error) {
	remote, gh, err := s.gitHubFor(ctx)
	if err != nil {
		return nil, err
	}
	return &agentCommit{loc: loc, remote: remote, gh: gh}, nil
}

func (c *agentCommit) file(kind, name string) string {
	return c.loc.directory().ObjectFile(kind, name)
}

// exists reports whether the base branch carries p.
func (c *agentCommit) exists(ctx context.Context, p string) (bool, error) {
	_, err := c.remote.ReadFile(ctx, c.loc.Repository, c.loc.Branch, p)
	if errors.Is(err, commit.ErrFileNotFound) {
		return false, nil
	}
	if err != nil {
		return false, commitError(err)
	}
	return true, nil
}

// requireReleaseFile refuses a release agent-manager's directory does not
// carry: it is defined elsewhere in the repository, where a second file of
// the same object would break the Kustomization's build.
func (c *agentCommit) requireReleaseFile(ctx context.Context, hr *unstructured.Unstructured) (string, error) {
	p := c.file("HelmRelease", hr.GetName())
	ok, err := c.exists(ctx, p)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", conflictf("HelmRelease %s/%s is not in agent-manager's directory %s of %s (no %s there): change it in the file that defines it", hr.GetNamespace(), hr.GetName(), c.loc.directory().Path(), c.loc.Repository, p)
	}
	return p, nil
}

// open decides the files against the base (layout.Build) and lands them as
// one pull request as the caller, or reports them on a dry run. Nothing to
// change opens none.
func (c *agentCommit) open(ctx context.Context, write map[string][]byte, remove []string, branch, title, body string, dryRun bool) (*CommitResult, error) {
	plan, err := layout.Build(ctx, c.remote, c.loc.directory(), write, remove)
	if err != nil {
		return nil, commitError(err)
	}
	out := &CommitResult{
		Repository: c.loc.Repository.String(), Base: c.loc.Branch, Directory: c.loc.directory().Path(),
		Kustomization: c.loc.kustomization, Prune: c.loc.prune, Branch: branch, Author: c.gh.Login,
		Files: make([]CommitFile, len(plan.Files)),
	}
	for i, f := range plan.Files {
		out.Files[i] = CommitFile{Path: f.Path, Action: string(f.Action)}
		if dryRun {
			out.Files[i].Content = string(f.Content)
		}
	}
	if dryRun || !plan.Changed() {
		return out, nil
	}
	prs, err := commit.Open(ctx, c.remote, commit.Request{Branch: branch, Title: title, Body: body + "\n\nOpened by agent-manager as @" + c.gh.Login + "."}, []commit.Change{plan.Change()})
	if err != nil {
		return nil, commitError(err)
	}
	out.PullRequest, out.Number = prs[0].URL, prs[0].Number
	return out, nil
}

// commitBranch is the pull request's head branch for a verb on an agent.
func commitBranch(verb, ns, name string) string {
	return fmt.Sprintf("%s/%s-%s-%s", CommitDirectory, verb, ns, name)
}

// requireOwnFile refuses an agent named like the chart source in commit
// mode: both would be the same file of the directory.
func (s *Service) requireOwnFile(name string) error {
	if name == s.cfg.Compose.ChartName {
		return invalidf("an agent named %q shares its file with the namespace's agent chart source (OCIRepository %s) in commit mode: pick another name", name, name)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
