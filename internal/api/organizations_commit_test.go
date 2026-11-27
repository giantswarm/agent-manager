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
	"github.com/giantswarm/agent-manager/internal/organizations"
)

var mcRepo = commit.Repository{Owner: "giantswarm", Name: "gazelle-config"}

// The layout the commit-mode tests write against: the installation's
// Kustomization applies objectsPath, whose kustomization.yaml lists the
// organizations' files; each organization's own Kustomization applies its
// content path.
const (
	objectsPath  = "management-clusters/gazelle/agent-platform-organizations"
	paymentsFile = objectsPath + "/payments.yaml"
	paymentsPath = "agent-platform/organizations/payments"
	// aboveObjects is a kustomization above the objects path, outside it:
	// no organization write changes it.
	aboveObjects = "management-clusters/gazelle/kustomization.yaml"
)

// aboveObjectsContent is what aboveObjects holds on the base branch.
const aboveObjectsContent = "resources:\n  - apps\n"

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
		aboveObjects:                        []byte(aboveObjectsContent),
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
		cfg.Commit.User = func(context.Context) (*agents.GitHubUserAccessToken, error) {
			return &agents.GitHubUserAccessToken{Login: "jane", Token: "ghu_person", App: "agent-manager"}, nil // #nosec G101 -- fixture grant
		}
	}, objs...)
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
// Organization's file in the objects path, its admins in spec.admins, kept
// from Flux's prune; no RoleBinding file and nothing live. What the caller's
// RBAC refuses is refused before any pull request.
func TestCreateOrganizationInCommitModeOpensOnePullRequest(t *testing.T) {
	f, fake := commitFixture(t, nil)
	f.dropOrganization(t, "payments")
	args := map[string]any{"name": "payments", "displayName": "Payments", "adminGroups": []any{"payments-leads"}, "mode": "commit"}

	text, isErr := f.call(t, ToolCreateOrganization, args)
	require.False(t, isErr, text)
	res := decode(t, text)
	assert.Nil(t, f.get(t, orgGVR, "", "payments"), "commit mode writes nothing live")
	require.Len(t, fake.PullRequests(), 1)
	pr := fake.PullRequests()[0]
	assert.Equal(t, pr.URL, res["commit"].(map[string]any)["pullRequest"])
	assert.Equal(t, "feat(organizations): add organization payments", pr.Title)
	assert.Contains(t, pr.Body, "@jane")

	head := fake.Files(mcRepo, pr.Head)
	obj := parseFile(t, head[paymentsFile])
	assert.Equal(t, "Organization", obj.GetKind())
	assert.Equal(t, "security.giantswarm.io/v1alpha1", obj.GetAPIVersion())
	assert.Empty(t, obj.GetNamespace())
	assert.Equal(t, organizations.PruneDisabled, obj.GetAnnotations()[organizations.PruneAnnotation])
	assert.Equal(t, []any{map[string]any{"kind": "Group", "name": "payments-leads"}}, obj.Object["spec"].(map[string]any)["admins"])
	enabled, _, _ := unstructured.NestedBool(obj.Object, "spec", "capabilities", "agentPlatform", "enabled")
	assert.True(t, enabled, "the file switches the Agent Platform on in spec.capabilities.agentPlatform")
	assert.Contains(t, string(head[objectsPath+"/kustomization.yaml"]), "payments.yaml", "the file is listed")
	assert.Equal(t, aboveObjectsContent, string(head[aboveObjects]), "the kustomization above the objects path is not changed")
	// The content path starts as an empty kustomization, which the
	// organization's Kustomization builds; no admins file goes there.
	for p := range head {
		if strings.HasPrefix(p, "agent-platform/organizations/") {
			assert.Equal(t, paymentsPath+"/kustomization.yaml", p)
		}
	}
	assert.Equal(t, "resources: []\n", string(head[paymentsPath+"/kustomization.yaml"]))
	var dry []string
	for _, d := range f.dryRuns.Received() {
		dry = append(dry, d.Object.GetKind()+"/"+d.Object.GetName())
	}
	assert.Equal(t, []string{"Organization/payments"}, dry, "the object is dry-run as the caller")

	// A caller who may not create Organizations opens nothing.
	g, gfake := commitFixture(t, nil)
	g.dropOrganization(t, "payments")
	g.deny = func(_, verb, resource, _ string) bool { return verb == "create" && resource == "organizations" }
	text, isErr = g.call(t, ToolCreateOrganization, args)
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "forbidden:"), text)
	assert.Empty(t, gfake.PullRequests())

	// Nor one whose create the API server's dry run refuses.
	h, hfake := commitFixture(t, nil)
	h.dropOrganization(t, "payments")
	h.dryRuns.Refuse = func(u *unstructured.Unstructured) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: u.GetKind()}, u.GetName(), errors.New("denied by policy"))
	}
	text, isErr = h.call(t, ToolCreateOrganization, args)
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "forbidden:"), text)
	assert.Empty(t, hfake.PullRequests())
}

// gitOrganization is payments applied from git: its live object carries
// Flux's labels, its file is in the objects path, and its content path holds
// its editors, a model with its credential and a nested directory. Its
// admins are the operator's binding organization-admins, not a file.
func gitOrganization(t *testing.T) (*orgFixture, *commit.Fake) {
	t.Helper()
	return gitOrganizationWith(t, nil)
}

