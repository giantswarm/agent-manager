package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"filippo.io/age"
	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/giantswarm/gitops-commit/commit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/organizations"
)

var mcRepo = commit.Repository{Owner: "giantswarm", Name: "gazelle-config"}

// The layout the commit-mode tests write against: the installation's
// Kustomization applies objectsPath, whose kustomization.yaml lists the
// parents; each organization's own Kustomization applies its content path.
const (
	objectsPath  = "management-clusters/gazelle/agent-platform-organizations"
	paymentsFile = objectsPath + "/org-acme/payments.yaml"
	paymentsPath = "agent-platform/organizations/org-acme/payments"
)

// sopsCovering is a .sops.yaml whose one creation rule covers pathRegex,
// for a fresh age key; no private key is kept and nothing is decrypted.
func sopsCovering(t *testing.T, pathRegex string) []byte {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	return []byte("creation_rules:\n  - age: " + id.Recipient().String() + "\n    path_regex: " + pathRegex + "\n    encrypted_regex: ^(data|stringData)$\n")
}

// commitFixture is newOrgFixture with mode commit configured over
// gitops-commit's fake forge, and a caller who holds a GitHub grant. base
// seeds the repository's main branch.
func commitFixture(t *testing.T, base map[string][]byte, objs ...runtime.Object) (*orgFixture, *commit.Fake) {
	t.Helper()
	return commitFixtureWith(t, base, nil, objs...)
}

// commitFixtureWith is commitFixture whose remote is wrap of the fake forge,
// when wrap is given: a test steps in between the remote's calls.
func commitFixtureWith(t *testing.T, base map[string][]byte, wrap func(*commit.Fake) organizations.GitHubRemote, objs ...runtime.Object) (*orgFixture, *commit.Fake) {
	t.Helper()
	fake := commit.NewFake()
	var remoteOf organizations.GitHubRemote = fake
	if wrap != nil {
		remoteOf = wrap(fake)
	}
	files := map[string][]byte{
		objectsPath + "/kustomization.yaml": []byte("resources: []\n"),
		".sops.yaml":                        sopsCovering(t, `^agent-platform/organizations/.*\.enc\.yaml$`),
	}
	for p, c := range base {
		files[p] = c
	}
	fake.AddBranch(mcRepo, "main", files)
	f := newOrgFixtureWith(t, func(cfg *organizations.Config) {
		remote := func(token string) (organizations.GitHubRemote, error) {
			require.Equal(t, "ghu_person", token)
			return remoteOf, nil
		}
		var err error
		cfg.Commit, err = organizations.NewCommitConfig(remote, mcRepo.String(), "main", objectsPath, "")
		require.NoError(t, err)
	}, objs...)
	f.ctx = identity.ContextWithGitHub(f.ctx, &identity.GitHub{Login: "jane", Token: "ghu_person"})
	return f, fake
}

func parseFile(t *testing.T, raw []byte) *unstructured.Unstructured {
	t.Helper()
	obj := map[string]any{}
	require.NoError(t, yaml.Unmarshal(raw, &obj), string(raw))
	return &unstructured.Unstructured{Object: obj}
}

// fromGitLabels are the labels Flux puts on what a Kustomization applies.
func fromGitLabels(ks string) map[string]string {
	return map[string]string{agents.KustomizationNameLabel: ks, agents.KustomizationNamespaceLabel: "flux-giantswarm"}
}

