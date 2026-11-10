package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/giantswarm/agent-platform-operator/pkg/adminmirror"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/agents"
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
	// user is who the API server authenticates the caller as (a
	// SelfSubjectReview).
	user authenticationv1.UserInfo
	ctx  context.Context
	orgs *organizations.Service
	// cfg and provider build the tools again, with another logger.
	cfg      organizations.Config
	provider kube.Provider
}

const orgNS = "agents-payments-x7k2q"

func newOrgFixture(t *testing.T, objs ...runtime.Object) *orgFixture {
	t.Helper()
	return newOrgFixtureWith(t, nil, objs...)
}

// newOrgFixtureWith is newOrgFixture with the tools' configuration changed
// by configure.
func newOrgFixtureWith(t *testing.T, configure func(*organizations.Config), objs ...runtime.Object) *orgFixture {
	t.Helper()
	f := &orgFixture{ready: true, deny: func(string, string, string, string) bool { return false },
		user: authenticationv1.UserInfo{Username: "alice@example.com"}}
	f.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		orgGVR: "AgentPlatformOrganizationList", rbGVR: "RoleBindingList", mcGVR: "ModelConfigList", connGVR: "RemoteMCPServerList",
		egressGVR: "EgressPolicyList", orgAgentGV: "AgentList", hrGVR: "HelmReleaseList",
	}, objs...)
	f.dryRuns = kubetest.AnswerDryRuns(f.dyn)
	answerCredentialResourceVersions(t, f.dyn)
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
	f.typed.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.SelfSubjectReview{Status: authenticationv1.SelfSubjectReviewStatus{UserInfo: f.user}}, nil
	})
	sa := kubefake.NewClientset(
		namespace(orgNS, map[string]string{apov1alpha1.OrganizationLabel: "payments", apov1alpha1.ParentLabel: "org-acme"}),
		namespace("agents-billing-a1b2c", map[string]string{apov1alpha1.OrganizationLabel: "billing", apov1alpha1.ParentLabel: "org-acme"}),
		// The parent of the organizations: a Giant Swarm Organization's
		// namespace, with the old label alone.
		namespace("org-acme", map[string]string{"giantswarm.io/organization": "acme"}),
		namespace("plain", nil),
	)
	cfg := organizations.Config{ // #nosec G101 -- synthetic UI URL and isolated fake store
		Namespaces:           organizations.NewNamespaces(sa, "kagent"),
		CredentialUIURL:      "https://portal.example.com/credentials?operation={operation}",
		CredentialOperations: organizations.NewCredentialOperations(operationMaps(t)),
		ReadyTimeout:         time.Second, ReadyPoll: 10 * time.Millisecond,
	}
	if configure != nil {
		configure(&cfg)
	}
	f.cfg, f.provider = cfg, callerOnly{c: kube.FromInterfaces(f.dyn, f.typed, f.typed.Discovery())}
	orgs := organizations.New(f.provider, cfg, nil)
	f.orgs = orgs
	svc, _ := newService(t)
	f.srv = NewMCPServer(svc, "test", WithOrganizations(orgs), WithLifecycle(t.Context()))
	f.ctx = identity.ContextWith(t.Context(), &identity.Identity{Issuer: "https://identity.example.com", Client: "platform", Subject: "a", Email: "alice@example.com"})
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
			if len(a.DeleteOptions.DryRun) == 0 {
				live = append(live, "delete")
			} else {
				live = append(live, "dry run of delete")
			}
		}
	}
	assert.Equal(t, []string{"update prune=enabled", "dry run of delete", "delete"}, live)
}

