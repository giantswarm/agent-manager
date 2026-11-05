package api

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/kube/kubetest"
	"github.com/giantswarm/agent-manager/internal/organizations"
)

var (
	orgGVR     = organizations.GVR
	rbGVR      = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}
	connGVR    = schema.GroupVersionResource{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "remotemcpservers"}
	orgAgentGV = schema.GroupVersionResource{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "agents"}
)

// callerOnly is the caller provider of a downstream-OAuth installation: the
// caller's client exists only for a request that carries an identity.
type callerOnly struct{ c kube.Client }

func (p callerOnly) Client(ctx context.Context) (kube.Client, error) {
	if _, ok := identity.FromContext(ctx); !ok {
		return nil, kube.ErrNoCallerToken
	}
	return p.c, nil
}
func (callerOnly) Identity() string { return kube.IdentityCaller }
func (callerOnly) Remote(context.Context, []byte) (kube.Client, error) {
	return nil, errors.New("no remote clusters here")
}

// orgFixture is an MCP server whose organization tools act through the
// caller's fake clients, and find organization namespaces through the
// ServiceAccount's own fake, which holds nothing but namespaces.
type orgFixture struct {
	srv     *mcpserver.MCPServer
	dyn     *dynamicfake.FakeDynamicClient
	typed   *kubefake.Clientset
	dryRuns *kubetest.DryRuns
	// deny is what RBAC refuses the caller: a SelfSubjectAccessReview with
	// these attributes is answered not allowed, and a write of a matching
	// resource (dry run or not) is refused as Forbidden.
	deny func(ns, verb, resource, name string) bool
	// ready makes the operator report an organization Ready with its
	// namespace as soon as it is created.
	ready bool
	ctx   context.Context
}

const orgNS = "agents-payments-x7k2q"

func newOrgFixture(t *testing.T, objs ...runtime.Object) *orgFixture {
	t.Helper()
	f := &orgFixture{ready: true, deny: func(string, string, string, string) bool { return false }}
	f.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		orgGVR: "AgentPlatformOrganizationList", rbGVR: "RoleBindingList", mcGVR: "ModelConfigList", connGVR: "RemoteMCPServerList",
		egressGVR: "EgressPolicyList", orgAgentGV: "AgentList", hrGVR: "HelmReleaseList",
	}, objs...)
	f.dryRuns = kubetest.AnswerDryRuns(f.dyn)
	// What the caller's RBAC refuses, dry run or not, the API server refuses.
	for _, verb := range []string{"create", "update", "delete"} {
		f.dyn.PrependReactor(verb, "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
			name := ""
			if n, ok := a.(interface{ GetName() string }); ok {
				name = n.GetName()
			}
			if o, ok := a.(interface{ GetObject() runtime.Object }); ok {
				if u, ok := o.GetObject().(*unstructured.Unstructured); ok {
					name = u.GetName()
				}
			}
			if f.deny(a.GetNamespace(), verb, a.GetResource().Resource, name) {
				return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), name, errors.New("RBAC: access denied"))
			}
			return false, nil, nil
		})
	}
	// The operator: a created organization becomes Ready with its namespace.
	f.dyn.PrependReactor("create", "agentplatformorganizations", func(a k8stesting.Action) (bool, runtime.Object, error) {
		c := a.(k8stesting.CreateActionImpl)
		if f.ready && len(c.CreateOptions.DryRun) == 0 {
			u := c.GetObject().(*unstructured.Unstructured)
			_ = unstructured.SetNestedField(u.Object, map[string]any{
				"namespace":  orgNS,
				"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "Reconciled", "lastTransitionTime": "2026-11-04T00:00:00Z"}},
			}, "status")
		}
		return false, nil, nil
	})
	f.typed = kubefake.NewClientset()
	f.typed.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		review := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		ra := review.Spec.ResourceAttributes
		review.Status.Allowed = !f.deny(ra.Namespace, ra.Verb, ra.Resource, ra.Name)
		return true, review, nil
	})
	sa := kubefake.NewClientset(
		namespace(orgNS, map[string]string{apov1alpha1.OrganizationLabel: "payments", apov1alpha1.ParentLabel: "org-acme"}),
		namespace("agents-billing-a1b2c", map[string]string{apov1alpha1.OrganizationLabel: "billing", apov1alpha1.ParentLabel: "org-acme"}),
		namespace("org-legacy", map[string]string{"giantswarm.io/organization": "legacy"}),
		namespace("plain", nil),
	)
	orgs := organizations.New(callerOnly{c: kube.FromInterfaces(f.dyn, f.typed, f.typed.Discovery())}, organizations.Config{
		Namespaces:   organizations.NewNamespaces(sa, "kagent"),
		DevPortalURL: "https://portal.example.com/",
		ReadyTimeout: time.Second, ReadyPoll: 10 * time.Millisecond,
	}, nil)
	svc, _ := newService(t)
	f.srv = NewMCPServer(svc, "test", WithOrganizations(orgs))
	f.ctx = identity.ContextWith(context.Background(), &identity.Identity{Subject: "a", Email: "alice@example.com"})
	return f
}

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func (f *orgFixture) call(t *testing.T, tool string, args map[string]any) (string, bool) {
	t.Helper()
	return callToolCtx(t, f.ctx, f.srv, tool, args)
}