// A create in commit mode opens one pull request as the person with the
// object's file under its parent and the first admins in its content path,
// both kept from Flux's prune; nothing is written live. What the caller's
// RBAC refuses is refused before any pull request.
func TestCreateOrganizationInCommitModeOpensOnePullRequest(t *testing.T) {
	f, fake := commitFixture(t, nil)
	args := map[string]any{"namespace": "org-acme", "name": "payments", "displayName": "Payments", "adminGroups": []any{"payments-leads"}, "mode": "commit"}

	text, isErr := f.call(t, ToolCreateOrganization, args)
	require.False(t, isErr, text)
	res := decode(t, text)
	assert.Nil(t, f.get(t, orgGVR, "org-acme", "payments"), "commit mode writes nothing live")
	require.Len(t, fake.PullRequests(), 1)
	pr := fake.PullRequests()[0]
	assert.Equal(t, pr.URL, res["commit"].(map[string]any)["pullRequest"])
	assert.Equal(t, "feat(organizations): add organization org-acme/payments", pr.Title)
	assert.Contains(t, pr.Body, "@jane")

	head := fake.Files(mcRepo, pr.Head)
	obj := parseFile(t, head[paymentsFile])
	assert.Equal(t, "AgentPlatformOrganization", obj.GetKind())
	assert.Equal(t, "org-acme", obj.GetNamespace())
	assert.Equal(t, organizations.PruneDisabled, obj.GetAnnotations()[organizations.PruneAnnotation])
	assert.Contains(t, string(head[objectsPath+"/kustomization.yaml"]), "org-acme", "the parent's directory is listed")
	binding := parseFile(t, head[paymentsPath+"/rolebinding-organization-admins.yaml"])
	assert.Equal(t, organizations.FirstAdminsBinding, binding.GetName())
	assert.Empty(t, binding.GetNamespace(), "the organization's Kustomization sets the namespace, which does not exist yet")
	assert.Equal(t, organizations.PruneDisabled, binding.GetAnnotations()[organizations.PruneAnnotation], "admission admits the Flux identity's apply only with prune disabled")
	assert.Equal(t, "payments-leads", binding.Object["subjects"].([]any)[0].(map[string]any)["name"])
	var dry []string
	for _, d := range f.dryRuns.Received() {
		dry = append(dry, d.Object.GetKind()+"/"+d.Object.GetName())
	}
	assert.Equal(t, []string{"AgentPlatformOrganization/payments"}, dry, "the object is dry-run in its parent as the caller")

	// A caller who may not bind the first admins opens nothing.
	g, gfake := commitFixture(t, nil)
	g.deny = func(_, verb, resource, _ string) bool { return verb == "bind" && resource == "clusterroles" }
	text, isErr = g.call(t, ToolCreateOrganization, args)
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "forbidden:"), text)
	assert.Empty(t, gfake.PullRequests())

	// Nor one whose create the API server's dry run refuses.
	h, hfake := commitFixture(t, nil)
	h.dryRuns.Refuse = func(u *unstructured.Unstructured) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: u.GetKind()}, u.GetName(), errors.New("denied by policy"))
	}
	text, isErr = h.call(t, ToolCreateOrganization, args)
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "forbidden:"), text)
	assert.Empty(t, hfake.PullRequests())
}

// gitOrganization is payments applied from git: its live object carries
// Flux's labels, its file is under org-acme, and its content path holds the
// first admins, a model with its credential and a nested directory.
func gitOrganization(t *testing.T) (*orgFixture, *commit.Fake) {
	t.Helper()
	live := organization("org-acme", "payments", fromGitLabels("flux"))
	live.SetAnnotations(map[string]string{organizations.PruneAnnotation: organizations.PruneDisabled})
	file := organization("org-acme", "payments", nil)
	delete(file.Object, "status")
	file.SetAnnotations(map[string]string{organizations.PruneAnnotation: organizations.PruneDisabled})
	fileYAML, err := yaml.Marshal(file.Object)
	require.NoError(t, err)
	claude := model("claude", true)
	claude.SetLabels(fromGitLabels("agents-payments-x7k2q"))
	claudeFile := model("claude", true)
	claudeFile.SetNamespace("")
	claudeYAML, err := yaml.Marshal(claudeFile.Object)
	require.NoError(t, err)
	admins := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": organizations.FirstAdminsBinding, "namespace": orgNS},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": organizations.RoleAdmin},
		"subjects": []any{map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Group", "name": "payments-leads"}},
	}}
	adminsFile := admins.DeepCopy()
	adminsFile.SetNamespace("")
	adminsYAML, err := yaml.Marshal(adminsFile.Object)
	require.NoError(t, err)
	admins.SetLabels(fromGitLabels("agents-payments-x7k2q"))
	return commitFixture(t, map[string][]byte{
		objectsPath + "/kustomization.yaml":          []byte("resources:\n  - org-acme\n"),
		objectsPath + "/org-acme/kustomization.yaml": []byte("resources:\n  - payments.yaml\n"),
		paymentsFile:                                                       fileYAML,
		paymentsPath + "/kustomization.yaml":                               []byte("resources:\n  - modelconfig-claude.yaml\n  - rolebinding-organization-admins.yaml\n  - tenant-credential-claude-secret.enc.yaml\n  - agents\n"),
		paymentsPath + "/modelconfig-claude.yaml":                          claudeYAML,
		paymentsPath + "/rolebinding-organization-admins.yaml":             adminsYAML,
		paymentsPath + "/tenant-credential-claude-secret.enc.yaml":         []byte("sops: {}\n"),
		paymentsPath + "/agents/kustomization.yaml":                        []byte("resources:\n  - sre.yaml\n"),
		paymentsPath + "/agents/sre.yaml":                                  []byte("kind: HelmRelease\n"),
		"agent-platform/organizations/org-acme/billing/modelconfig-x.yaml": []byte("kind: ModelConfig\n"),
	}, live, claude, admins)
}