// gitOrganizationWith is gitOrganization whose remote is wrap of the fake
// forge, as commitFixtureWith.
func gitOrganizationWith(t *testing.T, wrap func(*commit.Fake) organizations.GitHubRemote) (*orgFixture, *commit.Fake) {
	t.Helper()
	live := organization("payments", fromGitLabels("flux"))
	live.SetAnnotations(map[string]string{organizations.PruneAnnotation: organizations.PruneDisabled})
	file := organization("payments", nil)
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
	binding := func(name, role, group string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
			"metadata": map[string]any{"name": name, "namespace": orgNS},
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": role},
			"subjects": []any{map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Group", "name": group}},
		}}
	}
	admins := binding("organization-admins", organizations.RoleAdmin, "payments-leads")
	editors := binding("organization-editors", organizations.RoleEditor, "payments-team")
	editorsFile := editors.DeepCopy()
	editorsFile.SetNamespace("")
	editorsYAML, err := yaml.Marshal(editorsFile.Object)
	require.NoError(t, err)
	editors.SetLabels(fromGitLabels("agents-payments-x7k2q"))
	return commitFixtureWith(t, map[string][]byte{
		objectsPath + "/kustomization.yaml":                        []byte("resources:\n  - payments.yaml\n"),
		paymentsFile:                                               fileYAML,
		paymentsPath + "/kustomization.yaml":                       []byte("resources:\n  - modelconfig-claude.yaml\n  - rolebinding-organization-editors.yaml\n  - tenant-credential-claude-secret.enc.yaml\n  - agents\n"),
		paymentsPath + "/modelconfig-claude.yaml":                  claudeYAML,
		paymentsPath + "/rolebinding-organization-editors.yaml":    editorsYAML,
		paymentsPath + "/tenant-credential-claude-secret.enc.yaml": []byte("sops: {}\n"),
		paymentsPath + "/agents/kustomization.yaml":                []byte("resources:\n  - sre.yaml\n"),
		paymentsPath + "/agents/sre.yaml":                          []byte("kind: HelmRelease\n"),
		"agent-platform/organizations/billing/modelconfig-x.yaml":  []byte("kind: ModelConfig\n"),
	}, wrap, live, claude, admins, editors)
}

// Delete in commit mode, two steps: the pull request removes the object's
// file and the whole content path and the live object is marked with it; a
// live delete is refused while the file is in git; after the merge, which
// removed the file, Erase now marks the delete intent and deletes.
func TestDeleteOrganizationInCommitModeTakesTwoSteps(t *testing.T) {
	f, fake := gitOrganization(t)
	del := func(confirm string) (string, bool) {
		args := map[string]any{"name": "payments", "mode": "commit"}
		if confirm != "" {
			args["confirm"] = confirm
		}
		return f.call(t, ToolDeleteOrganization, args)
	}

	text, isErr := del("payments")
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
	assert.Contains(t, head, "agent-platform/organizations/billing/modelconfig-x.yaml", "another organization's path stays")
	// The last organization's delete keeps the objects path that the
	// installation's Kustomization builds, and leaves the one above it.
	assert.Equal(t, "resources: []\n", string(head[objectsPath+"/kustomization.yaml"]))
	assert.Equal(t, aboveObjectsContent, string(head[aboveObjects]))
	live := f.get(t, orgGVR, "", "payments")
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
	assert.NotNil(t, f.get(t, orgGVR, "", "payments"))

	text, isErr = del("payments")
	require.False(t, isErr, text)
	assert.Equal(t, true, decode(t, text)["deleted"])
	assert.Nil(t, f.get(t, orgGVR, "", "payments"))
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
		args := map[string]any{"name": "payments", "mode": "commit"}
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

	text, isErr = del("payments")
	require.True(t, isErr)
	assert.Contains(t, text, "still in git")

	require.NoError(t, fake.Merge(context.Background(), pr.PullRequest))
	text, isErr = del("")
	require.False(t, isErr, text)
	assert.Contains(t, decode(t, text)["next"], "pending erase")

	// A refused read of the merged files names the step and the App's
	// permissions, erases nothing and asks for no new sign-in.
	fake.Fail[commit.OpMergedFiles] = &commit.AuthError{Op: commit.OpMergedFiles, Status: http.StatusForbidden}
	text, isErr = del("payments")
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "forbidden:"), text)
	assert.Contains(t, text, commit.OpMergedFiles)
	assert.Contains(t, text, "permissions")
	assert.Contains(t, text, "nothing was erased")
	assert.NotContains(t, text, "core_auth_login")
	assert.NotContains(t, text, "push")
	assert.NotNil(t, f.get(t, orgGVR, "", "payments"))

	delete(fake.Fail, commit.OpMergedFiles)
	text, isErr = del("payments")
	require.False(t, isErr, text)
	assert.Equal(t, true, decode(t, text)["deleted"])
	assert.Nil(t, f.get(t, orgGVR, "", "payments"))
}