// A delete the API server refuses after the delete intent is written
// leaves the organization as it was, without the intent.
func TestARefusedDeleteTakesTheDeleteIntentOffAgain(t *testing.T) {
	f := newOrgFixture(t, organization("org-acme", "payments", nil))
	f.dryRuns.Refuse = func(u *unstructured.Unstructured) error {
		if u.GetKind() == "agentplatformorganizations" {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "agentplatformorganizations"}, u.GetName(), errors.New("denied by policy"))
		}
		return nil
	}
	text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "confirm": "org-acme/payments"})
	require.True(t, isErr)
	assert.Contains(t, text, "dry run of delete")
	org := f.get(t, orgGVR, "org-acme", "payments")
	require.NotNil(t, org)
	assert.NotContains(t, org.GetAnnotations(), organizations.PruneAnnotation)
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
	seedSecret(t, f, "tenant-credential-claude", "apiKey")
	seedSecret(t, f, "tenant-credential-haiku", "apiKey")
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
	assert.Contains(t, cred["formUrl"], "https://portal.example.com/credentials?operation=")
	assert.Contains(t, cred["next"], cred["formUrl"])

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
// asked as the caller: never an org-<name> labelled giantswarm.io/organization
// alone, nor a plain namespace, even where the caller may list Agents.
func TestThePickerListsOnlyTheCallersOrganizations(t *testing.T) {
	f := newOrgFixture(t)
	f.deny = func(ns, verb, resource, _ string) bool {
		// The caller may list Agents in org-acme and plain too: only the
		// organization label keeps them out.
		return verb == "list" && resource == "agents" && ns == "agents-billing-a1b2c"
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

	// A refused token is not "no organization".
	f.typed.PrependReactor("create", "selfsubjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewUnauthorized("token expired")
	})
	text, isErr = f.call(t, ToolListMyOrganizations, nil)
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "unauthenticated:"), text)
	assert.NotContains(t, text, "agents-", "a refused token learns no organization namespace")
}

// Members are added into their role's binding and removed from it; when
// admission keeps the first admins' last admin, the refusal says how to go
// on.
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

	f.dryRuns.Refuse = func(u *unstructured.Unstructured) error {
		if u.GetKind() == "rolebindings" && u.GetName() == organizations.FirstAdminsBinding {
			return apierrors.NewForbidden(schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "rolebindings"}, u.GetName(), errors.New("the first admins' binding is kept while the namespace lives"))
		}
		return nil
	}
	text, isErr = f.call(t, ToolRemoveOrganizationMember, map[string]any{"namespace": orgNS, "user": "jane@example.com"})
	require.True(t, isErr)
	assert.NotNil(t, f.get(t, rbGVR, orgNS, organizations.FirstAdminsBinding))
	assert.True(t, strings.HasPrefix(text, "forbidden:"), text)
	assert.Contains(t, text, "add another admin first")

	text, isErr = f.call(t, ToolRemoveOrganizationMember, map[string]any{"namespace": orgNS, "group": "payments-team"})
	require.False(t, isErr, text)
	assert.Nil(t, f.get(t, rbGVR, orgNS, "organization-editors"), "an emptied binding goes")

	text, isErr = f.call(t, ToolAddOrganizationMember, map[string]any{"namespace": "org-acme", "role": "user", "group": "g"})
	require.True(t, isErr)
	assert.Contains(t, text, "namespace org-acme is not an organization namespace")
	assert.NotContains(t, text, "tenant", "an org-* namespace is no tenant of any kind")
}

// A creator whose rights have not reached the new namespace by Ready (the
// operator has not mirrored them yet) gets the organization without a
// Forbidden write, and an answer naming who can bind the first admins; a
// second call finishes it.
func TestCreateOrganizationNamesWhoBindsTheFirstAdminsWhenTheCreatorCannot(t *testing.T) {
	f := newOrgFixture(t)
	f.deny = func(ns, verb, resource, _ string) bool {
		return ns == orgNS && (resource == "rolebindings" || verb == "bind")
	}
	args := map[string]any{"namespace": "org-acme", "name": "payments", "adminGroups": []any{"payments-leads"}}
	text, isErr := f.call(t, ToolCreateOrganization, args)
	require.False(t, isErr, text)
	pending := decode(t, text)["pending"].(string)
	assert.Contains(t, pending, "first admins are not bound")
	assert.Contains(t, pending, organizations.RoleOrganizationsAdmin)
	assert.NotNil(t, f.get(t, orgGVR, "org-acme", "payments"))
	assert.Nil(t, f.get(t, rbGVR, orgNS, organizations.FirstAdminsBinding))
	for _, d := range f.dryRuns.Received() {
		assert.NotEqual(t, "RoleBinding", d.Object.GetKind(), "no write is tried that RBAC refuses")
	}

	f.deny = func(string, string, string, string) bool { return false }
	text, isErr = f.call(t, ToolCreateOrganization, args)
	require.False(t, isErr, text)
	assert.NotNil(t, f.get(t, rbGVR, orgNS, organizations.FirstAdminsBinding))
}