// Delete in commit mode, two steps: the pull request removes the object's
// file and the whole content path and the live object is marked with it; a
// live delete is refused while the file is in git; after the merge, which
// removed the file, Erase now marks the delete intent and deletes.
func TestDeleteOrganizationInCommitModeTakesTwoSteps(t *testing.T) {
	f, fake := gitOrganization(t)
	del := func(confirm string) (string, bool) {
		args := map[string]any{"namespace": "org-acme", "name": "payments", "mode": "commit"}
		if confirm != "" {
			args["confirm"] = confirm
		}
		return f.call(t, ToolDeleteOrganization, args)
	}

	text, isErr := del("org-acme/payments")
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "conflict:"), text)
	assert.Contains(t, text, "still in git")
	assert.Empty(t, fake.PullRequests())

	text, isErr = del("")
	require.False(t, isErr, text)
	require.Len(t, fake.PullRequests(), 1)
	pr := fake.PullRequests()[0]
	head := fake.Files(mcRepo, pr.Head)
	for p := range head {
		assert.False(t, strings.HasPrefix(p, paymentsPath+"/"), "%s stays in the content path", p)
	}
	assert.NotContains(t, head, paymentsFile)
	assert.NotContains(t, head, objectsPath+"/org-acme/kustomization.yaml", "the emptied parent directory goes")
	assert.Contains(t, head, "agent-platform/organizations/org-acme/billing/modelconfig-x.yaml", "another organization's path stays")
	live := f.get(t, orgGVR, "org-acme", "payments")
	require.NotNil(t, live)
	assert.Equal(t, pr.URL, live.GetAnnotations()[organizations.DeletePullRequestAnnotation])
	assert.Equal(t, organizations.PruneDisabled, live.GetAnnotations()[organizations.PruneAnnotation], "nothing is erased before the merge")
	assert.Contains(t, decode(t, text)["next"], "Erase now")

	// While it is open, the call names it and opens no other.
	text, isErr = del("")
	require.False(t, isErr, text)
	assert.Contains(t, decode(t, text)["next"], "is open")
	assert.Len(t, fake.PullRequests(), 1)

	require.NoError(t, fake.Merge(context.Background(), pr.PullRequest))
	text, isErr = del("")
	require.False(t, isErr, text)
	assert.Equal(t, false, decode(t, text)["deleted"])
	assert.Contains(t, decode(t, text)["next"], "pending erase")
	assert.NotNil(t, f.get(t, orgGVR, "org-acme", "payments"))

	text, isErr = del("org-acme/payments")
	require.False(t, isErr, text)
	assert.Equal(t, true, decode(t, text)["deleted"])
	assert.Nil(t, f.get(t, orgGVR, "org-acme", "payments"))
}