func (f *orgFixture) get(t *testing.T, gvr schema.GroupVersionResource, ns, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := f.dyn.Resource(gvr).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	return obj
}

func decode(t *testing.T, text string) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out), text)
	return out
}

// A create writes the object in its parent as the caller, waits for Ready,
// then writes the first admins there, a group first; nothing is written
// without the caller's identity.
func TestCreateOrganizationWritesTheObjectAndTheFirstAdminsAsTheCaller(t *testing.T) {
	f := newOrgFixture(t)
	args := map[string]any{"namespace": "org-acme", "name": "payments", "displayName": "Payments",
		"adminUsers": []any{"jane@example.com"}, "adminGroups": []any{"payments-leads"}, "harnesses": []any{"kagent"}}

	text, isErr := callTool(t, f.srv, ToolCreateOrganization, args)
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "unauthenticated:"), text)
	assert.Nil(t, f.get(t, orgGVR, "org-acme", "payments"), "nothing is written without a caller")

	text, isErr = f.call(t, ToolCreateOrganization, args)
	require.False(t, isErr, text)
	res := decode(t, text)
	assert.Equal(t, true, res["created"])
	assert.Equal(t, "alice@example.com", res["requestedBy"])

	org := f.get(t, orgGVR, "org-acme", "payments")
	require.NotNil(t, org)
	assert.Equal(t, "Payments", org.GetAnnotations()[apov1alpha1.DisplayNameAnnotation])
	binding := f.get(t, rbGVR, orgNS, organizations.FirstAdminsBinding)
	require.NotNil(t, binding)
	assert.Equal(t, organizations.RoleAdmin, binding.Object["roleRef"].(map[string]any)["name"])
	subjects := binding.Object["subjects"].([]any)
	require.Len(t, subjects, 2)
	assert.Equal(t, map[string]any{"kind": "Group", "name": "payments-leads", "apiGroup": "rbac.authorization.k8s.io"}, subjects[0])
	assert.Equal(t, "jane@example.com", subjects[1].(map[string]any)["name"])

	// Both writes were dry-run first.
	var dry []string
	for _, d := range f.dryRuns.Received() {
		dry = append(dry, d.Object.GetKind()+"/"+d.Object.GetName())
	}
	assert.Equal(t, []string{"AgentPlatformOrganization/payments", "RoleBinding/" + organizations.FirstAdminsBinding}, dry)

	text, isErr = f.call(t, ToolCreateOrganization, args)
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "conflict:"), text)
}