// An organization being erased is not "not Ready yet".
func TestCreateOrganizationRefusesOneBeingErased(t *testing.T) {
	org := organization("org-acme", "payments", nil)
	now := metav1.Now()
	org.SetDeletionTimestamp(&now)
	org.SetFinalizers([]string{apov1alpha1.Finalizer})
	f := newOrgFixture(t, org)
	start := time.Now()
	text, isErr := f.call(t, ToolCreateOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "adminGroups": []any{"g"}})
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "conflict:"), text)
	assert.Contains(t, text, "being erased")
	assert.Less(t, time.Since(start), time.Second, "no wait for a Ready that never comes")
}

// A delete the API server refuses after its dry run passed takes the delete
// intent off again: the admission guard keeps protecting the object.
func TestAFailedDeleteTakesTheDeleteIntentOffAgain(t *testing.T) {
	f := newOrgFixture(t, organization("org-acme", "payments", nil))
	f.dyn.PrependReactor("delete", "agentplatformorganizations", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if len(a.(k8stesting.DeleteActionImpl).DeleteOptions.DryRun) == 0 {
			return true, nil, apierrors.NewServiceUnavailable("webhook unavailable")
		}
		return false, nil, nil
	})
	text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "confirm": "org-acme/payments"})
	require.True(t, isErr, text)
	org := f.get(t, orgGVR, "org-acme", "payments")
	require.NotNil(t, org)
	assert.NotContains(t, org.GetAnnotations(), organizations.PruneAnnotation)
}

// Moving the default mark never leaves an organization without one: when a
// write after the first fails, a model still carries the mark.
func TestMovingTheDefaultMarkNeverLeavesNone(t *testing.T) {
	f := newOrgFixture(t, model("claude", true), model("gpt", false))
	seedSecret(t, f, "tenant-credential-gpt", "apiKey")
	writes := 0
	f.dyn.PrependReactor("update", "modelconfigs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if len(a.(k8stesting.UpdateActionImpl).UpdateOptions.DryRun) > 0 {
			return false, nil, nil
		}
		if writes++; writes == 2 {
			return true, nil, apierrors.NewServiceUnavailable("etcd timeout")
		}
		return false, nil, nil
	})
	_, isErr := f.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "gpt", "default": true})
	require.True(t, isErr)
	assert.True(t, marked(f.get(t, mcGVR, orgNS, "claude")) || marked(f.get(t, mcGVR, orgNS, "gpt")), "a model keeps the mark")
}

// A connector's URL never carries a credential into or out of a tool call,
// and setting one credential header keeps the others.
func TestConnectorURLsAndHeaders(t *testing.T) {
	conn := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3", "kind": "RemoteMCPServer",
		"metadata": map[string]any{"name": "jira", "namespace": orgNS},
		// #nosec G101 -- a fake credential in a URL, the case under test
		"spec": map[string]any{"description": "jira", "url": "https://bot:hunter2@jira.example/mcp?api_key=k-live&site=acme",
			"headersFrom": []any{
				map[string]any{"name": "Authorization", "valueFrom": map[string]any{"type": "Secret", "name": "tenant-credential-jira", "key": "token"}},
				map[string]any{"name": "X-Atlassian-Token", "valueFrom": map[string]any{"type": "Secret", "name": "tenant-credential-jira", "key": "xsrf"}},
			}},
	}}
	// Written elsewhere, with a query that does not parse (a ; separator)
	// and a fragment: neither may hide a credential from a read.
	// #nosec G101 -- a fake credential in a URL, the case under test
	malformed := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3", "kind": "RemoteMCPServer",
		"metadata": map[string]any{"name": "malformed", "namespace": orgNS},
		"spec":     map[string]any{"description": "m", "url": "https://mcp.example/mcp?access_token=q-live;extra#token=f-live"},
	}}
	f := newOrgFixture(t, conn, malformed)
	seedSecret(t, f, "tenant-credential-jira", "token")
	seedSecret(t, f, "tenant-credential-jira-v2", "token")
	_, err := f.typed.CoreV1().Secrets(orgNS).Update(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tenant-credential-jira", Namespace: orgNS}, Data: map[string][]byte{"token": []byte(liveValue), "xsrf": []byte(liveValue)}}, metav1.UpdateOptions{})
	require.NoError(t, err)

	for _, url := range []string{
		"https://bot:hunter2@mcp.example/mcp",
		"https://mcp.example/mcp?access_token=t-live",
		// url.URL.Query drops the malformed pair, and the credential with it.
		"https://mcp.example/mcp?access_token=t-live;extra",
		"https://mcp.example/mcp#access_token=t-live",
		// Does not parse: the error must not carry the userinfo back.
		"https://bot:hunter2@mcp.example/%zz",
	} {
		text, isErr := f.call(t, ToolSetOrganizationConnector, map[string]any{"namespace": orgNS, "name": "x", "url": url})
		require.True(t, isErr, url)
		assert.True(t, strings.HasPrefix(text, "invalid_request:"), text)
		assert.NotContains(t, text, "hunter2", url)
		assert.NotContains(t, text, "t-live", url)
		assert.Nil(t, f.get(t, connGVR, orgNS, "x"))
	}

	text, isErr := f.call(t, ToolListOrganizationConnector, map[string]any{"namespace": orgNS})
	require.False(t, isErr, text)
	for _, secret := range []string{"hunter2", "k-live", "q-live", "f-live"} {
		assert.NotContains(t, text, secret)
	}
	assert.Contains(t, text, "site=acme")

	text, isErr = f.call(t, ToolSetOrganizationConnector, map[string]any{"namespace": orgNS, "name": "jira", "credentialHeader": "Authorization", "credentialSecret": "tenant-credential-jira-v2"}) // #nosec G101 -- a Secret's name, not a credential
	require.False(t, isErr, text)
	headers, _, _ := unstructured.NestedSlice(f.get(t, connGVR, orgNS, "jira").Object, "spec", "headersFrom")
	assert.ElementsMatch(t, []any{
		map[string]any{"name": "Authorization", "valueFrom": map[string]any{"type": "Secret", "name": "tenant-credential-jira-v2", "key": "token"}},
		map[string]any{"name": "X-Atlassian-Token", "valueFrom": map[string]any{"type": "Secret", "name": "tenant-credential-jira", "key": "xsrf"}},
	}, headers)
}