// The recovery from a listing that keeps failing, such as a delete pull
// request of more than 3000 files, which GitHub never lists whole: the
// erase is refused and names the way forward, which is to restore the
// object's file alone and open the delete again. While the file is in git
// the earlier pull request's files are not needed, so a listing that fails
// does not stop the new delete pull request; the content path is gone
// already, so the new one changes the objects path only, and its files
// list.
func TestARestoredFileOpensANewDeleteWhenTheListingKeepsFailing(t *testing.T) {
	ctx := context.Background()
	f, fake := gitOrganization(t)
	file := fake.Files(mcRepo, "main")[paymentsFile]
	del := map[string]any{"name": "payments", "mode": "commit"}
	text, isErr := f.call(t, ToolDeleteOrganization, del)
	require.False(t, isErr, text)
	require.Len(t, fake.PullRequests(), 1)
	first := fake.PullRequests()[0]
	require.NoError(t, fake.Merge(ctx, first.PullRequest))
	fake.Fail[commit.OpMergedFiles] = errors.New("list merged files giantswarm/gazelle-config#1: GitHub listed 3000 of 3001 changed files (it lists at most 3000)")

	erase := map[string]any{"name": "payments", "mode": "commit", "confirm": "payments"}
	text, isErr = f.call(t, ToolDeleteOrganization, erase)
	require.True(t, isErr)
	assert.Contains(t, text, "nothing was erased")
	assert.Contains(t, text, "listed 3000 of 3001", "the cause is named")
	assert.Contains(t, text, "restore only "+paymentsFile)
	assert.NotNil(t, f.get(t, orgGVR, "", "payments"))

	require.NoError(t, fake.Commit(ctx, mcRepo, "main", "restore", map[string][]byte{paymentsFile: file}))
	before := fake.Files(mcRepo, "main")
	text, isErr = f.call(t, ToolDeleteOrganization, del)
	require.False(t, isErr, text)
	require.Len(t, fake.PullRequests(), 2)
	second := fake.PullRequests()[1]
	head := fake.Files(mcRepo, second.Head)
	assert.NotContains(t, head, paymentsFile)
	for p, c := range before {
		assert.False(t, strings.HasPrefix(p, paymentsPath+"/"), "the content path is gone already: %s", p)
		if string(head[p]) != string(c) {
			assert.True(t, strings.HasPrefix(p, objectsPath+"/"), "the new delete changes the objects path only, not %s", p)
		}
	}
	assert.Equal(t, second.URL, f.get(t, orgGVR, "", "payments").GetAnnotations()[organizations.DeletePullRequestAnnotation])

	// Its files list, and it erases.
	delete(fake.Fail, commit.OpMergedFiles)
	require.NoError(t, fake.Merge(ctx, second.PullRequest))
	text, isErr = f.call(t, ToolDeleteOrganization, erase)
	require.False(t, isErr, text)
	assert.Equal(t, true, decode(t, text)["deleted"])
}

// orgDocument is payments' object as a file carries it.
func orgDocument(t *testing.T) map[string]any {
	t.Helper()
	o := organization("payments", nil)
	delete(o.Object, "status")
	return o.Object
}

// Files under the objects path that do not define the organization do not
// stop the erase: another organization's file, another kind of the name,
// and files that do not parse, which Kustomize cannot apply.
func TestADeleteThatAddsOtherOrganizationsStillErases(t *testing.T) {
	ctx := context.Background()
	f, fake := gitOrganization(t)
	text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "mode": "commit"})
	require.False(t, isErr, text)
	pr := fake.PullRequests()[0]
	other := func(name string) []byte {
		o := organization(name, nil)
		delete(o.Object, "status")
		raw, err := yaml.Marshal(o.Object)
		require.NoError(t, err)
		return raw
	}
	require.NoError(t, fake.Commit(ctx, mcRepo, pr.Head, "add others", map[string][]byte{
		objectsPath + "/billing.yaml":        other("billing"),
		objectsPath + "/settings.yaml":       []byte("kind: ConfigMap\nmetadata:\n  name: payments\n---\n"),
		objectsPath + "/README.md":           []byte("# Organizations\n\nOne file per organization.\n"),
		objectsPath + "/archive/broken.yaml": append(other("payments"), []byte("---\nmetadata: [payments\n")...),
	}))
	merge := pr.PullRequest
	st, err := fake.Status(ctx, merge)
	require.NoError(t, err)
	merge.HeadSHA = st.HeadSHA
	require.NoError(t, fake.Merge(ctx, merge))

	text, isErr = f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "mode": "commit", "confirm": "payments"})
	require.False(t, isErr, text)
	assert.Equal(t, true, decode(t, text)["deleted"])
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
	live := f.get(t, orgGVR, "", "payments")
	ann := live.GetAnnotations()
	ann[organizations.DeletePullRequestAnnotation] = other.URL
	live.SetAnnotations(ann)
	_, err = f.dyn.Resource(orgGVR).Update(context.Background(), live, metav1.UpdateOptions{})
	require.NoError(t, err)

	text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "mode": "commit", "confirm": "payments"})
	require.True(t, isErr)
	assert.Contains(t, text, "did not remove it")
	assert.NotNil(t, f.get(t, orgGVR, "", "payments"))

	// The file gone from git by another merge, without agent-manager's
	// delete: Missing from git, never erased.
	g, gfake := gitOrganization(t)
	require.NoError(t, gfake.Commit(context.Background(), mcRepo, "main", "oops", map[string][]byte{paymentsFile: nil}))
	text, isErr = g.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "mode": "commit", "confirm": "payments"})
	require.True(t, isErr)
	assert.Contains(t, text, "Missing from git")
	assert.NotNil(t, g.get(t, orgGVR, "", "payments"))
}

// refusedErase is a case of TestAMergedDeleteThatDidNotRemoveTheFileErasesNothing.
type refusedErase struct {
	name string
	// edit changes the delete pull request's branch before the merge, and
	// receives the main branch's file content.
	edit  func(t *testing.T, fake *commit.Fake, head string, file []byte)
	after func(t *testing.T, fake *commit.Fake)
	want  []string
	// notWant must not be in the answer.
	notWant []string
	// noConfirm: the call without confirm is refused too, and does not
	// answer pending erase.
	noConfirm bool
}

// siblingOverlay is a delete pull request that moved the organization's
// file to base/<name>.yaml as the organization name, where overlays/prod, a
// sibling overlay that refers to it by ../, holds the files overlay. The
// answer names the overlay's kustomization and why.
func siblingOverlay(title, name string, overlay map[string]string, why string) refusedErase {
	return refusedErase{
		name: title,
		edit: func(t *testing.T, fake *commit.Fake, head string, _ []byte) {
			doc := orgDocument(t)
			require.NoError(t, unstructured.SetNestedField(doc, name, "metadata", "name"))
			org, err := yaml.Marshal(doc)
			require.NoError(t, err)
			files := map[string][]byte{
				objectsPath + "/base/" + name + ".yaml":  org,
				objectsPath + "/base/kustomization.yaml": []byte("resources:\n  - " + name + ".yaml\n"),
			}
			for f, raw := range overlay {
				files[objectsPath+"/overlays/prod/"+f] = []byte(raw)
			}
			require.NoError(t, fake.Commit(context.Background(), mcRepo, head, "move it", files))
		},
		want:      []string{"conflict:", objectsPath + "/base/" + name + ".yaml in", "may still define it", objectsPath + "/overlays/prod/kustomization.yaml (" + why, "Check the overlay first", "remove that setting or the overlay"},
		noConfirm: true,
	}
}