// An organization the operator has not made Ready yet keeps its first
// admins for a second call, which binds them once it is.
func TestCreateOrganizationFinishesTheFirstAdminsOnASecondCall(t *testing.T) {
	f := newOrgFixture(t)
	f.ready = false
	args := map[string]any{"namespace": "org-acme", "name": "payments", "adminGroups": []any{"payments-leads"}}
	text, isErr := f.call(t, ToolCreateOrganization, args)
	require.False(t, isErr, text)
	assert.Contains(t, decode(t, text)["pending"], "call create_organization again")
	assert.NotNil(t, f.get(t, orgGVR, "org-acme", "payments"))

	org := f.get(t, orgGVR, "org-acme", "payments")
	require.NoError(t, unstructured.SetNestedField(org.Object, map[string]any{"namespace": orgNS,
		"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "Reconciled", "lastTransitionTime": "2026-11-04T00:00:00Z"}}}, "status"))
	_, err := f.dyn.Resource(orgGVR).Namespace("org-acme").Update(context.Background(), org, metav1.UpdateOptions{})
	require.NoError(t, err)

	text, isErr = f.call(t, ToolCreateOrganization, args)
	require.False(t, isErr, text)
	assert.Equal(t, false, decode(t, text)["created"])
	assert.NotNil(t, f.get(t, rbGVR, orgNS, organizations.FirstAdminsBinding))
}

// A caller who may create the object but not bind the first admins is
// refused before the object exists, and the refusal names the role.
func TestCreateOrganizationRefusesACallerWithoutRightsBeforeAnyWrite(t *testing.T) {
	f := newOrgFixture(t)
	f.deny = func(_, verb, resource, _ string) bool {
		return (verb == "bind" && resource == "clusterroles") || resource == "rolebindings"
	}
	text, isErr := f.call(t, ToolCreateOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "adminGroups": []any{"payments-leads"}})
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "forbidden:"), text)
	assert.Contains(t, text, organizations.RoleOrganizationsAdmin)
	assert.Nil(t, f.get(t, orgGVR, "org-acme", "payments"), "no organization without its first admins")
}

// What the API server's dry run refuses is never written: neither the object
// it refuses nor an earlier write of the same call.
func TestTheDryRunRefusesBeforeAnyWrite(t *testing.T) {
	refuse := func(kind, name string) func(*unstructured.Unstructured) error {
		return func(u *unstructured.Unstructured) error {
			if u.GetKind() == kind && u.GetName() == name {
				return apierrors.NewForbidden(schema.GroupResource{Resource: kind}, name, errors.New("denied by policy"))
			}
			return nil
		}
	}

	t.Run("create_organization", func(t *testing.T) {
		f := newOrgFixture(t)
		f.dryRuns.Refuse = refuse("AgentPlatformOrganization", "payments")
		text, isErr := f.call(t, ToolCreateOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "adminGroups": []any{"g"}})
		require.True(t, isErr)
		assert.Contains(t, text, "dry run of create AgentPlatformOrganization org-acme/payments")
		assert.Contains(t, text, organizations.RoleOrganizationsAdmin)
		assert.Nil(t, f.get(t, orgGVR, "org-acme", "payments"))
	})

	t.Run("moving the default mark", func(t *testing.T) {
		f := newOrgFixture(t, model("claude", true), model("gpt", false))
		f.dryRuns.Refuse = refuse("ModelConfig", "gpt")
		text, isErr := f.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "gpt", "default": true})
		require.True(t, isErr)
		assert.Contains(t, text, organizations.RoleAdmin, "the refusal names the role")
		assert.True(t, marked(f.get(t, mcGVR, orgNS, "claude")), "the mark stays where it was")
		assert.False(t, marked(f.get(t, mcGVR, orgNS, "gpt")))
	})
}