// list_organizations lists a parent's organizations, or every parent's, as
// the caller; a refusal names the role.
func TestListOrganizations(t *testing.T) {
	other := organization("org-other", "billing", nil)
	f := newOrgFixture(t, organization("org-acme", "payments", nil), other)
	names := func(text string) []string {
		var out []string
		for _, o := range decode(t, text)["organizations"].([]any) {
			out = append(out, o.(map[string]any)["parent"].(string)+"/"+o.(map[string]any)["name"].(string))
		}
		return out
	}
	text, isErr := f.call(t, ToolListOrganizations, map[string]any{"namespace": "org-acme"})
	require.False(t, isErr, text)
	assert.Equal(t, []string{"org-acme/payments"}, names(text))
	text, isErr = f.call(t, ToolListOrganizations, nil)
	require.False(t, isErr, text)
	assert.Equal(t, []string{"org-acme/payments", "org-other/billing"}, names(text))

	f.dyn.PrependReactor("list", "agentplatformorganizations", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(orgGVR.GroupResource(), "", errors.New("RBAC: access denied"))
	})
	text, isErr = f.call(t, ToolListOrganizations, map[string]any{"namespace": "org-acme"})
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "forbidden:"), text)
	assert.Contains(t, text, organizations.RoleOrganizationsAdmin)
}

// get_organization answers what a delete erases as far as the caller can
// read it, and names what it cannot read instead of failing.
func TestGetOrganizationNamesWhatItCannotRead(t *testing.T) {
	f := newOrgFixture(t, organization("org-acme", "payments", nil), model("claude", true))
	f.dyn.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == orgNS && (a.GetResource().Resource == "agents" || a.GetResource().Resource == "rolebindings") {
			return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("RBAC: access denied"))
		}
		return false, nil, nil
	})
	text, isErr := f.call(t, ToolGetOrganization, map[string]any{"namespace": "org-acme", "name": "payments"})
	require.False(t, isErr, text)
	erases := decode(t, text)["erases"].(map[string]any)
	assert.Equal(t, []any{"claude"}, erases["models"])
	assert.Equal(t, []any{"tenant-credential-claude"}, erases["credentials"])
	unreadable := erases["unreadable"].([]any)
	require.Len(t, unreadable, 2)
	assert.Contains(t, unreadable[0], "agents:")
	assert.Contains(t, unreadable[1], "members:")
	assert.Contains(t, erases["kept"], "session tombstones")
}