// leavingOverlay is a delete pull request that also wrote overlays/prod, a
// sibling overlay whose kustomization holds the files overlay. The
// organization is defined outside the objects path, in shared/orgs at the
// repository's root, which the scan does not read. The answer names the
// overlay's kustomization and the entry (giantswarm/agent-manager#437).
func leavingOverlay(title string, overlay map[string]string, entry string) refusedErase {
	return refusedErase{
		name: title,
		edit: func(t *testing.T, fake *commit.Fake, head string, file []byte) {
			files := map[string][]byte{
				// Changed, so that the move is not a rename.
				"shared/orgs/payments.yaml":      append([]byte("# shared\n"), file...),
				"shared/orgs/kustomization.yaml": []byte("resources:\n  - payments.yaml\n"),
			}
			for f, raw := range overlay {
				files[objectsPath+"/overlays/prod/"+f] = []byte(raw)
			}
			require.NoError(t, fake.Commit(context.Background(), mcRepo, head, "refer to it", files))
		},
		want:      []string{"conflict:", "loads files from outside it", objectsPath + "/overlays/prod/kustomization.yaml (" + entry + ")", "Bring what the entry loads under the objects path"},
		notWant:   []string{"still defines it"},
		noConfirm: true,
	}
}

// An overlay that cannot make a document the organization does not stop
// the erase: a namespace moves no document into it, since an Organization is
// cluster-scoped, and a name prefix or suffix makes no document of a name
// that is not a part of the organization's name into it; empty patch fields
// change nothing (giantswarm/agent-manager#423).
func TestAnOverlayThatCannotMakeTheOrganizationDoesNotStopTheErase(t *testing.T) {
	ctx := context.Background()
	org := func(t *testing.T, name string) []byte {
		doc := orgDocument(t)
		require.NoError(t, unstructured.SetNestedField(doc, name, "metadata", "name"))
		raw, err := yaml.Marshal(doc)
		require.NoError(t, err)
		return raw
	}
	for _, tc := range []struct {
		name  string
		files func(t *testing.T) map[string][]byte
	}{
		{
			name: "a sibling overlay sets a namespace on another organization",
			files: func(t *testing.T) map[string][]byte {
				return map[string][]byte{
					objectsPath + "/base/billing.yaml":                org(t, "billing"),
					objectsPath + "/base/kustomization.yaml":          []byte("resources:\n  - billing.yaml\n"),
					objectsPath + "/overlays/prod/kustomization.yaml": []byte("namespace: payments\nresources:\n  - ../../base\npatches: []\ntransformers: []\nreplacements: []\npatchesJson6902: []\npatchesStrategicMerge: []\n"),
				}
			},
		},
		{
			name: "a sibling overlay sets a name suffix on an organization whose name is not a part of the name",
			files: func(t *testing.T) map[string][]byte {
				return map[string][]byte{
					objectsPath + "/base/billing.yaml":                org(t, "billing"),
					objectsPath + "/overlays/prod/kustomization.yaml": []byte("nameSuffix: -prod\nnamePrefix: x-\nresources:\n  - ../../base/billing.yaml\n"),
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, fake := gitOrganization(t)
			text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "mode": "commit"})
			require.False(t, isErr, text)
			pr := fake.PullRequests()[0]
			require.NoError(t, fake.Merge(ctx, pr.PullRequest))
			require.NoError(t, fake.Commit(ctx, mcRepo, "main", "overlays", tc.files(t)))

			text, isErr = f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "mode": "commit"})
			require.False(t, isErr, text)
			assert.Contains(t, text, "pending erase:")
			text, isErr = f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "mode": "commit", "confirm": "payments"})
			require.False(t, isErr, text)
			assert.Equal(t, true, decode(t, text)["deleted"])
		})
	}
}

// A kustomization entry that stays under the objects path is followed as
// before and does not stop the erase by itself: a sibling overlay that
// refers to a directory, a file and a component by ../
// (giantswarm/agent-manager#437).
func TestAnEntryThatStaysUnderTheObjectsPathDoesNotStopTheErase(t *testing.T) {
	ctx := context.Background()
	doc := orgDocument(t)
	require.NoError(t, unstructured.SetNestedField(doc, "billing", "metadata", "name"))
	billing, err := yaml.Marshal(doc)
	require.NoError(t, err)
	f, fake := gitOrganization(t)
	text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "mode": "commit"})
	require.False(t, isErr, text)
	require.NoError(t, fake.Merge(ctx, fake.PullRequests()[0].PullRequest))
	require.NoError(t, fake.Commit(ctx, mcRepo, "main", "overlays", map[string][]byte{
		objectsPath + "/base/billing.yaml":                    billing,
		objectsPath + "/base/kustomization.yaml":              []byte("resources:\n  - billing.yaml\n"),
		objectsPath + "/components/labels/kustomization.yaml": []byte("apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\n"),
		objectsPath + "/overlays/prod/kustomization.yaml":     []byte("resources:\n  - ../../base\n  - ../../base/billing.yaml\n  - .\ncomponents:\n  - ../../components/labels\nbases:\n  - ../../base\n"),
	}))

	text, isErr = f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "mode": "commit"})
	require.False(t, isErr, text)
	assert.Contains(t, text, "pending erase:")
	text, isErr = f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "mode": "commit", "confirm": "payments"})
	require.False(t, isErr, text)
	assert.Equal(t, true, decode(t, text)["deleted"])
}