func organization(parent, name string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apov1alpha1.GroupVersion.String(), "kind": "AgentPlatformOrganization",
		"metadata": map[string]any{"name": name, "namespace": parent},
		"spec":     map[string]any{"harnesses": []any{"kagent"}},
		"status": map[string]any{"namespace": orgNS, "snapshotPrefix": "s3://bucket/uid",
			"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "Reconciled", "lastTransitionTime": "2026-11-04T00:00:00Z"}}},
	}}
	u.SetLabels(labels)
	return u
}

// Update and delete work live; a delete answers what it erases until the
// person typed the organization's name, and marks Flux's delete intent
// before it deletes.
func TestUpdateAndDeleteOrganizationInApplyMode(t *testing.T) {
	f := newOrgFixture(t, organization("org-acme", "payments", nil), model("claude", true))

	text, isErr := f.call(t, ToolUpdateOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "harnesses": []any{"kagent", "claude"}, "workerReplicas": 2})
	require.False(t, isErr, text)
	assert.ElementsMatch(t, []any{"harnesses", "workerReplicas"}, decode(t, text)["changed"])
	spec := f.get(t, orgGVR, "org-acme", "payments").Object["spec"].(map[string]any)
	assert.Equal(t, []any{"kagent", "claude"}, spec["harnesses"])
	assert.EqualValues(t, 2, spec["workers"].(map[string]any)["replicas"])

	text, isErr = f.call(t, ToolDeleteOrganization, map[string]any{"namespace": "org-acme", "name": "payments"})
	require.False(t, isErr, text)
	res := decode(t, text)
	assert.Equal(t, false, res["deleted"])
	assert.Equal(t, []any{"claude"}, res["erases"].(map[string]any)["models"])
	assert.Equal(t, []any{"tenant-credential-claude"}, res["erases"].(map[string]any)["credentials"])
	assert.NotNil(t, f.get(t, orgGVR, "org-acme", "payments"))

	f.dyn.ClearActions()
	text, isErr = f.call(t, ToolDeleteOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "confirm": "org-acme/payments"})
	require.False(t, isErr, text)
	assert.Equal(t, true, decode(t, text)["deleted"])
	assert.Nil(t, f.get(t, orgGVR, "org-acme", "payments"))
	var live []string
	for _, a := range f.dyn.Actions() {
		switch a := a.(type) {
		case k8stesting.UpdateActionImpl:
			if len(a.UpdateOptions.DryRun) == 0 {
				live = append(live, "update prune="+a.GetObject().(*unstructured.Unstructured).GetAnnotations()[organizations.PruneAnnotation])
			}
		case k8stesting.DeleteActionImpl:
			live = append(live, "delete")
		}
	}
	assert.Equal(t, []string{"update prune=enabled", "delete"}, live)
}

// An organization Flux applies from git is not changed live.
func TestOrganizationFromGitIsNotWrittenLive(t *testing.T) {
	f := newOrgFixture(t, organization("org-acme", "payments", map[string]string{"kustomize.toolkit.fluxcd.io/name": "flux"}))
	text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "confirm": "org-acme/payments"})
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "gitops_owned:"), text)
	assert.NotNil(t, f.get(t, orgGVR, "org-acme", "payments"))
}

func model(name string, isDefault bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3", "kind": "ModelConfig",
		"metadata": map[string]any{"name": name, "namespace": orgNS},
		"spec":     map[string]any{"provider": "Anthropic", "model": "m", "apiKeySecret": "tenant-credential-" + name, "apiKeySecretKey": "apiKey"},
	}}
	if isDefault {
		u.SetAnnotations(map[string]string{apov1alpha1.DefaultModelConfigAnnotation: "true"})
	}
	return u
}

func marked(u *unstructured.Unstructured) bool {
	return u != nil && u.GetAnnotations()[apov1alpha1.DefaultModelConfigAnnotation] == "true"
}