// What a Flux Kustomization applies from git is not changed live: Flux
// would revert it.
func TestGitOwnedObjectsAreNotWrittenLive(t *testing.T) {
	fromGit := map[string]string{"kustomize.toolkit.fluxcd.io/name": "org-payments"}
	gitModel := model("claude", true)
	gitModel.SetLabels(fromGit)
	gitEditors := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": "organization-editors", "namespace": orgNS, "labels": map[string]any{"kustomize.toolkit.fluxcd.io/name": "org-payments"}},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": organizations.RoleEditor},
		"subjects": []any{map[string]any{"kind": "Group", "name": "payments-team", "apiGroup": "rbac.authorization.k8s.io"}},
	}}
	gitOrg := organization("org-acme", "payments", fromGit)
	for name, call := range map[string]struct {
		tool string
		args map[string]any
	}{
		"change a model":        {ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "claude", "model": "other"}},
		"move the mark off one": {ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "haiku", "provider": "Anthropic", "model": "m", "default": true}},
		"delete a model":        {ToolDeleteOrganizationObject, map[string]any{"namespace": orgNS, "kind": "model", "name": "claude"}},
		"add to a git binding":  {ToolAddOrganizationMember, map[string]any{"namespace": orgNS, "role": "editor", "user": "jane@example.com"}},
		"remove from one":       {ToolRemoveOrganizationMember, map[string]any{"namespace": orgNS, "group": "payments-team"}},
		"bind the first admins": {ToolCreateOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "adminGroups": []any{"g"}}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newOrgFixture(t, gitModel.DeepCopy(), gitEditors.DeepCopy(), gitOrg.DeepCopy())
			text, isErr := f.call(t, call.tool, call.args)
			require.True(t, isErr, text)
			assert.True(t, strings.HasPrefix(text, "gitops_owned:"), text)
			assert.Contains(t, text, "org-payments")
			for _, a := range f.dyn.Actions() {
				switch a.GetVerb() {
				case "create", "update", "delete":
					t.Errorf("a %s %s was sent", a.GetVerb(), a.GetResource().Resource)
				}
			}
		})
	}
}

// delete_organization_object deletes as the caller and says when the
// organization is left without a default model; a curated copy stays.
func TestDeleteOrganizationObject(t *testing.T) {
	curated := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "api.kagent.dev/v1alpha3", "kind": "RemoteMCPServer",
		"metadata": map[string]any{"name": "muster", "namespace": orgNS, "labels": map[string]any{agents.OperatorObjectLabel: "curated"}},
		"spec":     map[string]any{"description": "muster", "url": "https://muster.example/mcp"},
	}}
	f := newOrgFixture(t, model("claude", true), curated)
	text, isErr := f.call(t, ToolDeleteOrganizationObject, map[string]any{"namespace": orgNS, "kind": "model", "name": "claude"})
	require.False(t, isErr, text)
	assert.Contains(t, decode(t, text)["note"], "has none now")
	assert.Nil(t, f.get(t, mcGVR, orgNS, "claude"))

	text, isErr = f.call(t, ToolDeleteOrganizationObject, map[string]any{"namespace": orgNS, "kind": "connector", "name": "muster"})
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "conflict:"), text)
	assert.NotNil(t, f.get(t, connGVR, orgNS, "muster"))
}

// parentBinding writes rb into the parent org-acme, as its admins did before
// the create.
func (f *orgFixture) parentBinding(t *testing.T, rb rbacv1.RoleBinding) {
	t.Helper()
	rb.Namespace = "org-acme"
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&rb)
	require.NoError(t, err)
	u := &unstructured.Unstructured{Object: obj}
	u.SetAPIVersion("rbac.authorization.k8s.io/v1")
	u.SetKind("RoleBinding")
	_, err = f.dyn.Resource(rbGVR).Namespace("org-acme").Create(t.Context(), u, metav1.CreateOptions{})
	require.NoError(t, err)
}

// callerNamed is who the API server authenticates as the subject s of a
// RoleBinding.
func callerNamed(s rbacv1.Subject) authenticationv1.UserInfo {
	switch s.Kind {
	case rbacv1.GroupKind:
		return authenticationv1.UserInfo{Username: "carol@example.com", Groups: []string{s.Name}}
	case rbacv1.ServiceAccountKind:
		return authenticationv1.UserInfo{Username: "system:serviceaccount:" + s.Namespace + ":" + s.Name}
	}
	return authenticationv1.UserInfo{Username: s.Name}
}