// The erase needs the merged delete pull request itself to have removed the
// object's file. A pull request edited before its merge erases nothing: one
// that kept the file, which another merge then removed, and one that moved
// it, so the organization is still in git. Nor does one whose files GitHub
// could not list.
func TestAMergedDeleteThatDidNotRemoveTheFileErasesNothing(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []refusedErase{
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
			// GitHub reports a move whose content changed much as one removed
			// file and one added file.
			name: "the file was moved and changed",
			edit: func(t *testing.T, fake *commit.Fake, head string, file []byte) {
				moved := append([]byte("# archived\n"), file...)
				require.NoError(t, fake.Commit(ctx, mcRepo, head, "move it", map[string][]byte{objectsPath + "/archive/payments.yaml": moved}))
			},
			want:      []string{"conflict:", objectsPath + "/archive/payments.yaml", "still defines it", "remove that definition", "move it back to " + paymentsFile},
			noConfirm: true,
		},
		{
			name: "the moved file was moved again after the merge",
			edit: func(t *testing.T, fake *commit.Fake, head string, file []byte) {
				moved := append([]byte("# archived\n"), file...)
				require.NoError(t, fake.Commit(ctx, mcRepo, head, "move it", map[string][]byte{objectsPath + "/archive/payments.yaml": moved}))
			},
			after: func(t *testing.T, fake *commit.Fake) {
				moved := fake.Files(mcRepo, "main")[objectsPath+"/archive/payments.yaml"]
				require.NoError(t, fake.Commit(ctx, mcRepo, "main", "move it again", map[string][]byte{objectsPath + "/archive/payments.yaml": nil, objectsPath + "/old/current": moved}))
			},
			want:      []string{"conflict:", objectsPath + "/old/current", "still defines it"},
			noConfirm: true,
		},
		{
			name: "the file was moved into a list",
			edit: func(t *testing.T, fake *commit.Fake, head string, _ []byte) {
				list, err := yaml.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{orgDocument(t)}})
				require.NoError(t, err)
				require.NoError(t, fake.Commit(ctx, mcRepo, head, "move it", map[string][]byte{objectsPath + "/archive/bundle.yaml": list}))
			},
			want:      []string{"conflict:", objectsPath + "/archive/bundle.yaml", "still defines it"},
			noConfirm: true,
		},
		{
			// A namespace does not change an Organization's identity: the
			// moved file still defines it.
			name: "the file was moved under a kustomization that sets a namespace",
			edit: func(t *testing.T, fake *commit.Fake, head string, _ []byte) {
				org, err := yaml.Marshal(orgDocument(t))
				require.NoError(t, err)
				require.NoError(t, fake.Commit(ctx, mcRepo, head, "move it", map[string][]byte{
					objectsPath + "/archive/payments.manifest":  org,
					objectsPath + "/archive/kustomization.yaml": []byte("namespace: org-acme\nresources:\n  - payments.manifest\n"),
				}))
			},
			want:      []string{"conflict:", objectsPath + "/archive/payments.manifest", "still defines it"},
			noConfirm: true,
		},
		{
			// Another kustomization may list a file of a kustomization's name
			// as a resource file.
			name: "the file was moved into a resource file named kustomization.yaml",
			edit: func(t *testing.T, fake *commit.Fake, head string, _ []byte) {
				org, err := yaml.Marshal(orgDocument(t))
				require.NoError(t, err)
				require.NoError(t, fake.Commit(ctx, mcRepo, head, "move it", map[string][]byte{objectsPath + "/archive/kustomization.yaml": org}))
			},
			want:      []string{"conflict:", objectsPath + "/archive/kustomization.yaml", "still defines it"},
			noConfirm: true,
		},
		{
			name: "a sibling overlay sets a name suffix of the moved file",
			edit: func(t *testing.T, fake *commit.Fake, head string, _ []byte) {
				doc := orgDocument(t)
				require.NoError(t, unstructured.SetNestedField(doc, "pay", "metadata", "name"))
				org, err := yaml.Marshal(doc)
				require.NoError(t, err)
				require.NoError(t, fake.Commit(ctx, mcRepo, head, "move it", map[string][]byte{
					objectsPath + "/base/pay.yaml":                    org,
					objectsPath + "/overlays/prod/kustomization.yaml": []byte("nameSuffix: ments\nresources:\n  - ../../base/pay.yaml\n"),
				}))
			},
			want:      []string{"conflict:", objectsPath + "/base/pay.yaml", "may still define it", objectsPath + "/overlays/prod/kustomization.yaml"},
			noConfirm: true,
		},
		siblingOverlay("a sibling overlay renames the file's organization by a patch", "legacy", map[string]string{
			"kustomization.yaml": "resources:\n  - ../../base\npatches:\n  - target:\n      kind: Organization\n    patch: |-\n      - op: replace\n        path: /metadata/name\n        value: payments\n",
		}, "patches"),
		siblingOverlay("a sibling overlay renames the file's organization by a JSON 6902 patch", "legacy", map[string]string{
			"kustomization.yaml": "resources:\n  - ../../base\npatchesJson6902:\n  - target:\n      group: " + orgGVR.Group + "\n      version: v1alpha1\n      kind: Organization\n      name: legacy\n    path: rename.yaml\n",
			"rename.yaml":        "- op: replace\n  path: /metadata/name\n  value: payments\n",
		}, "patchesJson6902"),
		siblingOverlay("a sibling overlay has a strategic merge patch", "legacy", map[string]string{
			"kustomization.yaml": "resources:\n  - ../../base\npatchesStrategicMerge:\n  - labels.yaml\n",
			"labels.yaml":        "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\n  labels:\n    tier: prod\n",
		}, "patchesStrategicMerge"),
		siblingOverlay("a sibling overlay renames the file's organization by a transformer", "legacy", map[string]string{
			"kustomization.yaml": "resources:\n  - ../../base\ntransformers:\n  - prefix.yaml\n",
			"prefix.yaml":        "apiVersion: builtin\nkind: PrefixSuffixTransformer\nmetadata:\n  name: rename\nprefix: pay\n",
		}, "transformers"),
		siblingOverlay("a sibling overlay has replacements", "legacy", map[string]string{
			"kustomization.yaml": "resources:\n  - ../../base\nreplacements:\n  - path: replace.yaml\n",
		}, "replacements"),
		siblingOverlay("a sibling overlay writes a patch field's key in another case", "legacy", map[string]string{
			"kustomization.yaml": "resources:\n  - ../../base\nPatches:\n  - path: move.yaml\n",
		}, "patches"),
		siblingOverlay("a sibling overlay sets a name suffix twice in two cases", "legacy", map[string]string{
			"kustomization.yaml": "nameSuffix: -a\nNameSuffix: -b\nresources:\n  - ../../base\n",
		}, "nameSuffix of a form the scan does not know"),
		siblingOverlay("a sibling overlay sets a name suffix of a form the scan does not know", "legacy", map[string]string{
			"kustomization.yaml": "nameSuffix: [a]\nresources:\n  - ../../base\n",
		}, "nameSuffix of a form the scan does not know"),
		siblingOverlay("a sibling overlay sets transformers of a form the scan does not know", "legacy", map[string]string{
			"kustomization.yaml": "transformers: prefix.yaml\nresources:\n  - ../../base\n",
		}, "transformers"),
		siblingOverlay("a sibling overlay does not parse", "legacy", map[string]string{
			"kustomization.yaml": "resources: [../../base\n",
		}, "it does not parse"),
		siblingOverlay("a sibling overlay substitutes the name by vars with configurations", "legacy", map[string]string{
			"kustomization.yaml": "resources:\n  - ../../base\nconfigurations:\n  - varreference.yaml\nvars:\n  - name: ORG\n    objref:\n      kind: ConfigMap\n      name: org\n      apiVersion: v1\n    fieldref:\n      fieldpath: data.name\n",
			"varreference.yaml":  "varReference:\n  - path: metadata/name\n    kind: Organization\n",
		}, "vars with configurations"),
		siblingOverlay("a sibling overlay writes the vars and configurations keys in another case", "legacy", map[string]string{
			"kustomization.yaml": "resources:\n  - ../../base\nConfigurations:\n  - varreference.yaml\nVars:\n  - name: ORG\n",
		}, "vars with configurations"),
		{
			name: "the file was moved to a document named by a Kustomize var",
			edit: func(t *testing.T, fake *commit.Fake, head string, _ []byte) {
				doc := orgDocument(t)
				require.NoError(t, unstructured.SetNestedField(doc, "$(ORG)", "metadata", "name"))
				org, err := yaml.Marshal(doc)
				require.NoError(t, err)
				require.NoError(t, fake.Commit(ctx, mcRepo, head, "move it", map[string][]byte{objectsPath + "/base/org.yaml": org}))
			},
			want:      []string{"conflict:", objectsPath + "/base/org.yaml", "still defines it"},
			noConfirm: true,
		},
		leavingOverlay("a sibling overlay loads resources from outside the objects path", map[string]string{
			"kustomization.yaml": "resources:\n  - ../../../../../shared/orgs\n",
		}, `resources entry "../../../../../shared/orgs" leaves the objects path`),
		leavingOverlay("a sibling overlay loads a remote resource", map[string]string{
			"kustomization.yaml": "resources:\n  - https://github.com/example/orgs//shared/orgs?ref=v1\n",
		}, `resources entry "https://github.com/example/orgs//shared/orgs?ref=v1" is a remote`),
		leavingOverlay("a sibling overlay loads a remote without a scheme", map[string]string{
			"kustomization.yaml": "resources:\n  - github.com/example/orgs//shared/orgs?ref=v1\n",
		}, `resources entry "github.com/example/orgs//shared/orgs?ref=v1" is a remote`),
		leavingOverlay("a sibling overlay loads a path that names nothing under the objects path", map[string]string{
			"kustomization.yaml": "resources:\n  - example.org/orgs/shared?ref=v1\n",
		}, `resources entry "example.org/orgs/shared?ref=v1" names nothing under the objects path, so Kustomize loads it as a remote or fails on it`),
		leavingOverlay("a sibling overlay loads a github.com remote that is also a local directory", map[string]string{
			"kustomization.yaml":                         "resources:\n  - github.com/example/orgs/shared\n",
			"github.com/example/orgs/shared/config.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\n",
		}, `resources entry "github.com/example/orgs/shared" is a remote`),
		leavingOverlay("a sibling overlay loads an SSH remote", map[string]string{
			"kustomization.yaml": "resources:\n  - git@github.com:example/orgs.git\n",
		}, `resources entry "git@github.com:example/orgs.git" is a remote`),
		leavingOverlay("a sibling overlay loads an absolute path", map[string]string{
			"kustomization.yaml": "resources:\n  - /shared/orgs\n",
		}, `resources entry "/shared/orgs" leaves the objects path`),
		leavingOverlay("a sibling overlay loads components from outside the objects path", map[string]string{
			"kustomization.yaml": "components:\n  - ../../../../../shared/orgs\n",
		}, `components entry "../../../../../shared/orgs" leaves the objects path`),
		leavingOverlay("a sibling overlay loads remote components", map[string]string{
			"kustomization.yaml": "components:\n  - https://github.com/example/orgs//shared/orgs?ref=v1\n",
		}, `components entry "https://github.com/example/orgs//shared/orgs?ref=v1" is a remote`),
		leavingOverlay("a sibling overlay loads bases from outside the objects path", map[string]string{
			"kustomization.yaml": "bases:\n  - ../../../../../shared/orgs\n",
		}, `bases entry "../../../../../shared/orgs" leaves the objects path`),
		leavingOverlay("a sibling overlay writes the resources key in another case", map[string]string{
			"kustomization.yaml": "Resources:\n  - ../../../../../shared/orgs\n",
		}, `resources entry "../../../../../shared/orgs" leaves the objects path`),
		leavingOverlay("a sibling overlay sets resources of a form the scan does not know", map[string]string{
			"kustomization.yaml": "resources: ../../../../../shared/orgs\n",
		}, "resources of a form the scan does not know"),
		{
			name: "the head of the branch cannot be read",
			after: func(t *testing.T, fake *commit.Fake) {
				fake.Fail[commit.OpHead] = errors.New("get ref heads/main: 404 Not Found")
			},
			want:      []string{"nothing was erased", "the head of main", "whether the organization is still defined in git is unknown"},
			noConfirm: true,
		},
		{
			name: "the objects path cannot be read",
			after: func(t *testing.T, fake *commit.Fake) {
				fake.Fail[commit.OpListFiles] = errors.New("the repository's tree is too large for one read")
			},
			want:      []string{"nothing was erased", "too large", "whether the organization is still defined in git is unknown"},
			noConfirm: true,
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
			text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "mode": "commit"})
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
				text, isErr = f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "mode": "commit"})
				require.True(t, isErr, text)
				assert.NotContains(t, text, "pending erase:")
			}

			text, isErr = f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "mode": "commit", "confirm": "payments"})
			require.True(t, isErr, text)
			for _, w := range tc.want {
				assert.Contains(t, text, w)
			}
			for _, w := range tc.notWant {
				assert.NotContains(t, text, w)
			}
			live := f.get(t, orgGVR, "", "payments")
			require.NotNil(t, live, "nothing is erased")
			assert.Equal(t, organizations.PruneDisabled, live.GetAnnotations()[organizations.PruneAnnotation], "the delete intent is not written")
		})
	}
}