// The first model of an organization is its default; a later one is not;
// default true moves the mark; a namespace with two marks takes no new
// model until one is chosen.
func TestTheDefaultModelMark(t *testing.T) {
	f := newOrgFixture(t)
	add := func(name string, extra map[string]any) (string, bool) {
		args := map[string]any{"namespace": orgNS, "name": name, "provider": "Anthropic", "model": "claude-sonnet-4-6"}
		for k, v := range extra {
			args[k] = v
		}
		return f.call(t, ToolSetOrganizationModel, args)
	}
	text, isErr := add("claude", nil)
	require.False(t, isErr, text)
	assert.Equal(t, "claude", decode(t, text)["defaultModel"])
	text, isErr = add("haiku", nil)
	require.False(t, isErr, text)
	assert.Equal(t, "claude", decode(t, text)["defaultModel"])
	assert.True(t, marked(f.get(t, mcGVR, orgNS, "claude")))
	assert.False(t, marked(f.get(t, mcGVR, orgNS, "haiku")))

	text, isErr = f.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "haiku", "default": true})
	require.False(t, isErr, text)
	assert.Equal(t, []any{"claude"}, decode(t, text)["unmarked"])
	assert.False(t, marked(f.get(t, mcGVR, orgNS, "claude")))
	assert.True(t, marked(f.get(t, mcGVR, orgNS, "haiku")))

	g := newOrgFixture(t, model("a", true), model("b", true))
	text, isErr = g.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "c", "provider": "Anthropic", "model": "m"})
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "conflict:"), text)
	assert.Contains(t, text, "a, b")
	assert.Nil(t, g.get(t, mcGVR, orgNS, "c"))
}

// No organization tool takes a secret value; a missing credential is
// answered with the Dev Portal's form link, and a present one never crosses
// back.
func TestCredentialsAreReferencesOnly(t *testing.T) {
	f := newOrgFixture(t)
	listReq, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	out, _ := json.Marshal(f.srv.HandleMessage(context.Background(), listReq))
	var listed struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(out, &listed))
	valueLike := regexp.MustCompile(`(?i)(value|password|token|apikey|plaintext)`)
	var walk func(tool, path string, schema map[string]any)
	walk = func(tool, path string, schema map[string]any) {
		props, _ := schema["properties"].(map[string]any)
		for name, p := range props {
			assert.False(t, valueLike.MatchString(name), "%s takes %s%s, a secret value", tool, path, name)
			if sub, ok := p.(map[string]any); ok {
				walk(tool, path+name+".", sub)
				if items, ok := sub["items"].(map[string]any); ok {
					walk(tool, path+name+"[].", items)
				}
			}
		}
	}
	seen := 0
	for _, tool := range listed.Result.Tools {
		if ok, _ := filepath.Match(OrganizationToolPattern, "x_agent-manager_"+tool.Name); ok {
			seen++
			walk(tool.Name, "", tool.InputSchema)
		}
	}
	assert.Equal(t, len(OrganizationToolNames()), seen)

	text, isErr := f.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "claude", "provider": "Anthropic", "model": "claude-sonnet-4-6"})
	require.False(t, isErr, text)
	cred := decode(t, text)["credentials"].([]any)[0].(map[string]any)
	assert.Equal(t, organizations.CredentialMissing, cred["state"])
	link := "https://portal.example.com/agent-platform/organizations/org-acme/payments/credentials/tenant-credential-claude/apiKey/ModelConfig/claude"
	assert.Equal(t, link, cred["formUrl"])
	assert.Contains(t, cred["next"], link)

	_, err := f.typed.CoreV1().Secrets(orgNS).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-credential-gh", Namespace: orgNS},
		Data:       map[string][]byte{"token": []byte("ghp-live-value")},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	text, isErr = f.call(t, ToolSetOrganizationConnector, map[string]any{"namespace": orgNS, "name": "gh", "url": "https://mcp.github.example/mcp", "credentialHeader": "Authorization"})
	require.False(t, isErr, text)
	assert.NotContains(t, text, "ghp-live-value")
	cred = decode(t, text)["credentials"].([]any)[0].(map[string]any)
	assert.Equal(t, organizations.CredentialPresent, cred["state"])
	assert.NotContains(t, cred, "next")
	headers, _, _ := unstructured.NestedSlice(f.get(t, connGVR, orgNS, "gh").Object, "spec", "headersFrom")
	assert.Equal(t, []any{map[string]any{"name": "Authorization", "valueFrom": map[string]any{"type": "Secret", "name": "tenant-credential-gh", "key": "token"}}}, headers)

	text, isErr = f.call(t, ToolSetOrganizationEgress, map[string]any{"namespace": orgNS, "name": "github",
		"destinations": []any{map[string]any{"origin": "https://api.github.com", "credentialHeader": "Authorization", "credentialPrefix": "Bearer ", "credentialSecret": "tenant-credential-gh"}}}) // #nosec G101 -- a Secret's name, not a credential
	require.False(t, isErr, text)
	assert.NotContains(t, text, "ghp-live-value")
}