// A caller without a right cluster-wide whose right in the parent comes from
// a binding agent-platform-operator does not mirror for them is refused
// before anything is written, dry run or not; one the operator mirrors goes
// on and binds the first admins. The bindings and their mirrored subjects are
// the operator's own table, so the two cannot drift.
func TestCreateOrganizationRefusesBeforeTheWriteWhenTheParentRightIsNotMirrored(t *testing.T) {
	for _, c := range adminmirror.Cases() {
		for _, subject := range c.Binding.Subjects {
			mirrored := slices.ContainsFunc(c.Mirrored, func(m rbacv1.Subject) bool { return m.Kind == subject.Kind && m.Name == subject.Name })
			for _, dryRun := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s %s/dryRun=%t", c.Binding.Name, subject.Kind, path.Join(subject.Namespace, subject.Name), dryRun), func(t *testing.T) {
					f := newOrgFixture(t)
					f.deny = func(ns, _, _, _ string) bool { return ns == "" }
					f.user = callerNamed(subject)
					f.parentBinding(t, c.Binding)
					text, isErr := f.call(t, ToolCreateOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "adminGroups": []any{"payments-leads"}, "dryRun": dryRun})

					if mirrored {
						require.False(t, isErr, text)
						assert.Equal(t, true, decode(t, text)["created"], text)
						assert.Equal(t, !dryRun, f.get(t, rbGVR, orgNS, organizations.FirstAdminsBinding) != nil, "the first admins are bound unless it is a dry run")
						return
					}
					require.True(t, isErr, text)
					assert.True(t, strings.HasPrefix(text, "forbidden:"), text)
					assert.Contains(t, text, c.Binding.Name+" (", "the refusal names the binding")
					if strings.HasPrefix(f.user.Username, "system:") {
						assert.Contains(t, text, "a cluster-admin creates the organization, or binds the right to you cluster-wide", "a system: name is never mirrored")
					} else {
						assert.Contains(t, text, "binds "+organizations.RoleOrganizationsAdmin+" to you", "the first way out")
						assert.Contains(t, text, "or a cluster-admin creates the organization", "the second way out")
					}
					assert.Nil(t, f.get(t, orgGVR, "org-acme", "payments"))
					assert.Empty(t, f.dryRuns.Received(), "not even a dry run is sent")
				})
			}
		}
	}
}

// The check beyond the operator's table: a caller whose bindings cannot be
// read goes on, and the access review in the organization namespace after
// Ready answers pending when the right has not reached it; a caller with only
// half the right cluster-wide, and no parent binding, is refused.
func TestCreateOrganizationGoesOnUnreadCallersAndRefusesHalfARight(t *testing.T) {
	forbidden := func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("RBAC: access denied"))
	}
	for _, tc := range []struct {
		name    string
		deny    func(ns, verb, resource, name string) bool
		prepare func(f *orgFixture)
		refused string
	}{{
		name:    "may not list the parent's RoleBindings",
		deny:    func(ns, _, _, _ string) bool { return ns == "" || ns == orgNS },
		prepare: func(f *orgFixture) { f.dyn.PrependReactor("list", "rolebindings", forbidden) },
	}, {
		name:    "may not ask who it is",
		deny:    func(ns, _, _, _ string) bool { return ns == "" || ns == orgNS },
		prepare: func(f *orgFixture) { f.typed.PrependReactor("create", "selfsubjectreviews", forbidden) },
	}, {
		name:    "may create RoleBindings cluster-wide but not bind the role",
		deny:    func(ns, verb, _, _ string) bool { return ns == "" && verb == "bind" },
		prepare: func(*orgFixture) {},
		refused: "No RoleBinding in org-acme that agent-platform-operator mirrors names you",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrgFixture(t)
			f.deny = tc.deny
			tc.prepare(f)
			text, isErr := f.call(t, ToolCreateOrganization, map[string]any{"namespace": "org-acme", "name": "payments", "adminGroups": []any{"payments-leads"}})
			if tc.refused != "" {
				require.True(t, isErr, text)
				assert.True(t, strings.HasPrefix(text, "forbidden:"), text)
				assert.Contains(t, text, tc.refused)
				assert.Nil(t, f.get(t, orgGVR, "org-acme", "payments"))
				return
			}
			require.False(t, isErr, text)
			assert.Contains(t, decode(t, text)["pending"], "first admins are not bound")
			assert.NotNil(t, f.get(t, orgGVR, "org-acme", "payments"))
		})
	}
}
