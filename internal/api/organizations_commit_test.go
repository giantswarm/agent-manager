package api

import (
	"context"
	"errors"
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
	fake := commit.NewFake()
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
			return fake, nil
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

// A credential lands as its own encrypted file at a secret path, never in
// plaintext, in the answer or in git; a path no creation rule covers is
// refused; an existing file is replaced only on request.
func TestCredentialsAreCommittedEncryptedOrRefused(t *testing.T) {
	const value = "sk-ant-value-1234" // #nosec G101 -- a test value, no credential
	secretFile := paymentsPath + "/tenant-credential-claude-secret.enc.yaml"
	req := organizations.CredentialCommit{Namespace: orgNS, Secret: "tenant-credential-claude", Key: "apiKey", Value: []byte(value)} // #nosec G101 -- a test value

	f, fake := commitFixture(t, nil)
	res, err := f.orgs.CommitCredential(f.ctx, req)
	require.NoError(t, err)
	require.Len(t, fake.PullRequests(), 1)
	assert.Equal(t, res.PullRequest, fake.PullRequests()[0].URL)
	head := fake.Files(mcRepo, fake.PullRequests()[0].Head)
	require.Contains(t, head, secretFile)
	assert.Contains(t, string(head[secretFile]), "sops:", "encrypted for the .sops.yaml recipients")
	for p, content := range head {
		assert.NotContains(t, string(content), value, "plaintext in %s", p)
	}
	for _, file := range res.Files {
		assert.NotContains(t, file.Content, value)
	}

	// Replacing needs the request; with it the file is re-encrypted.
	require.NoError(t, fake.Merge(context.Background(), fake.PullRequests()[0].PullRequest))
	_, err = f.orgs.CommitCredential(f.ctx, req)
	require.ErrorIs(t, err, agents.ErrConflict)
	req.Replace, req.Value = true, []byte("sk-ant-new-5678")
	res, err = f.orgs.CommitCredential(f.ctx, req)
	require.NoError(t, err)
	require.Len(t, fake.PullRequests(), 2)
	assert.Equal(t, "update", filesOf(res)[secretFile])
	for _, content := range fake.Files(mcRepo, fake.PullRequests()[1].Head) {
		assert.NotContains(t, string(content), "sk-ant-new-5678")
	}

	// A repository whose .sops.yaml covers no organization path takes none.
	g, gfake := commitFixture(t, map[string][]byte{".sops.yaml": sopsCovering(t, `^clusters/.*`)})
	_, err = g.orgs.CommitCredential(g.ctx, req) // #nosec G101 -- a test value
	require.ErrorIs(t, err, agents.ErrInvalid)
	assert.Contains(t, err.Error(), secretFile)
	assert.NotContains(t, err.Error(), value)
	assert.Empty(t, gfake.PullRequests())

	// A caller who may not write the Secret opens nothing.
	h, hfake := commitFixture(t, nil)
	h.deny = func(_, verb, resource, _ string) bool { return resource == "secrets" }
	_, err = h.orgs.CommitCredential(h.ctx, req) // #nosec G101 -- a test value
	require.ErrorIs(t, err, agents.ErrForbidden)
	assert.Empty(t, hfake.PullRequests())
}

func filesOf(c *organizations.CommitResult) map[string]string {
	out := map[string]string{}
	for _, f := range c.Files {
		out[f.Path] = f.Action
	}
	return out
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

// The other tools write their files into the content path: a model's file
// keeps what git says (not what the API server defaulted live), moving the
// default mark rewrites the other model's file too; an update changes the
// object's file; apply mode refuses what git owns.
func TestContentToolsCommitIntoTheContentPath(t *testing.T) {
	f, fake := gitOrganization(t)
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