// movingRemote is the fake forge where another writer, once armed, commits
// a move on the base branch right after the objects path is listed: between
// the scan's list and its reads.
type movingRemote struct {
	*commit.Fake
	move map[string][]byte
}

func (r *movingRemote) ListFiles(ctx context.Context, repo commit.Repository, ref, dir string) ([]string, error) {
	files, err := r.Fake.ListFiles(ctx, repo, ref, dir)
	if err == nil && dir == objectsPath && r.move != nil {
		move := r.move
		r.move = nil
		if err := r.Commit(ctx, repo, "main", "another writer moves the definition", move); err != nil {
			return nil, err
		}
	}
	return files, err
}

// The scan reads the objects path at one commit: a commit that moves the
// definition between the list and the reads does not hide it, with and
// without confirm (giantswarm/agent-manager#420).
func TestACommitDuringTheScanDoesNotHideTheDefinition(t *testing.T) {
	ctx := context.Background()
	archived := objectsPath + "/archive/payments.yaml"
	for _, confirm := range []string{"", "payments"} {
		t.Run("confirm "+confirm, func(t *testing.T) {
			remote := &movingRemote{}
			f, fake := gitOrganizationWith(t, func(fake *commit.Fake) organizations.GitHubRemote {
				remote.Fake = fake
				return remote
			})
			file := fake.Files(mcRepo, "main")[paymentsFile]
			text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "mode": "commit"})
			require.False(t, isErr, text)
			pr := fake.PullRequests()[0]
			// The delete pull request moved the file and changed it, which
			// GitHub reports as a removal and an addition.
			require.NoError(t, fake.Commit(ctx, mcRepo, pr.Head, "move it", map[string][]byte{archived: append([]byte("# archived\n"), file...)}))
			merge := pr.PullRequest
			st, err := fake.Status(ctx, merge)
			require.NoError(t, err)
			merge.HeadSHA = st.HeadSHA
			require.NoError(t, fake.Merge(ctx, merge))

			moved := fake.Files(mcRepo, "main")[archived]
			remote.move = map[string][]byte{archived: nil, objectsPath + "/old/current": moved}
			args := map[string]any{"name": "payments", "mode": "commit"}
			if confirm != "" {
				args["confirm"] = confirm
			}
			text, isErr = f.call(t, ToolDeleteOrganization, args)
			require.Nil(t, remote.move, "the other writer committed during the scan")
			require.NotContains(t, fake.Files(mcRepo, "main"), archived)
			require.True(t, isErr, text)
			assert.True(t, strings.HasPrefix(text, "conflict:"), text)
			assert.Contains(t, text, archived+" in "+mcRepo.String()+" still defines it", "the file as the commit the scan read has it")
			assert.NotContains(t, text, "pending erase:")
			live := f.get(t, orgGVR, "", "payments")
			require.NotNil(t, live, "nothing is erased")
			assert.Equal(t, organizations.PruneDisabled, live.GetAnnotations()[organizations.PruneAnnotation])
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
	text, isErr := f.call(t, ToolSetOrganizationModel, map[string]any{"organization": "payments", "name": "haiku", "provider": "Anthropic", "model": "claude-haiku", "default": true})
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "gitops_owned:"), text)
	assert.Contains(t, text, "mode commit")

	text, isErr = f.call(t, ToolSetOrganizationModel, map[string]any{"organization": "payments", "name": "haiku", "provider": "Anthropic", "model": "claude-haiku", "default": true, "mode": "commit"})
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

	// A member's binding is a file of the content path; the admins are not.
	text, isErr = f.call(t, ToolAddOrganizationMember, map[string]any{"organization": "payments", "role": "editor", "group": "payments-oncall", "mode": "commit"})
	require.False(t, isErr, text)
	require.Len(t, fake.PullRequests(), 2)
	editors := parseFile(t, fake.Files(mcRepo, fake.PullRequests()[1].Head)[paymentsPath+"/rolebinding-organization-editors.yaml"])
	assert.Len(t, editors.Object["subjects"], 2)

	text, isErr = f.call(t, ToolUpdateOrganization, map[string]any{"name": "payments", "workerReplicas": 3, "mode": "commit"})
	require.False(t, isErr, text)
	require.Len(t, fake.PullRequests(), 3)
	obj := parseFile(t, fake.Files(mcRepo, fake.PullRequests()[2].Head)[paymentsFile])
	replicas, _, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "capabilities", "agentPlatform", "workers", "replicas")
	assert.EqualValues(t, 3, replicas)
	assert.Equal(t, organizations.PruneDisabled, obj.GetAnnotations()[organizations.PruneAnnotation])
	assert.NotContains(t, obj.Object, "status")
}