// agent-manager's GitHub App has no permission for the commit statuses and
// check runs of a pull request's head, and GitHub refuses them with 403. The
// delete reads its pull request with the pull request reads alone (merged,
// and its files), so every step, the erase too, works without them; a read
// refused with 403 names the step and the App's permissions, not a new
// sign-in.
func TestTheDeleteInCommitModeReadsNoStatusesOrChecks(t *testing.T) {
	f, fake := gitOrganization(t)
	fake.Fail[commit.OpStatus] = &commit.AuthError{Op: commit.OpStatus, Status: http.StatusForbidden}
	del := func(confirm string) (string, bool) {
		args := map[string]any{"namespace": "org-acme", "name": "payments", "mode": "commit"}
		if confirm != "" {
			args["confirm"] = confirm
		}
		return f.call(t, ToolDeleteOrganization, args)
	}

	text, isErr := del("")
	require.False(t, isErr, text)
	require.Len(t, fake.PullRequests(), 1)
	pr := fake.PullRequests()[0]

	text, isErr = del("")
	require.False(t, isErr, text)
	assert.Contains(t, decode(t, text)["next"], "is open")

	text, isErr = del("org-acme/payments")
	require.True(t, isErr)
	assert.Contains(t, text, "still in git")

	require.NoError(t, fake.Merge(context.Background(), pr.PullRequest))
	text, isErr = del("")
	require.False(t, isErr, text)
	assert.Contains(t, decode(t, text)["next"], "pending erase")

	// A refused read of the merged files names the step and the App's
	// permissions, erases nothing and asks for no new sign-in.
	fake.Fail[commit.OpMergedFiles] = &commit.AuthError{Op: commit.OpMergedFiles, Status: http.StatusForbidden}
	text, isErr = del("org-acme/payments")
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "forbidden:"), text)
	assert.Contains(t, text, commit.OpMergedFiles)
	assert.Contains(t, text, "permissions")
	assert.Contains(t, text, "nothing was erased")
	assert.NotContains(t, text, "core_auth_login")
	assert.NotContains(t, text, "push")
	assert.NotNil(t, f.get(t, orgGVR, "org-acme", "payments"))

	delete(fake.Fail, commit.OpMergedFiles)
	text, isErr = del("org-acme/payments")
	require.False(t, isErr, text)
	assert.Equal(t, true, decode(t, text)["deleted"])
	assert.Nil(t, f.get(t, orgGVR, "org-acme", "payments"))
}

// The recovery from a listing that keeps failing: restore the file and open
// the delete again. While the file is in git the earlier pull request's
// files are not needed, so a listing that fails does not stop the new delete
// pull request.
func TestARestoredFileOpensANewDeleteWhenTheListingKeepsFailing(t *testing.T) {
	ctx := context.Background()
	f, fake := gitOrganization(t)
	file := fake.Files(mcRepo, "main")[paymentsFile]
	del := map[string]any{"namespace": "org-acme", "name": "payments", "mode": "commit"}
	text, isErr := f.call(t, ToolDeleteOrganization, del)
	require.False(t, isErr, text)
	require.Len(t, fake.PullRequests(), 1)
	first := fake.PullRequests()[0]
	require.NoError(t, fake.Merge(ctx, first.PullRequest))
	fake.Fail[commit.OpMergedFiles] = errors.New("GitHub listed 3000 of 3001 changed files")

	require.NoError(t, fake.Commit(ctx, mcRepo, "main", "restore", map[string][]byte{paymentsFile: file}))
	text, isErr = f.call(t, ToolDeleteOrganization, del)
	require.False(t, isErr, text)
	require.Len(t, fake.PullRequests(), 2)
	second := fake.PullRequests()[1]
	assert.NotContains(t, fake.Files(mcRepo, second.Head), paymentsFile)
	assert.Equal(t, second.URL, f.get(t, orgGVR, "org-acme", "payments").GetAnnotations()[organizations.DeletePullRequestAnnotation])
}