// The picker lists the organizations whose Agents the caller may list,
// asked as the caller, never a legacy tenant or a plain namespace.
func TestThePickerListsOnlyTheCallersOrganizations(t *testing.T) {
	f := newOrgFixture(t)
	f.deny = func(ns, verb, resource, _ string) bool {
		return verb == "list" && resource == "agents" && ns != orgNS
	}
	text, isErr := f.call(t, ToolListMyOrganizations, nil)
	require.False(t, isErr, text)
	assert.Equal(t, []any{map[string]any{"namespace": orgNS, "organization": "payments", "parent": "org-acme"}}, decode(t, text)["organizations"])

	f.deny = func(string, string, string, string) bool { return true }
	text, isErr = f.call(t, ToolListMyOrganizations, nil)
	require.False(t, isErr, text)
	res := decode(t, text)
	assert.Empty(t, res["organizations"])
	assert.Contains(t, res["none"], "add_organization_member")
}

// Members are added into their role's binding and removed from it; the first
// admins' binding is never emptied, and the refusal says how to go on.
func TestOrganizationMembers(t *testing.T) {
	f := newOrgFixture(t)
	_, err := f.dyn.Resource(rbGVR).Namespace(orgNS).Create(context.Background(), &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": organizations.FirstAdminsBinding, "namespace": orgNS},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": organizations.RoleAdmin},
		"subjects": []any{map[string]any{"kind": "User", "name": "jane@example.com", "apiGroup": "rbac.authorization.k8s.io"}},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)

	text, isErr := f.call(t, ToolAddOrganizationMember, map[string]any{"namespace": orgNS, "role": "editor", "group": "payments-team"})
	require.False(t, isErr, text)
	require.NotNil(t, f.get(t, rbGVR, orgNS, "organization-editors"))

	text, isErr = f.call(t, ToolListOrganizationMembers, map[string]any{"namespace": orgNS})
	require.False(t, isErr, text)
	members := decode(t, text)["members"].([]any)
	require.Len(t, members, 2)
	assert.Equal(t, "Group", members[0].(map[string]any)["kind"], "groups first")

	text, isErr = f.call(t, ToolRemoveOrganizationMember, map[string]any{"namespace": orgNS, "user": "jane@example.com"})
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "conflict:"), text)
	assert.Contains(t, text, "add another admin first")

	text, isErr = f.call(t, ToolRemoveOrganizationMember, map[string]any{"namespace": orgNS, "group": "payments-team"})
	require.False(t, isErr, text)
	assert.Nil(t, f.get(t, rbGVR, orgNS, "organization-editors"), "an emptied binding goes")

	text, isErr = f.call(t, ToolAddOrganizationMember, map[string]any{"namespace": "org-legacy", "role": "user", "group": "g"})
	require.True(t, isErr)
	assert.Contains(t, text, "legacy tenant")
}