// In commit mode an explicit workerReplicas 0 is written into a file that
// has no spec.workers.replicas: left unset, the operator would run its
// default of 1.
func TestUpdateOrganizationCommitsAnExplicitZeroWorkerReplicas(t *testing.T) {
	f, fake := gitOrganization(t)
	text, isErr := f.call(t, ToolUpdateOrganization, map[string]any{"name": "payments", "workerReplicas": 0, "mode": "commit"})
	require.False(t, isErr, text)
	res := decode(t, text)
	assert.Equal(t, []any{"workerReplicas"}, res["changed"])
	assert.EqualValues(t, 0, res["after"].(map[string]any)["workerReplicas"])
	require.Len(t, fake.PullRequests(), 1)
	obj := parseFile(t, fake.Files(mcRepo, fake.PullRequests()[0].Head)[paymentsFile])
	replicas, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "capabilities", "agentPlatform", "workers", "replicas")
	require.True(t, found, "an explicit 0 is written into the file")
	assert.EqualValues(t, 0, replicas)
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
		{ToolRemoveOrganizationMember, map[string]any{"group": "payments-team"}, "rolebinding-organization-editors.yaml", "remove"},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			f, fake := gitOrganization(t)
			seedSecret(t, f, "tenant-credential-gh", "token")
			args := map[string]any{"organization": "payments", "mode": "commit", "dryRun": true}
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