// A merged pull request counts as the organization's delete only when the
// file is gone from git: one that left it, or a file gone without one, is
// no reason to erase.
func TestAMergedPullRequestThatKeptTheFileIsNotTheDelete(t *testing.T) {
	f, fake := gitOrganization(t)
	// An unrelated pull request, merged, that the object names.
	require.NoError(t, fake.CreateBranch(context.Background(), mcRepo, "other", "main"))
	require.NoError(t, fake.Commit(context.Background(), mcRepo, "other", "docs", map[string][]byte{"README.md": []byte("x")}))
	other, err := fake.OpenPullRequest(context.Background(), mcRepo, "other", "main", "docs", "")
	require.NoError(t, err)
	require.NoError(t, fake.Merge(context.Background(), other))
	live := f.get(t, orgGVR, "org-acme", "payments")
	ann := live.GetAnnotations()
	ann[organizations.DeletePullRequestAnnotation] = other.URL
	live.SetAnnotations(ann)
	_, err = f.dyn.Resource(orgGVR).Namespace("org-acme").Update(context.Background(), live, metav1.UpdateOptions{})
	require.NoError(t, err)

	text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "mode": "commit", "confirm": "org-acme/payments"})
	require.True(t, isErr)
	assert.Contains(t, text, "did not remove it")
	assert.NotNil(t, f.get(t, orgGVR, "org-acme", "payments"))

	// The file gone from git by another merge, without agent-manager's
	// delete: Missing from git, never erased.
	g, gfake := gitOrganization(t)
	require.NoError(t, gfake.Commit(context.Background(), mcRepo, "main", "oops", map[string][]byte{paymentsFile: nil}))
	text, isErr = g.call(t, ToolDeleteOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "mode": "commit", "confirm": "org-acme/payments"})
	require.True(t, isErr)
	assert.Contains(t, text, "Missing from git")
	assert.NotNil(t, g.get(t, orgGVR, "org-acme", "payments"))
}

// The erase needs the merged delete pull request itself to have removed the
// object's file. A pull request edited before its merge erases nothing: one
// that kept the file, which another merge then removed, and one that moved
// it, so the organization is still in git. Nor does one whose files GitHub
// could not list.
func TestAMergedDeleteThatDidNotRemoveTheFileErasesNothing(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// edit changes the delete pull request's branch before the merge, and
		// after receives the main branch's file content.
		edit  func(t *testing.T, fake *commit.Fake, head string, file []byte)
		after func(t *testing.T, fake *commit.Fake)
		want  []string
		// noConfirm: the call without confirm is refused too, and does not
		// answer pending erase.
		noConfirm bool
	}{
		{
			name: "the removal was reverted and another merge removed the file",
			edit: func(t *testing.T, fake *commit.Fake, head string, file []byte) {
				require.NoError(t, fake.Commit(ctx, mcRepo, head, "keep it", map[string][]byte{paymentsFile: file}))
			},
			after: func(t *testing.T, fake *commit.Fake) {
				require.NoError(t, fake.Commit(ctx, mcRepo, "main", "oops", map[string][]byte{paymentsFile: nil}))
			},
			want:      []string{"conflict:", "did not remove it", "restore its file"},
			noConfirm: true,
		},
		{
			name: "the file was moved",
			edit: func(t *testing.T, fake *commit.Fake, head string, file []byte) {
				require.NoError(t, fake.Commit(ctx, mcRepo, head, "move it", map[string][]byte{objectsPath + "/archive/payments.yaml": file}))
			},
			want: []string{"conflict:", "moved its file to " + objectsPath + "/archive/payments.yaml", "move the file back"},
		},
		{
			name: "the files cannot be listed",
			after: func(t *testing.T, fake *commit.Fake) {
				fake.Fail[commit.OpMergedFiles] = errors.New("boom")
			},
			want: []string{"nothing was erased"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, fake := gitOrganization(t)
			file := fake.Files(mcRepo, "main")[paymentsFile]
			text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "mode": "commit"})
			require.False(t, isErr, text)
			require.Len(t, fake.PullRequests(), 1)
			pr := fake.PullRequests()[0]
			merge := pr.PullRequest
			if tc.edit != nil {
				tc.edit(t, fake, pr.Head, file)
				st, err := fake.Status(ctx, merge)
				require.NoError(t, err)
				merge.HeadSHA = st.HeadSHA
			}
			require.NoError(t, fake.Merge(ctx, merge))
			if tc.after != nil {
				tc.after(t, fake)
			}
			require.NotContains(t, fake.Files(mcRepo, "main"), paymentsFile, "the file is gone from git")
			if tc.noConfirm {
				text, isErr = f.call(t, ToolDeleteOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "mode": "commit"})
				require.True(t, isErr, text)
				assert.NotContains(t, text, "pending erase:")
			}

			text, isErr = f.call(t, ToolDeleteOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "mode": "commit", "confirm": "org-acme/payments"})
			require.True(t, isErr, text)
			for _, w := range tc.want {
				assert.Contains(t, text, w)
			}
			live := f.get(t, orgGVR, "org-acme", "payments")
			require.NotNil(t, live, "nothing is erased")
			assert.Equal(t, organizations.PruneDisabled, live.GetAnnotations()[organizations.PruneAnnotation], "the delete intent is not written")
		})
	}
}

