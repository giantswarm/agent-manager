package agents

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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

// GitHubRemote is what commit mode needs of GitHub: the pull request seams,
// the reads of the base (at a branch, or at one commit its head resolves to)
// and the files a merged pull request changed, acting as the person.
type GitHubRemote interface {
	commit.Remote
	commit.Reader
	commit.Lister
	commit.HeadReader
	commit.MergedFilesLister
	commit.PrefixFinder
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

// namespaceLocation is where a new agent of the site goes: the caller's target, else
// the repository that owns the namespace's agents — the shared chart source,
// then any agent release applied from git, then the Namespace itself.
func (s *Service) namespaceLocation(ctx context.Context, st *site, w WriteOptions) (commitLocation, error) {
	if w.Repository != "" {
		return explicitLocation(w)
	}
	dyn, ns := st.flux, st.fluxNS
	var candidates []*unstructured.Unstructured
	if src, err := s.getObject(ctx, dyn, s.ociRepositoryGVR(), ns, s.cfg.Compose.ChartName, "ocirepository"); err != nil {
		return commitLocation{}, err
	} else if src != nil {
		candidates = append(candidates, src)
	}
	hrs, err := s.agentHelmReleases(ctx, st)
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
	if s.cfg.GitHubUser == nil {
		return nil, nil, fmt.Errorf("%w: commit mode needs the GitHub grant exchange (github.tokenExchange), which this server is not configured with", ErrUnsupported)
	}
	gh, err := s.cfg.GitHubUser(ctx)
	if err != nil {
		return nil, nil, err
	}
	if gh == nil || gh.Token == "" {
		return nil, nil, errors.New("the GitHub grant exchange returned no grant")
	}
	remote, err := s.cfg.GitHub(gh.Token)
	if err != nil {
		return nil, nil, commitError(gh, err)
	}
	return remote, gh, nil
}

// readOps are the remote operations that only read: GitHub refusing one with
// 403 is the App's permissions or installation, never the person's push
// access, and no new sign-in or grant changes it.
var readOps = []string{commit.OpStatus, commit.OpHead, commit.OpReadFile, commit.OpListFiles, commit.OpFindPullRequest, commit.OpFindByPrefix, commit.OpMergedFiles}

// GitHubRefusal answers GitHub refusing gh's grant of its provider App
// (gh.App) on an operation of commit mode with what renews it: a 401 asks for
// that grant again, and a 403 names what the App and the person lack, which
// no new grant gives. A 403 on a read names the step and the App's
// permissions.
func GitHubRefusal(gh *identity.GitHub, auth *commit.AuthError) error {
	switch {
	case auth.Status == http.StatusForbidden && slices.Contains(readOps, auth.Op):
		return fmt.Errorf("%w: GitHub refused the step %q (status %d) for the permissions of the GitHub App %s: the App lacks the permission for this read or is not installed on the repository, which a new sign-in does not change", ErrForbidden, auth.Op, auth.Status, gh.App)
	case auth.Status == http.StatusUnauthorized:
		return &GrantRequiredError{App: gh.App, Reason: fmt.Sprintf("GitHub refused your grant of the GitHub App %s on %s: grant it again in muster (core_auth_login app=%s), then call again", gh.App, auth.Op, gh.App)}
	}
	return fmt.Errorf("%w: GitHub refused %s (status %d) with your grant of the GitHub App %s: the App must be installed on the repository and your account allowed to push to it", ErrForbidden, auth.Op, auth.Status, gh.App)
}

// commitError answers a token GitHub refused with what renews it.
func commitError(gh *identity.GitHub, err error) error {
	var auth *commit.AuthError
	if errors.As(err, &auth) {
		return GitHubRefusal(gh, auth)
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
	log    *slog.Logger
}

func (s *Service) newCommit(ctx context.Context, loc commitLocation) (*agentCommit, error) {
	remote, gh, err := s.gitHubFor(ctx)
	if err != nil {
		return nil, err
	}
	return &agentCommit{loc: loc, remote: remote, gh: gh, log: s.log}, nil
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
		return false, commitError(c.gh, err)
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
// change opens none. object is the agent's branch name (commitBranch). While
// a pull request of it is open, the write adds its commit to that pull
// request, as when every write of the object shared one head; otherwise the
// write cuts a head branch of its own (Head) from the base, so no file of an
// earlier, closed or merged, pull request of the agent rides along.
func (c *agentCommit) open(ctx context.Context, write map[string][]byte, remove []string, object, title, body string, dryRun bool) (*CommitResult, error) {
	plan, err := layout.Build(ctx, c.remote, c.loc.directory(), write, remove)
	if err != nil {
		return nil, commitError(c.gh, err)
	}
	out := &CommitResult{
		Repository: c.loc.Repository.String(), Base: c.loc.Branch, Directory: c.loc.directory().Path(),
		Kustomization: c.loc.kustomization, Prune: c.loc.prune, Branch: Head(object), Author: c.gh.Login,
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
	open, err := PullRequestsOf(ctx, c.remote, c.loc.Repository, object)
	if err != nil {
		return nil, commitError(c.gh, err)
	}
	if len(open) > 0 {
		out.Branch = open[0].Head
	}
	prs, err := commit.Open(ctx, c.remote, commit.Request{Branch: out.Branch, Title: title, Body: body + "\n\nOpened by agent-manager as @" + c.gh.Login + "."}, []commit.Change{plan.Change()})
	if err != nil {
		return nil, commitError(c.gh, err)
	}
	pr := prs[0]
	if len(open) > 0 && pr.Number != open[0].Number {
		// The pull request found open was closed or merged after the lookup,
		// and the commit landed on its branch in another pull request: one
		// this call opened, or one another writer opened there meanwhile. It
		// can carry the closed one's files. Nothing proves this call opened
		// it, so the write closes nothing and names both to the caller.
		return nil, fmt.Errorf("%w: the pull request %s of this agent was closed or merged while this call added to it, and the change is now in %s on the same branch, which can carry the files of %s: review %s, or close it and call again for a pull request of its own", ErrConflict, open[0].URL, pr.URL, open[0].URL, pr.URL)
	}
	if len(open) == 0 {
		// Another write of the agent may have opened its pull request since
		// the lookup: the one with the lower number stays.
		if first, lost, err := KeepFirst(ctx, c.log, c.remote, c.loc.Repository, object, pr); lost {
			return nil, RaceConflict("agent", first, pr, err, "call again to add this change to it")
		}
	}
	out.PullRequest, out.Number = pr.URL, pr.Number
	return out, nil
}

// commitBranch is the branch name of a verb on an agent, which every write
// of it shares: each pull request's head is this name, HeadSeparator and a
// suffix (Head); an older agent-manager used the name itself as the head.
func commitBranch(verb, ns, name string) string {
	return fmt.Sprintf("%s/%s-%s-%s", CommitDirectory, verb, ns, name)
}

// HeadSeparator ends an object's branch name before a write's suffix. No
// part of the name carries it (Kubernetes names, verbs and slugs have no
// underscore), so the prefix of one object never matches a head of another
// whose name starts with the same characters (sre, sre-eu). It is not "/":
// the branch of an older agent-manager, named like the object, would block
// every ref below it.
const HeadSeparator = "_"

// Head is a write's own head branch: the object's branch name, the separator
// and a random suffix. A new name is cut from the base, never from the
// branch of an earlier pull request.
func Head(object string) string {
	// The suffix is for uniqueness, not secrecy; crypto/rand never fails.
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	return object + HeadSeparator + hex.EncodeToString(suffix)
}

// PullRequestsOf returns the open pull requests of an object's branch name,
// sorted by number: those on a head of a write (the name, HeadSeparator and
// a suffix), and the one on the name itself, the head an older agent-manager
// opened. The lookup of the name can go once no pull request of an
// agent-manager older than per-write heads is open on an installation
// (giantswarm/agent-manager#385, giantswarm/agent-manager#433).
func PullRequestsOf(ctx context.Context, remote GitHubRemote, repo commit.Repository, object string) ([]commit.PullRequest, error) {
	prs, err := remote.FindPullRequestsByPrefix(ctx, repo, object+HeadSeparator)
	if err != nil {
		return nil, err
	}
	old, has, err := remote.FindPullRequest(ctx, repo, object)
	if err != nil {
		return nil, err
	}
	if has {
		prs = append(prs, old)
		slices.SortFunc(prs, func(a, b commit.PullRequest) int { return cmp.Compare(a.Number, b.Number) })
	}
	return prs, nil
}

// KeepFirst runs after a write opened own, a new pull request of object,
// when its lookup found none open: two writes of the object at almost the
// same time can each open one. The one with the lower number stays. When an
// open pull request of the object has a lower number than own, the write
// closes own (its branch stays, as a refused write's does) and KeepFirst
// returns that one with lost true, and the error of the close, if any. A
// pull request the write did not open is never closed. A failed lookup
// keeps own and logs it on log (slog's default when nil): the write is
// done, and a rare second pull request is the reviewer's to close.
func KeepFirst(ctx context.Context, log *slog.Logger, remote GitHubRemote, repo commit.Repository, object string, own commit.PullRequest) (commit.PullRequest, bool, error) {
	open, err := PullRequestsOf(ctx, remote, repo, object)
	if err != nil {
		if log == nil {
			log = slog.Default()
		}
		log.WarnContext(ctx, "commit mode: the check for a second open pull request of the object failed; this write's pull request stays", "repository", repo.String(), "object", object, "pullRequest", own.URL, "error", err)
		return commit.PullRequest{}, false, nil
	}
	for _, pr := range open {
		if pr.Number < own.Number {
			return pr, true, remote.Close(ctx, own, false)
		}
	}
	return commit.PullRequest{}, false, nil
}

// RaceConflict answers a write that lost KeepFirst to first: what the
// caller does while first is open (next), and that this call opened own at
// the same time and closed it; when the close failed, it names both pull
// requests and asks the caller to close own.
func RaceConflict(what string, first, own commit.PullRequest, closeErr error, next string) error {
	if closeErr != nil {
		return fmt.Errorf("%w: the pull request %s for this %s is open, and this call opened %s for it at the same time; closing %s failed (%v): close %s yourself, then %s", ErrConflict, first.URL, what, own.URL, own.URL, closeErr, own.URL, next)
	}
	return fmt.Errorf("%w: the pull request %s for this %s is open: %s (this call opened %s at the same time and closed it)", ErrConflict, first.URL, what, next, own.URL)
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