// The removal of the last object of an organization's content path keeps
// its kustomization.yaml, with an empty list: the organization's
// Kustomization builds that path, and would fail on one that does not exist.
func TestRemovingTheLastContentObjectKeepsTheContentPath(t *testing.T) {
	editors := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": "organization-editors"},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": organizations.RoleEditor},
		"subjects": []any{map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Group", "name": "payments-team"}},
	}}
	file, err := yaml.Marshal(editors.Object)
	require.NoError(t, err)
	live := editors.DeepCopy()
	live.SetNamespace(orgNS)
	live.SetLabels(fromGitLabels("agents-payments-x7k2q"))
	f, fake := commitFixture(t, map[string][]byte{
		paymentsPath + "/kustomization.yaml":                    []byte("resources:\n  - rolebinding-organization-editors.yaml\n"),
		paymentsPath + "/rolebinding-organization-editors.yaml": file,
	}, live)

	text, isErr := f.call(t, ToolRemoveOrganizationMember, map[string]any{"organization": "payments", "group": "payments-team", "mode": "commit"})
	require.False(t, isErr, text)
	require.Len(t, fake.PullRequests(), 1)
	head := fake.Files(mcRepo, fake.PullRequests()[0].Head)
	assert.NotContains(t, head, paymentsPath+"/rolebinding-organization-editors.yaml")
	assert.Equal(t, "resources: []\n", string(head[paymentsPath+"/kustomization.yaml"]))
}

// A .sops.yaml rule that makes a plain object's file secret is refused:
// gitops-commit would encrypt the file once and never update it.
func TestAPlainFileTheRepositoryWouldEncryptIsRefused(t *testing.T) {
	f, fake := commitFixture(t, map[string][]byte{".sops.yaml": sopsCovering(t, `^management-clusters/gazelle/agent-platform-organizations/.*`)})
	f.dropOrganization(t, "payments")
	text, isErr := f.call(t, ToolCreateOrganization, map[string]any{"name": "payments", "adminGroups": []any{"g"}, "mode": "commit"})
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "invalid"), text)
	assert.Contains(t, text, paymentsFile)
	assert.Empty(t, fake.PullRequests())
}

// A mark that fails after the delete pull request opened is written by the
// next call, from the open pull request; no second one is opened.
func TestAFailedDeleteMarkIsWrittenByTheNextCall(t *testing.T) {
	f, fake := gitOrganization(t)
	failed := false
	f.dyn.PrependReactor("update", "organizations", func(a k8stesting.Action) (bool, runtime.Object, error) {
		u := a.(k8stesting.UpdateActionImpl)
		obj := u.GetObject().(*unstructured.Unstructured)
		if len(u.UpdateOptions.DryRun) == 0 && obj.GetAnnotations()[organizations.DeletePullRequestAnnotation] != "" && !failed {
			failed = true
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "organizations"}, obj.GetName(), errors.New("the object has been modified"))
		}
		return false, nil, nil
	})
	args := map[string]any{"name": "payments", "mode": "commit"}
	text, isErr := f.call(t, ToolDeleteOrganization, args)
	require.True(t, isErr)
	assert.Contains(t, text, "call delete_organization again")
	require.Len(t, fake.PullRequests(), 1)
	assert.NotContains(t, f.get(t, orgGVR, "", "payments").GetAnnotations(), organizations.DeletePullRequestAnnotation)

	text, isErr = f.call(t, ToolDeleteOrganization, args)
	require.False(t, isErr, text)
	assert.Len(t, fake.PullRequests(), 1)
	assert.Equal(t, fake.PullRequests()[0].URL, f.get(t, orgGVR, "", "payments").GetAnnotations()[organizations.DeletePullRequestAnnotation])
}