// The other tools write their files into the content path: a model's file
// keeps what git says (not what the API server defaulted live), moving the
// default mark rewrites the other model's file too; an update changes the
// object's file; apply mode refuses what git owns.
func TestContentToolsCommitIntoTheContentPath(t *testing.T) {
	f, fake := gitOrganization(t)
	seedSecret(t, f, "tenant-credential-haiku", "apiKey")
	text, isErr := f.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "haiku", "provider": "Anthropic", "model": "claude-haiku", "default": true})
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "gitops_owned:"), text)
	assert.Contains(t, text, "mode commit")

	text, isErr = f.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "haiku", "provider": "Anthropic", "model": "claude-haiku", "default": true, "mode": "commit"})
	require.False(t, isErr, text)
	require.Len(t, fake.PullRequests(), 1)
	head := fake.Files(mcRepo, fake.PullRequests()[0].Head)
	haiku := parseFile(t, head[paymentsPath+"/modelconfig-haiku.yaml"])
	assert.Equal(t, "true", haiku.GetAnnotations()[apov1alpha1.DefaultModelConfigAnnotation])
	assert.Empty(t, haiku.GetNamespace())
	claude := parseFile(t, head[paymentsPath+"/modelconfig-claude.yaml"])
	assert.NotContains(t, claude.GetAnnotations(), apov1alpha1.DefaultModelConfigAnnotation, "the mark moved off the other model's file")
	assert.NotContains(t, claude.GetLabels(), agents.KustomizationNameLabel, "Flux's labels stay out of git")
	assert.Contains(t, string(head[paymentsPath+"/kustomization.yaml"]), "modelconfig-haiku.yaml")
	assert.Nil(t, f.get(t, mcGVR, orgNS, "haiku"), "nothing live")

	// The first admins' file keeps Flux from pruning it, which admission
	// requires of the organization's Flux identity.
	text, isErr = f.call(t, ToolAddOrganizationMember, map[string]any{"namespace": orgNS, "role": "admin", "group": "payments-oncall", "mode": "commit"})
	require.False(t, isErr, text)
	require.Len(t, fake.PullRequests(), 2)
	admins := parseFile(t, fake.Files(mcRepo, fake.PullRequests()[1].Head)[paymentsPath+"/rolebinding-organization-admins.yaml"])
	assert.Len(t, admins.Object["subjects"], 2)
	assert.Equal(t, organizations.PruneDisabled, admins.GetAnnotations()[organizations.PruneAnnotation])

	text, isErr = f.call(t, ToolUpdateOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "workerReplicas": 3, "mode": "commit"})
	require.False(t, isErr, text)
	require.Len(t, fake.PullRequests(), 3)
	obj := parseFile(t, fake.Files(mcRepo, fake.PullRequests()[2].Head)[paymentsFile])
	replicas, _, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "workers", "replicas")
	assert.EqualValues(t, 3, replicas)
	assert.Equal(t, organizations.PruneDisabled, obj.GetAnnotations()[organizations.PruneAnnotation])
	assert.NotContains(t, obj.Object, "status")
}

// Every content tool lands its file in the content path in commit mode; a
// dry run opens nothing; a second change while the object's pull request
// is open is refused rather than undoing the first in it.
func TestEveryContentToolCommits(t *testing.T) {
	cases := []struct {
		tool   string
		args   map[string]any
		file   string
		action string
	}{
		{ToolSetOrganizationConnector, map[string]any{"name": "gh", "url": "https://mcp.example.com/mcp", "credentialHeader": "Authorization"}, "remotemcpserver-gh.yaml", "add"},
		{ToolSetOrganizationEgress, map[string]any{"name": "github", "destinations": []any{map[string]any{"origin": "https://api.github.com"}}}, "egresspolicy-github.yaml", "add"},
		{ToolDeleteOrganizationObject, map[string]any{"kind": "model", "name": "claude"}, "modelconfig-claude.yaml", "remove"},
		{ToolRemoveOrganizationMember, map[string]any{"group": "payments-leads"}, "rolebinding-organization-admins.yaml", "remove"},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			f, fake := gitOrganization(t)
			seedSecret(t, f, "tenant-credential-gh", "token")
			args := map[string]any{"namespace": orgNS, "mode": "commit", "dryRun": true}
			for k, v := range tc.args {
				args[k] = v
			}
			text, isErr := f.call(t, tc.tool, args)
			require.False(t, isErr, text)
			assert.Empty(t, fake.PullRequests(), "a dry run opens nothing")

			delete(args, "dryRun")
			text, isErr = f.call(t, tc.tool, args)
			require.False(t, isErr, text)
			require.Len(t, fake.PullRequests(), 1)
			files := map[string]string{}
			for _, file := range decode(t, text)["commit"].(map[string]any)["files"].([]any) {
				m := file.(map[string]any)
				files[m["path"].(string)] = m["action"].(string)
			}
			assert.Equal(t, tc.action, files[paymentsPath+"/"+tc.file])

			text, isErr = f.call(t, tc.tool, args)
			require.True(t, isErr)
			assert.True(t, strings.HasPrefix(text, "conflict:"), text)
			assert.Contains(t, text, fake.PullRequests()[0].URL)
			assert.Len(t, fake.PullRequests(), 1)
		})
	}
}

// A .sops.yaml rule that makes a plain object's file secret is refused:
// gitops-commit would encrypt the file once and never update it.
func TestAPlainFileTheRepositoryWouldEncryptIsRefused(t *testing.T) {
	f, fake := commitFixture(t, map[string][]byte{".sops.yaml": sopsCovering(t, `^agent-platform/organizations/.*`)})
	text, isErr := f.call(t, ToolCreateOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "adminGroups": []any{"g"}, "mode": "commit"})
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "invalid"), text)
	assert.Contains(t, text, "rolebinding-organization-admins.yaml")
	assert.Empty(t, fake.PullRequests())
}

// A mark that fails after the delete pull request opened is written by the
// next call, from the open pull request; no second one is opened.
func TestAFailedDeleteMarkIsWrittenByTheNextCall(t *testing.T) {
	f, fake := gitOrganization(t)
	failed := false
	f.dyn.PrependReactor("update", "agentplatformorganizations", func(a k8stesting.Action) (bool, runtime.Object, error) {
		u := a.(k8stesting.UpdateActionImpl)
		obj := u.GetObject().(*unstructured.Unstructured)
		if len(u.UpdateOptions.DryRun) == 0 && obj.GetAnnotations()[organizations.DeletePullRequestAnnotation] != "" && !failed {
			failed = true
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "agentplatformorganizations"}, obj.GetName(), errors.New("the object has been modified"))
		}
		return false, nil, nil
	})
	args := map[string]any{"namespace": "org-acme", "name": "payments", "mode": "commit"}
	text, isErr := f.call(t, ToolDeleteOrganization, args)
	require.True(t, isErr)
	assert.Contains(t, text, "call delete_organization again")
	require.Len(t, fake.PullRequests(), 1)
	assert.NotContains(t, f.get(t, orgGVR, "org-acme", "payments").GetAnnotations(), organizations.DeletePullRequestAnnotation)

	text, isErr = f.call(t, ToolDeleteOrganization, args)
	require.False(t, isErr, text)
	assert.Len(t, fake.PullRequests(), 1)
	assert.Equal(t, fake.PullRequests()[0].URL, f.get(t, orgGVR, "org-acme", "payments").GetAnnotations()[organizations.DeletePullRequestAnnotation])
}
