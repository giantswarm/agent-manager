package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	orgv1alpha1 "github.com/giantswarm/organization-operator/api/v1alpha1"
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

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/kube/kubetest"
	"github.com/giantswarm/agent-manager/internal/organizations"
	"github.com/giantswarm/agent-manager/internal/tags/tagstest"
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
	srv   *mcpserver.MCPServer
	dyn   *dynamicfake.FakeDynamicClient
	typed *kubefake.Clientset
	// sa is the ServiceAccount's own client, which finds the organization
	// namespaces.
	sa      *kubefake.Clientset
	dryRuns *kubetest.DryRuns
	// deny is what RBAC refuses the caller: a SelfSubjectAccessReview with
	// these attributes is answered not allowed, and a write of a matching
	// resource (dry run or not) is refused as Forbidden.
	deny func(ns, verb, resource, name string) bool
	// ready makes the operator report an organization Ready with its
	// namespace as soon as it is created.
	ready bool
	ctx   context.Context
	orgs  *organizations.Service
	// logs is what the organizations service logged.
	logs *logBuffer
	// cfg and provider build the tools again, with another logger.
	cfg      organizations.Config
	provider kube.Provider
}

const orgNS = "agents-payments-x7k2q"

func newOrgFixture(t *testing.T, objs ...runtime.Object) *orgFixture {
	t.Helper()
	return newOrgFixtureWith(t, nil, objs...)
}

// secretReads are the verbs that return a Secret's data: an organization
// admin holds none of them.
var secretReads = []string{"get", "list", "watch", "patch"}

func refuseSecretRead(a k8stesting.Action) (bool, runtime.Object, error) {
	return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", fmt.Errorf("RBAC: an organization admin may not %s Secrets", a.GetVerb()))
}

// newOrgFixtureWith is newOrgFixture with the tools' configuration changed
// by configure.
func newOrgFixtureWith(t *testing.T, configure func(*organizations.Config), objs ...runtime.Object) *orgFixture {
	t.Helper()
	f := &orgFixture{ready: true, deny: func(string, string, string, string) bool { return false }}
	f.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		orgGVR: "OrganizationList", rbGVR: "RoleBindingList", mcGVR: "ModelConfigList", connGVR: "RemoteMCPServerList",
		egressGVR: "EgressPolicyList", orgAgentGV: "AgentList", hrGVR: "HelmReleaseList", ociGVR: "OCIRepositoryList",
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
	f.dyn.PrependReactor("create", "organizations", func(a k8stesting.Action) (bool, runtime.Object, error) {
		c := a.(k8stesting.CreateActionImpl)
		if f.ready && len(c.CreateOptions.DryRun) == 0 {
			u := c.GetObject().(*unstructured.Unstructured)
			_ = unstructured.SetNestedField(u.Object, readyStatus(orgNS), "status")
		}
		return false, nil, nil
	})
	f.typed = kubefake.NewClientset()
	// Credentials are write-only: no read of a Secret, typed or dynamic, is
	// answered, as for an organization admin, who holds no get, list, watch
	// or patch on Secrets.
	for _, verb := range secretReads {
		f.dyn.PrependReactor(verb, "secrets", refuseSecretRead)
		f.typed.PrependReactor(verb, "secrets", refuseSecretRead)
	}
	f.typed.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		review := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		ra := review.Spec.ResourceAttributes
		review.Status.Allowed = !f.deny(ra.Namespace, ra.Verb, ra.Resource, ra.Name)
		return true, review, nil
	})
	sa := kubefake.NewClientset(
		namespace(orgNS, map[string]string{apov1alpha1.OrganizationLabel: "payments", apov1alpha1.OrganizationUIDLabel: "uid-payments"}),
		namespace("agents-billing-a1b2c", map[string]string{apov1alpha1.OrganizationLabel: "billing"}),
		// A Giant Swarm Organization's namespace, with its own label alone:
		// no organization namespace.
		namespace("org-acme", map[string]string{"giantswarm.io/organization": "acme"}),
		namespace("plain", nil),
	)
	f.sa = sa
	cfg := organizations.Config{ // #nosec G101 -- synthetic UI URL and isolated fake store
		Namespaces:           organizations.NewNamespaces(sa, f.dyn, "kagent"),
		CredentialUIURL:      "https://portal.example.com/credentials?operation={operation}",
		CredentialOperations: organizations.NewCredentialOperations(operationMaps(t)),
	}
	if configure != nil {
		configure(&cfg)
	}
	f.cfg, f.provider = cfg, callerOnly{c: kube.FromInterfaces(f.dyn, f.typed, f.typed.Discovery())}
	f.logs = &logBuffer{}
	orgs := organizations.New(f.provider, cfg, slog.New(slog.NewJSONHandler(f.logs, nil)))
	f.orgs = orgs
	svc, _ := newService(t)
	f.srv = NewMCPServer(svc, "test", WithOrganizations(orgs), WithLifecycle(t.Context()))
	f.ctx = identity.ContextWith(t.Context(), &identity.Identity{Issuer: "https://identity.example.com", Client: "platform", Subject: "a", Email: "alice@example.com"})
	return f
}

type logBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.String()
}

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func (f *orgFixture) call(t *testing.T, tool string, args map[string]any) (string, bool) {
	t.Helper()
	return callToolCtx(t, f.ctx, f.srv, tool, args)
}

// get reads the fake API server's stored object, past the reactors that
// answer the caller: what a test asserts, not what the tools may read.
func (f *orgFixture) get(t *testing.T, gvr schema.GroupVersionResource, ns, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := f.dyn.Tracker().Get(gvr, ns, name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	u, ok := obj.(*unstructured.Unstructured)
	require.True(t, ok, "%T", obj)
	return u
}

func decode(t *testing.T, text string) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out), text)
	return out
}

// A create writes one Organization with the Agent Platform on and its admins
// in spec.admins, as the caller, a group first, and no RoleBinding: the
// operator binds the admins. Nothing is written without the caller's
// identity.
func TestCreateOrganizationWritesTheObjectWithItsAdminsAsTheCaller(t *testing.T) {
	f := newOrgFixture(t)
	args := map[string]any{"name": "payments", "displayName": "Payments",
		"adminUsers": []any{"jane@example.com"}, "adminGroups": []any{"payments-leads"}, "harnesses": []any{"kagent"}}

	text, isErr := callTool(t, f.srv, ToolCreateOrganization, args)
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "unauthenticated:"), text)
	assert.Nil(t, f.get(t, orgGVR, "", "payments"), "nothing is written without a caller")

	text, isErr = f.call(t, ToolCreateOrganization, args)
	require.False(t, isErr, text)
	res := decode(t, text)
	assert.Equal(t, true, res["created"])
	assert.Equal(t, "alice@example.com", res["requestedBy"])
	assert.Contains(t, res["next"], "binds spec.admins")

	org := f.get(t, orgGVR, "", "payments")
	require.NotNil(t, org)
	assert.Equal(t, "Payments", org.GetAnnotations()[apov1alpha1.DisplayNameAnnotation])
	assert.Equal(t, map[string]any{"enabled": true, "harnesses": []any{"kagent"}}, org.Object["spec"].(map[string]any)["agentPlatform"],
		"an omitted workerReplicas writes no field: the CRD's default of 1 applies")
	assert.Equal(t, []any{
		map[string]any{"kind": "Group", "name": "payments-leads"},
		map[string]any{"kind": "User", "name": "jane@example.com"},
	}, org.Object["spec"].(map[string]any)["admins"])

	// The Organization alone is written, dry run first: no RoleBinding.
	var dry []string
	for _, d := range f.dryRuns.Received() {
		dry = append(dry, d.Object.GetKind()+"/"+d.Object.GetName())
	}
	assert.Equal(t, []string{"Organization/payments"}, dry)
	for _, a := range f.dyn.Actions() {
		assert.NotEqual(t, "rolebindings", a.GetResource().Resource, "%s of a RoleBinding", a.GetVerb())
	}

	text, isErr = f.call(t, ToolCreateOrganization, args)
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "conflict:"), text)
}

// A created organization is listed at once, and with its namespace once the
// operator reports it in status.agentPlatform.namespace.
func TestACreatedOrganizationIsListedWithItsNamespaceOnceReported(t *testing.T) {
	f := newOrgFixture(t)
	f.ready = false
	text, isErr := f.call(t, ToolCreateOrganization, map[string]any{"name": "payments", "adminGroups": []any{"payments-leads"}})
	require.False(t, isErr, text)

	listed := func() map[string]any {
		text, isErr := f.call(t, ToolListOrganizations, nil)
		require.False(t, isErr, text)
		orgs := decode(t, text)["organizations"].([]any)
		require.Len(t, orgs, 1, text)
		return orgs[0].(map[string]any)
	}
	before := listed()
	assert.NotContains(t, before, "namespace")
	assert.Equal(t, map[string]any{"manage": true, "member": false, "createAgents": false}, before["permissions"])

	org := f.get(t, orgGVR, "", "payments")
	require.NoError(t, unstructured.SetNestedField(org.Object, readyStatus(orgNS), "status"))
	_, err := f.dyn.Resource(orgGVR).Update(context.Background(), org, metav1.UpdateOptions{})
	require.NoError(t, err)

	after := listed()
	assert.Equal(t, orgNS, after["namespace"])
	assert.Equal(t, map[string]any{"manage": true, "member": true, "createAgents": true}, after["permissions"])
}

// A caller who may not create Organizations is refused before anything is
// written, and the refusal names the role.
func TestCreateOrganizationRefusesACallerWithoutRightsBeforeAnyWrite(t *testing.T) {
	f := newOrgFixture(t)
	f.deny = func(_, verb, resource, _ string) bool { return verb == "create" && resource == "organizations" }
	text, isErr := f.call(t, ToolCreateOrganization, map[string]any{"name": "payments", "adminGroups": []any{"payments-leads"}})
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "forbidden:"), text)
	assert.Contains(t, text, organizations.RoleOrganizationsAdmin)
	assert.Nil(t, f.get(t, orgGVR, "", "payments"))
	assert.Empty(t, f.dryRuns.Received(), "not even a dry run is sent")
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
		f.dryRuns.Refuse = refuse("Organization", "payments")
		text, isErr := f.call(t, ToolCreateOrganization, map[string]any{"name": "payments", "adminGroups": []any{"g"}})
		require.True(t, isErr)
		assert.Contains(t, text, "dry run of create Organization payments")
		assert.Contains(t, text, organizations.RoleOrganizationsAdmin)
		assert.Nil(t, f.get(t, orgGVR, "", "payments"))
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

// readyStatus is what agent-platform-operator reports of an organization
// whose namespace ns is Ready.
func readyStatus(ns string) map[string]any {
	return map[string]any{
		"agentPlatform": map[string]any{"namespace": ns, "snapshotPrefix": "s3://bucket/uid"},
		"conditions":    []any{map[string]any{"type": "Ready", "status": "True", "reason": "Reconciled", "lastTransitionTime": "2026-11-04T00:00:00Z"}},
	}
}

// organization is an Organization with the Agent Platform enabled, Ready
// in orgNS, of UID uid-<name>: the UID the organization namespaces' label
// names.
func organization(name string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": orgv1alpha1.GroupVersion.String(), "kind": "Organization",
		"metadata": map[string]any{"name": name, "uid": "uid-" + name},
		"spec": map[string]any{
			"admins":        []any{map[string]any{"kind": "Group", "name": "payments-leads"}, map[string]any{"kind": "User", "name": "jane@example.com"}},
			"agentPlatform": map[string]any{"enabled": true, "harnesses": []any{"kagent"}},
		},
		"status": readyStatus(orgNS),
	}}
	u.SetLabels(labels)
	return u
}

// kaasOrganization is an Organization without the Agent Platform: no
// organization of the tools.
func kaasOrganization(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": orgv1alpha1.GroupVersion.String(), "kind": "Organization",
		"metadata": map[string]any{"name": name},
		"status":   map[string]any{"namespace": "org-" + name},
	}}
}

// Update and delete work live; a delete answers what it erases until the
// person typed the organization's name, and marks Flux's delete intent
// before it deletes.
func TestUpdateAndDeleteOrganizationInApplyMode(t *testing.T) {
	f := newOrgFixture(t, organization("payments", nil), model("claude", true))

	text, isErr := f.call(t, ToolUpdateOrganization, map[string]any{"name": "payments", "harnesses": []any{"kagent", "claude"}, "workerReplicas": 2})
	require.False(t, isErr, text)
	assert.ElementsMatch(t, []any{"harnesses", "workerReplicas"}, decode(t, text)["changed"])
	spec := f.get(t, orgGVR, "", "payments").Object["spec"].(map[string]any)["agentPlatform"].(map[string]any)
	assert.Equal(t, []any{"kagent", "claude"}, spec["harnesses"])
	assert.EqualValues(t, 2, spec["workers"].(map[string]any)["replicas"])

	text, isErr = f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments"})
	require.False(t, isErr, text)
	res := decode(t, text)
	assert.Equal(t, false, res["deleted"])
	assert.Equal(t, []any{"claude"}, res["erases"].(map[string]any)["models"])
	assert.Equal(t, []any{"tenant-credential-claude"}, res["erases"].(map[string]any)["credentials"])
	assert.Contains(t, res["next"], `confirm "payments"`)
	assert.NotNil(t, f.get(t, orgGVR, "", "payments"))

	f.dyn.ClearActions()
	text, isErr = f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "confirm": "payments"})
	require.False(t, isErr, text)
	assert.Equal(t, true, decode(t, text)["deleted"])
	assert.Nil(t, f.get(t, orgGVR, "", "payments"))
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

// Where organization-operator runs, it deletes its namespace of the
// Organization (status.namespace) with it: the delete's answer names that
// namespace, also before the Agent Platform's namespace exists.
func TestDeleteOrganizationNamesOrganizationOperatorsNamespace(t *testing.T) {
	for _, tc := range []struct {
		name        string
		agentsNS    bool
		wantModels  bool
		wantErasure string
	}{
		{"with the Agent Platform namespace", true, true, "agents-payments-x7k2q"},
		{"before the Agent Platform namespace", false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			org := organization("payments", nil)
			require.NoError(t, unstructured.SetNestedField(org.Object, "org-payments", "status", "namespace"))
			if !tc.agentsNS {
				unstructured.RemoveNestedField(org.Object, "status", "agentPlatform", "namespace")
			}
			f := newOrgFixture(t, org, model("claude", true))
			text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments"})
			require.False(t, isErr, text)
			erases := decode(t, text)["erases"].(map[string]any)
			assert.Equal(t, "org-payments", erases["organizationNamespace"])
			assert.Contains(t, erases["kept"], "organization-operator deletes the namespace org-payments")
			if tc.wantModels {
				assert.Equal(t, tc.wantErasure, erases["namespace"])
				assert.Equal(t, []any{"claude"}, erases["models"])
			} else {
				assert.Empty(t, erases["namespace"])
			}
			assert.NotNil(t, f.get(t, orgGVR, "", "payments"), "nothing is deleted without confirm")
		})
	}
}

// update_organization replaces the admin groups, or the admin users, of
// spec.admins and keeps the other kind; an empty list removes them.
func TestUpdateOrganizationChangesItsAdmins(t *testing.T) {
	f := newOrgFixture(t, organization("payments", nil))
	admins := func() any { return f.get(t, orgGVR, "", "payments").Object["spec"].(map[string]any)["admins"] }

	text, isErr := f.call(t, ToolUpdateOrganization, map[string]any{"name": "payments", "adminGroups": []any{"payments-owners", "platform"}})
	require.False(t, isErr, text)
	res := decode(t, text)
	assert.Equal(t, []any{"admins"}, res["changed"])
	assert.Equal(t, []any{
		map[string]any{"kind": "Group", "name": "payments-owners"},
		map[string]any{"kind": "Group", "name": "platform"},
		map[string]any{"kind": "User", "name": "jane@example.com"},
	}, admins(), "the users stay")
	assert.Len(t, res["after"].(map[string]any)["admins"], 3)

	text, isErr = f.call(t, ToolUpdateOrganization, map[string]any{"name": "payments", "adminGroups": []any{"payments-owners", "platform"}})
	require.False(t, isErr, text)
	assert.Empty(t, decode(t, text)["changed"], "the same admins are no change")

	text, isErr = f.call(t, ToolUpdateOrganization, map[string]any{"name": "payments", "adminGroups": []any{}, "adminUsers": []any{}})
	require.False(t, isErr, text)
	assert.Nil(t, admins(), "no admins: the platform team manages it")
}

// A delete the API server refuses after the delete intent is written
// leaves the organization as it was, without the intent.
func TestARefusedDeleteTakesTheDeleteIntentOffAgain(t *testing.T) {
	f := newOrgFixture(t, organization("payments", nil))
	f.dryRuns.Refuse = func(u *unstructured.Unstructured) error {
		if u.GetKind() == "organizations" {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "organizations"}, u.GetName(), errors.New("denied by policy"))
		}
		return nil
	}
	text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "confirm": "payments"})
	require.True(t, isErr)
	assert.Contains(t, text, "dry run of delete")
	org := f.get(t, orgGVR, "", "payments")
	require.NotNil(t, org)
	assert.NotContains(t, org.GetAnnotations(), organizations.PruneAnnotation)
}

// An organization Flux applies from git is not changed live.
func TestOrganizationFromGitIsNotWrittenLive(t *testing.T) {
	f := newOrgFixture(t, organization("payments", map[string]string{"kustomize.toolkit.fluxcd.io/name": "flux"}))
	text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "confirm": "payments"})
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "gitops_owned:"), text)
	assert.NotNil(t, f.get(t, orgGVR, "", "payments"))
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
	assert.NotContains(t, decode(t, text), "note", "one create alone")
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

// rivalCreate makes the first live create of a ModelConfig race another
// first model: the rival, marked as the default, lands just before it.
func rivalCreate(t *testing.T, f *orgFixture, rival string) {
	t.Helper()
	raced := false
	f.dyn.PrependReactor("create", "modelconfigs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if raced || len(a.(k8stesting.CreateActionImpl).CreateOptions.DryRun) > 0 {
			return false, nil, nil
		}
		raced = true
		require.NoError(t, f.dyn.Tracker().Create(mcGVR, model(rival, true), orgNS))
		return false, nil, nil
	})
}

// Two first models created at the same time both take the default mark; the
// answer reads the models again and names the two marks and the fix.
func TestTwoConcurrentFirstModelsNameTheDoubleMark(t *testing.T) {
	f := newOrgFixture(t)
	seedSecret(t, f, "tenant-credential-claude", "apiKey")
	rivalCreate(t, f, "gpt")
	text, isErr := f.call(t, ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "claude", "provider": "Anthropic", "model": "claude-sonnet-4-6"})
	require.False(t, isErr, text)
	out := decode(t, text)
	assert.Contains(t, out["note"], "2 models marked as its default (claude, gpt)")
	assert.Contains(t, out["note"], "set_organization_model with default true on one of them")
	assert.NotContains(t, out, "defaultModel")
	assert.True(t, marked(f.get(t, mcGVR, orgNS, "claude")))
	assert.True(t, marked(f.get(t, mcGVR, orgNS, "gpt")))
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
	// A number of model tokens, kagent's maxTokens: no credential.
	notASecret := map[string]bool{ToolSetOrganizationModel + "." + argMaxTokens: true}
	var walk func(tool, path string, schema map[string]any)
	walk = func(tool, path string, schema map[string]any) {
		props, _ := schema["properties"].(map[string]any)
		for name, p := range props {
			if notASecret[tool+"."+path+name] {
				continue
			}
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

	seedSecret(t, f, "tenant-credential-gh", "token")
	text, isErr = f.call(t, ToolSetOrganizationConnector, map[string]any{"namespace": orgNS, "name": "gh", "url": "https://mcp.github.example/mcp", "credentialHeader": "Authorization"})
	require.False(t, isErr, text)
	assert.NotContains(t, text, liveValue)
	cred = decode(t, text)["credentials"].([]any)[0].(map[string]any)
	assert.Equal(t, organizations.CredentialPresent, cred["state"])
	assert.NotContains(t, cred, "next")
	headers, _, _ := unstructured.NestedSlice(f.get(t, connGVR, orgNS, "gh").Object, "spec", "headersFrom")
	assert.Equal(t, []any{map[string]any{"name": "Authorization", "valueFrom": map[string]any{"type": "Secret", "name": "tenant-credential-gh", "key": "token"}}}, headers)

	text, isErr = f.call(t, ToolSetOrganizationEgress, map[string]any{"namespace": orgNS, "name": "github",
		"destinations": []any{map[string]any{"origin": "https://api.github.com", "credentialHeader": "Authorization", "credentialPrefix": "Bearer ", "credentialSecret": "tenant-credential-gh"}}}) // #nosec G101 -- a Secret's name, not a credential
	require.False(t, isErr, text)
	assert.NotContains(t, text, liveValue)
}

// list_organizations lists what the caller manages or belongs to, each
// review asked as the caller; a member who does not manage one sees its
// status only.
func TestListOrganizationsByTheCallersAccess(t *testing.T) {
	lost := organization("payments", nil)
	lost.Object["status"].(map[string]any)["conditions"] = append(lost.Object["status"].(map[string]any)["conditions"].([]any),
		map[string]any{"type": "Orphaned", "status": "True", "reason": "MissingFromGit", "message": "gone from git", "lastTransitionTime": "2026-11-04T00:00:00Z"})
	billing := organization("billing", nil)
	_ = unstructured.SetNestedField(billing.Object, "agents-billing-a1b2c", "status", "agentPlatform", "namespace")
	other := organization("treasury", nil)
	_ = unstructured.SetNestedField(other.Object, "agents-treasury-z9y8x", "status", "agentPlatform", "namespace")
	f := newOrgFixture(t, lost, billing, other)
	// A member of payments only, who may create agents there; the manager of
	// billing only; treasury neither.
	f.deny = func(ns, verb, resource, name string) bool {
		switch {
		case resource == "agents", resource == "helmreleases":
			return ns != orgNS
		case verb == "update" && resource == "organizations":
			return ns != "" || name != "billing"
		}
		return false
	}
	text, isErr := f.call(t, ToolListOrganizations, nil)
	require.False(t, isErr, text)
	listed := decode(t, text)["organizations"].([]any)
	require.Len(t, listed, 2, text)

	billingListed := listed[0].(map[string]any)
	assert.Equal(t, "billing", billingListed["name"])
	assert.Equal(t, map[string]any{"manage": true, "member": false, "createAgents": false}, billingListed["permissions"])
	assert.Equal(t, "s3://bucket/uid", billingListed["organization"].(map[string]any)["snapshotPrefix"], "a manager reads the summary")

	member := listed[1].(map[string]any)
	assert.Equal(t, map[string]any{"name": "payments", "namespace": orgNS,
		"orphaned":    map[string]any{"status": "True", "reason": "MissingFromGit", "message": "gone from git"},
		"permissions": map[string]any{"manage": false, "member": true, "createAgents": true}}, member, "a member reads the status only")
	assert.NotContains(t, text, "treasury", "an organization the caller neither manages nor belongs to is not listed")

	f.deny = func(string, string, string, string) bool { return true }
	text, isErr = f.call(t, ToolListOrganizations, nil)
	require.False(t, isErr, text)
	res := decode(t, text)
	assert.Empty(t, res["organizations"])
	assert.Contains(t, res["none"], "add_organization_member")

	// A refused token is not "no organization".
	f.typed.PrependReactor("create", "selfsubjectaccessreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewUnauthorized("token expired")
	})
	text, isErr = f.call(t, ToolListOrganizations, nil)
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "unauthenticated:"), text)
	assert.NotContains(t, text, "agents-", "a refused token learns no organization namespace")
}

// createAgents is the caller's right to create the HelmRelease of an agent in
// the organization namespace, asked as the caller, whatever role grants it: a
// user is a member who may not create agents, an editor creates them and does
// not manage the organization, and an admin who also manages it may do all
// three. An organization without a namespace yet has no namespace to create in.
func TestListOrganizationsSaysWhoMayCreateAgents(t *testing.T) {
	roles := map[string]func(ns, verb, resource, name string) bool{
		// agent-platform:user reads and runs the agents of its organization.
		"user": func(_, verb, resource, _ string) bool {
			return resource == "organizations" || resource == "helmreleases" || (resource == "agents" && verb != "list")
		},
		// agent-platform:editor also writes them.
		"editor": func(_, _, resource, _ string) bool { return resource == "organizations" },
		// agent-platform:admin of the organization and organizations-admin.
		"admin": func(string, string, string, string) bool { return false },
	}
	for _, tc := range []struct {
		role string
		want map[string]any
	}{
		{"user", map[string]any{"manage": false, "member": true, "createAgents": false}},
		{"editor", map[string]any{"manage": false, "member": true, "createAgents": true}},
		{"admin", map[string]any{"manage": true, "member": true, "createAgents": true}},
	} {
		t.Run(tc.role, func(t *testing.T) {
			f := newOrgFixture(t, organization("payments", nil))
			f.deny = roles[tc.role]
			reviews := recordAccessReviews(f)
			text, isErr := f.call(t, ToolListOrganizations, nil)
			require.False(t, isErr, text)
			listed := decode(t, text)["organizations"].([]any)
			require.Len(t, listed, 1, text)
			assert.Equal(t, tc.want, listed[0].(map[string]any)["permissions"])
			assert.Contains(t, reviews(), authorizationv1.ResourceAttributes{
				Namespace: orgNS, Verb: "create", Group: hrGVR.Group, Resource: hrGVR.Resource,
			}, "createAgents asks for the HelmReleases of the organization namespace")
		})
	}

	t.Run("no namespace yet", func(t *testing.T) {
		pending := organization("payments", nil)
		unstructured.RemoveNestedField(pending.Object, "status", "agentPlatform", "namespace")
		f := newOrgFixture(t, pending)
		reviews := recordAccessReviews(f)
		text, isErr := f.call(t, ToolListOrganizations, nil)
		require.False(t, isErr, text)
		listed := decode(t, text)["organizations"].([]any)
		require.Len(t, listed, 1, text)
		assert.Equal(t, map[string]any{"manage": true, "member": false, "createAgents": false}, listed[0].(map[string]any)["permissions"])
		assert.Equal(t, []authorizationv1.ResourceAttributes{{Verb: "update", Group: orgGVR.Group, Resource: orgGVR.Resource, Name: "payments"}}, reviews(),
			"the manage review of the cluster-scoped Organization alone: none in a namespace it does not have")
	})
}

// "May the caller create agents here" is one question: create on the
// HelmRelease create_agent writes, as the caller, in the organization
// namespace. A binding that grants only one of create on HelmReleases and
// create on Agents gives the same answer in list_organizations'
// createAgents, in create_agent and in get_info's namespaces.managed.
func TestCreateAgentsIsTheRightCreateAgentNeeds(t *testing.T) {
	for _, tc := range []struct {
		name string
		// denied is the one resource the binding does not grant create on.
		denied string
		want   bool
	}{
		{"agents only", "helmreleases", false},
		{"helmreleases only", "agents", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrgFixture(t, organization("payments", nil),
				&unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "api.kagent.dev/v1alpha3", "kind": "ModelConfig",
					"metadata": map[string]any{"name": "own", "namespace": orgNS},
					"spec":     map[string]any{"provider": "Anthropic", "model": "claude-sonnet-4-6"},
				}})
			f.deny = func(_, verb, resource, _ string) bool { return verb == "create" && resource == tc.denied }
			// The agent tools act as the same caller, with the organization
			// namespaces managed.
			svc := agents.New(f.provider, embeddedChart{}, nil, agents.InProcessPinner(pinner{}), agents.Config{
				DefaultNamespace: "kagent", Version: "test", Tags: tagstest.Rule(), Tenants: f.cfg.Namespaces,
			}, nil)
			f.srv = NewMCPServer(svc, "test", WithOrganizations(f.orgs), WithLifecycle(t.Context()))

			text, isErr := f.call(t, ToolListOrganizations, nil)
			require.False(t, isErr, text)
			listed := decode(t, text)["organizations"].([]any)
			require.Len(t, listed, 1, text)
			assert.Equal(t, tc.want, listed[0].(map[string]any)["permissions"].(map[string]any)["createAgents"], text)

			text, isErr = f.call(t, ToolGetInfo, nil)
			require.False(t, isErr, text)
			managed := decode(t, text)["namespaces"].(map[string]any)["managed"]
			if tc.want {
				assert.Contains(t, managed, orgNS, text)
			} else {
				assert.NotContains(t, managed, orgNS, text)
			}

			text, isErr = f.call(t, ToolCreateAgent, map[string]any{
				"namespace": orgNS, "name": "sre", "modelConfig": "own", "toolset": []string{"preset:read-only"},
			})
			if !tc.want {
				require.True(t, isErr, text)
				assert.True(t, strings.HasPrefix(text, "forbidden:"), text)
				assert.Nil(t, f.get(t, hrGVR, orgNS, "sre"), "nothing is written")
				return
			}
			require.False(t, isErr, text)
			assert.NotNil(t, f.get(t, hrGVR, orgNS, "sre"), "the HelmRelease is written in the organization namespace")
		})
	}
}

// A failed createAgents review (an error, not a denial) leaves createAgents
// out: not known, so the Dev Portal does not show an editor as unable to
// create agents. A denial answers false and an allowance true.
func TestListOrganizationsLeavesOutCreateAgentsItCouldNotAsk(t *testing.T) {
	for _, tc := range []struct {
		name   string
		review func(f *orgFixture)
		want   map[string]any
	}{
		{"failed", func(f *orgFixture) {
			f.typed.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
				ra := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview).Spec.ResourceAttributes
				if ra.Verb == "create" && ra.Resource == "helmreleases" {
					return true, nil, apierrors.NewInternalError(errors.New("etcd timeout"))
				}
				return false, nil, nil
			})
		}, map[string]any{"manage": true, "member": true}},
		// A failed authorization webhook: the review answers, but neither
		// allows nor denies.
		{"evaluation error", func(f *orgFixture) {
			f.typed.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
				review := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
				if ra := review.Spec.ResourceAttributes; ra.Verb == "create" && ra.Resource == "helmreleases" {
					review.Status = authorizationv1.SubjectAccessReviewStatus{EvaluationError: "webhook timeout"}
					return true, review, nil
				}
				return false, nil, nil
			})
		}, map[string]any{"manage": true, "member": true}},
		{"denied", func(f *orgFixture) {
			f.deny = func(_, verb, resource, _ string) bool { return resource == "helmreleases" && verb == "create" }
		}, map[string]any{"manage": true, "member": true, "createAgents": false}},
		{"allowed", func(*orgFixture) {}, map[string]any{"manage": true, "member": true, "createAgents": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrgFixture(t, organization("payments", nil))
			tc.review(f)
			text, isErr := f.call(t, ToolListOrganizations, nil)
			require.False(t, isErr, text)
			listed := decode(t, text)["organizations"].([]any)
			require.Len(t, listed, 1, text)
			assert.Equal(t, tc.want, listed[0].(map[string]any)["permissions"])
		})
	}
}

// recordAccessReviews records the attributes of every
// SelfSubjectAccessReview the caller asks, and leaves the answer to the
// fixture.
func recordAccessReviews(f *orgFixture) func() []authorizationv1.ResourceAttributes {
	var mu sync.Mutex
	var reviews []authorizationv1.ResourceAttributes
	f.typed.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		reviews = append(reviews, *a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview).Spec.ResourceAttributes)
		return false, nil, nil
	})
	return func() []authorizationv1.ResourceAttributes {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(reviews)
	}
}

// Editors and users are added into their role's binding and removed from
// it. The admins are listed from organization-admins, which the operator
// derives from spec.admins, and say so; the member tools neither give nor
// take the admin role, and name update_organization instead.
func TestOrganizationMembers(t *testing.T) {
	f := newOrgFixture(t)
	_, err := f.dyn.Resource(rbGVR).Namespace(orgNS).Create(context.Background(), &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": "organization-admins", "namespace": orgNS},
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
	assert.NotContains(t, members[0], "from")
	admin := members[1].(map[string]any)
	assert.Equal(t, "admin", admin["role"])
	assert.Equal(t, "organization-admins", admin["binding"])
	assert.Contains(t, admin["from"], "spec.admins")

	// An admin binding of another name is not the Organization's: it is
	// listed by its name, without from.
	_, err = f.dyn.Resource(rbGVR).Namespace(orgNS).Create(context.Background(), &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": "legacy-admins", "namespace": orgNS},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": organizations.RoleAdmin},
		"subjects": []any{map[string]any{"kind": "User", "name": "zoe@example.com", "apiGroup": "rbac.authorization.k8s.io"}},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)
	text, isErr = f.call(t, ToolListOrganizationMembers, map[string]any{"namespace": orgNS})
	require.False(t, isErr, text)
	members = decode(t, text)["members"].([]any)
	require.Len(t, members, 3)
	legacy := members[2].(map[string]any)
	assert.Equal(t, "legacy-admins", legacy["binding"])
	assert.NotContains(t, legacy, "from")
	// Nor does a remove without a role change it.
	text, isErr = f.call(t, ToolRemoveOrganizationMember, map[string]any{"namespace": orgNS, "user": "zoe@example.com"})
	require.False(t, isErr, text)
	assert.Equal(t, false, decode(t, text)["changed"])
	assert.Contains(t, decode(t, text)["note"], "legacy-admins")
	assert.Len(t, f.get(t, rbGVR, orgNS, "legacy-admins").Object["subjects"], 1)

	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{ToolAddOrganizationMember, map[string]any{"namespace": orgNS, "role": "admin", "user": "bob@example.com"}},
		{ToolRemoveOrganizationMember, map[string]any{"namespace": orgNS, "role": "admin", "user": "jane@example.com"}},
	} {
		text, isErr = f.call(t, call.tool, call.args)
		require.True(t, isErr, call.tool)
		assert.True(t, strings.HasPrefix(text, "invalid_request:"), text)
		assert.Contains(t, text, "spec.admins", call.tool)
		assert.Contains(t, text, "update_organization", call.tool)
	}
	// Without a role, an admin stays one, and the answer says why.
	text, isErr = f.call(t, ToolRemoveOrganizationMember, map[string]any{"namespace": orgNS, "user": "jane@example.com"})
	require.False(t, isErr, text)
	res := decode(t, text)
	assert.Equal(t, false, res["changed"])
	assert.Contains(t, res["note"], "update_organization")
	assert.Len(t, f.get(t, rbGVR, orgNS, "organization-admins").Object["subjects"], 1)

	text, isErr = f.call(t, ToolRemoveOrganizationMember, map[string]any{"namespace": orgNS, "group": "payments-team"})
	require.False(t, isErr, text)
	assert.Nil(t, f.get(t, rbGVR, orgNS, "organization-editors"), "an emptied binding goes")

	text, isErr = f.call(t, ToolAddOrganizationMember, map[string]any{"namespace": "org-acme", "role": "user", "group": "g"})
	require.True(t, isErr)
	assert.Contains(t, text, "namespace org-acme is not an organization namespace")
	assert.NotContains(t, text, "tenant", "an org-* namespace is no tenant of any kind")
}

// A create of an Organization that exists is refused at once: one being
// erased, one with the Agent Platform, and one without it, which the tools
// do not turn on.
func TestCreateOrganizationRefusesOneThatExists(t *testing.T) {
	erased := organization("payments", nil)
	now := metav1.Now()
	erased.SetDeletionTimestamp(&now)
	erased.SetFinalizers([]string{apov1alpha1.Finalizer})
	for _, tc := range []struct {
		name     string
		existing *unstructured.Unstructured
		want     string
	}{
		{"being erased", erased, "being erased"},
		{"with the Agent Platform", organization("payments", nil), "update_organization changes it"},
		{"without the Agent Platform", kaasOrganization("payments"), "do not turn it on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrgFixture(t, tc.existing)
			text, isErr := f.call(t, ToolCreateOrganization, map[string]any{"name": "payments", "adminGroups": []any{"g"}})
			require.True(t, isErr)
			assert.True(t, strings.HasPrefix(text, "conflict:"), text)
			assert.Contains(t, text, tc.want)
			assert.Empty(t, f.dryRuns.Received(), "nothing is sent")
		})
	}
}

// A delete the API server refuses after its dry run passed takes the delete
// intent off again: the admission guard keeps protecting the object.
func TestAFailedDeleteTakesTheDeleteIntentOffAgain(t *testing.T) {
	f := newOrgFixture(t, organization("payments", nil))
	f.dyn.PrependReactor("delete", "organizations", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if len(a.(k8stesting.DeleteActionImpl).DeleteOptions.DryRun) == 0 {
			return true, nil, apierrors.NewServiceUnavailable("webhook unavailable")
		}
		return false, nil, nil
	})
	text, isErr := f.call(t, ToolDeleteOrganization, map[string]any{"name": "payments", "confirm": "payments"})
	require.True(t, isErr, text)
	org := f.get(t, orgGVR, "", "payments")
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

// list_organizations lists the Organizations with the Agent Platform
// enabled, by name, and no other; a ServiceAccount that may not list them
// answers unsupported.
func TestListOrganizations(t *testing.T) {
	f := newOrgFixture(t, organization("payments", nil), organization("billing", nil), kaasOrganization("treasury"))
	names := func(text string) []string {
		var out []string
		for _, o := range decode(t, text)["organizations"].([]any) {
			out = append(out, o.(map[string]any)["name"].(string))
		}
		return out
	}
	text, isErr := f.call(t, ToolListOrganizations, nil)
	require.False(t, isErr, text)
	assert.Equal(t, []string{"billing", "payments"}, names(text))

	text, isErr = f.call(t, ToolGetOrganization, map[string]any{"name": "treasury"})
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "not_found:"), text)
	assert.Contains(t, text, "does not have the Agent Platform enabled")

	f.dyn.PrependReactor("list", "organizations", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(orgGVR.GroupResource(), "", errors.New("RBAC: access denied"))
	})
	text, isErr = f.call(t, ToolListOrganizations, nil)
	require.True(t, isErr)
	assert.True(t, strings.HasPrefix(text, "unsupported:"), text)
}

// An organization with no spec.workers.replicas runs the operator's default
// of 1, and the tools report it so. An omitted workerReplicas writes no
// field; an explicit 0 is written, also over an object without the field.
func TestWorkerReplicasDefaultIsOneAndAnExplicitZeroIsWritten(t *testing.T) {
	f := newOrgFixture(t, organization("payments", nil))
	replicasOf := func() (any, bool) {
		v, found, _ := unstructured.NestedFieldNoCopy(f.get(t, orgGVR, "", "payments").Object, "spec", "agentPlatform", "workers", "replicas")
		return v, found
	}

	text, isErr := f.call(t, ToolGetOrganization, map[string]any{"name": "payments"})
	require.False(t, isErr, text)
	assert.EqualValues(t, 1, decode(t, text)["workerReplicas"])

	text, isErr = f.call(t, ToolUpdateOrganization, map[string]any{"name": "payments", "displayName": "Payments"})
	require.False(t, isErr, text)
	_, found := replicasOf()
	assert.False(t, found, "an omitted workerReplicas writes no field")

	text, isErr = f.call(t, ToolUpdateOrganization, map[string]any{"name": "payments", "workerReplicas": 0})
	require.False(t, isErr, text)
	res := decode(t, text)
	assert.Equal(t, []any{"workerReplicas"}, res["changed"])
	assert.EqualValues(t, 1, res["before"].(map[string]any)["workerReplicas"])
	assert.EqualValues(t, 0, res["after"].(map[string]any)["workerReplicas"])
	replicas, found := replicasOf()
	require.True(t, found, "an explicit 0 is written")
	assert.EqualValues(t, 0, replicas)

	text, isErr = f.call(t, ToolUpdateOrganization, map[string]any{"name": "payments", "workerReplicas": 0})
	require.False(t, isErr, text)
	assert.Empty(t, decode(t, text)["changed"], "a stored 0 is no change")
}

// An explicit workerReplicas equal to the default is written over an
// object without the field, so it stays when the operator's default
// changes; a create with an explicit 0 writes 0.
func TestWorkerReplicasExplicitValuesAreWritten(t *testing.T) {
	f := newOrgFixture(t, organization("payments", nil))
	text, isErr := f.call(t, ToolUpdateOrganization, map[string]any{"name": "payments", "workerReplicas": 1})
	require.False(t, isErr, text)
	assert.Equal(t, []any{"workerReplicas"}, decode(t, text)["changed"])
	replicas, found, _ := unstructured.NestedFieldNoCopy(f.get(t, orgGVR, "", "payments").Object, "spec", "agentPlatform", "workers", "replicas")
	require.True(t, found, "an explicit 1 is written")
	assert.EqualValues(t, 1, replicas)

	f = newOrgFixture(t)
	text, isErr = f.call(t, ToolCreateOrganization, map[string]any{"name": "payments", "adminGroups": []any{"payments-leads"}, "workerReplicas": 0})
	require.False(t, isErr, text)
	replicas, found, _ = unstructured.NestedFieldNoCopy(f.get(t, orgGVR, "", "payments").Object, "spec", "agentPlatform", "workers", "replicas")
	require.True(t, found, "a create writes an explicit 0")
	assert.EqualValues(t, 0, replicas)
}

// get_organization answers what a delete erases as far as the caller can
// read it, and names what it cannot read instead of failing.
func TestGetOrganizationNamesWhatItCannotRead(t *testing.T) {
	f := newOrgFixture(t, organization("payments", nil), model("claude", true))
	f.dyn.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == orgNS && (a.GetResource().Resource == "agents" || a.GetResource().Resource == "rolebindings") {
			return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("RBAC: access denied"))
		}
		return false, nil, nil
	})
	text, isErr := f.call(t, ToolGetOrganization, map[string]any{"name": "payments"})
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
	gitOrg := organization("payments", fromGit)
	for name, call := range map[string]struct {
		tool string
		args map[string]any
	}{
		"change a model":          {ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "claude", "model": "other"}},
		"move the mark off one":   {ToolSetOrganizationModel, map[string]any{"namespace": orgNS, "name": "haiku", "provider": "Anthropic", "model": "m", "default": true}},
		"delete a model":          {ToolDeleteOrganizationObject, map[string]any{"namespace": orgNS, "kind": "model", "name": "claude"}},
		"add to a git binding":    {ToolAddOrganizationMember, map[string]any{"namespace": orgNS, "role": "editor", "user": "jane@example.com"}},
		"remove from one":         {ToolRemoveOrganizationMember, map[string]any{"namespace": orgNS, "group": "payments-team"}},
		"change the organization": {ToolUpdateOrganization, map[string]any{"name": "payments", "adminGroups": []any{"g"}}},
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

// list_organization_connectors marks an agent's muster binding with the
// agent's name, by the release that renders it with the Agent and not by a
// label the agent's values can set; a connector of the organization and one
// another chart's HelmRelease renders stay unmarked.
func TestListConnectorsMarksMusterBindings(t *testing.T) {
	object := func(kind, name, release string, labels map[string]any) *unstructured.Unstructured {
		if release != "" {
			labels["helm.toolkit.fluxcd.io/name"] = release
			labels["helm.toolkit.fluxcd.io/namespace"] = orgNS
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "api.kagent.dev/v1alpha3", "kind": kind,
			"metadata": map[string]any{"name": name, "namespace": orgNS, "labels": labels},
			"spec":     map[string]any{"url": "https://" + name + ".example/mcp"},
		}}
	}
	f := newOrgFixture(t,
		object("RemoteMCPServer", "jira", "", map[string]any{}),
		// helper's values set app.kubernetes.io/name and a muster URL of their own.
		object("RemoteMCPServer", "helper", "helper", map[string]any{"app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/name": "helper"}),
		object("Agent", "helper", "helper", map[string]any{"app.kubernetes.io/managed-by": "Helm"}),
		object("RemoteMCPServer", "vendor", "vendor-mcp", map[string]any{"app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/name": "vendor-mcp"}),
	)

	text, isErr := f.call(t, ToolListOrganizationConnector, map[string]any{"namespace": orgNS})
	require.False(t, isErr, text)
	var out struct {
		Connectors []struct{ Name, ManagedBy, MusterBindingOf string }
	}
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, []struct{ Name, ManagedBy, MusterBindingOf string }{
		{"helper", "Helm", "helper"},
		{"jira", "", ""},
		{"vendor", "Helm", ""},
	}, out.Connectors)

	// A caller who may not list the Agents still gets the list, unmarked.
	f.dyn.PrependReactor("list", "agents", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("RBAC: access denied"))
	})
	text, isErr = f.call(t, ToolListOrganizationConnector, map[string]any{"namespace": orgNS})
	require.False(t, isErr, text)
	out.Connectors = nil
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, []struct{ Name, ManagedBy, MusterBindingOf string }{
		{"helper", "Helm", ""},
		{"jira", "", ""},
		{"vendor", "Helm", ""},
	}, out.Connectors)
}

// list_organization_connectors says, for each credential, whether its Secret
// exists, asked as the caller with a dry-run create and never by a read; a
// caller who may not create the Secret gets unknown. Each connector carries
// kagent's Accepted condition of its current generation.
func TestListConnectorsReportsCredentialAndAcceptedState(t *testing.T) {
	connector := func(name string, generation int64, condition map[string]any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "api.kagent.dev/v1alpha3", "kind": "RemoteMCPServer",
			"metadata": map[string]any{"name": name, "namespace": orgNS, "generation": generation},
			"spec": map[string]any{"url": "https://" + name + ".example/mcp", "headersFrom": []any{
				map[string]any{"name": "Authorization", "valueFrom": map[string]any{"type": "Secret", "name": "tenant-credential-" + name, "key": "token"}},
			}},
		}}
		if condition != nil {
			u.Object["status"] = map[string]any{"conditions": []any{condition}}
		}
		return u
	}
	f := newOrgFixture(t,
		connector("jira", 2, map[string]any{"type": "Accepted", "status": "True", "reason": "DiscoverySucceeded", "message": "3 tools", "observedGeneration": int64(2)}),
		connector("wiki", 1, map[string]any{"type": "Accepted", "status": "False", "reason": "DiscoveryFailed", "message": "401 Unauthorized", "observedGeneration": int64(1)}),
		connector("stale", 3, map[string]any{"type": "Accepted", "status": "True", "reason": "DiscoverySucceeded", "observedGeneration": int64(2)}),
	)
	seedSecret(t, f, "tenant-credential-jira", "token")

	type state struct {
		Name        string
		Accepted    *bool
		Reason      string
		Message     string
		Credentials []struct{ Secret, State string }
	}
	list := func() map[string]state {
		text, isErr := f.call(t, ToolListOrganizationConnector, map[string]any{"namespace": orgNS})
		require.False(t, isErr, text)
		var out struct{ Connectors []state }
		require.NoError(t, json.Unmarshal([]byte(text), &out))
		byName := map[string]state{}
		for _, c := range out.Connectors {
			byName[c.Name] = c
		}
		return byName
	}
	yes, no := true, false

	got := list()
	assert.Equal(t, &yes, got["jira"].Accepted)
	assert.Equal(t, "DiscoverySucceeded", got["jira"].Reason)
	assert.Equal(t, []struct{ Secret, State string }{{"tenant-credential-jira", "present"}}, got["jira"].Credentials)
	assert.Equal(t, &no, got["wiki"].Accepted)
	assert.Equal(t, "401 Unauthorized", got["wiki"].Message)
	assert.Equal(t, []struct{ Secret, State string }{{"tenant-credential-wiki", "missing"}}, got["wiki"].Credentials)
	assert.Nil(t, got["stale"].Accepted, "a condition of an earlier generation is not reported")
	assert.Nil(t, f.get(t, secretGVR, orgNS, "tenant-credential-wiki"), "the check writes nothing")

	f.deny = func(_, verb, resource, _ string) bool { return verb == "create" && resource == "secrets" }
	got = list()
	assert.Equal(t, []struct{ Secret, State string }{{"tenant-credential-jira", "unknown"}}, got["jira"].Credentials)
	assert.Equal(t, []struct{ Secret, State string }{{"tenant-credential-wiki", "unknown"}}, got["wiki"].Credentials)

	// A dry run the API server does not answer leaves the state unknown and
	// the listing whole.
	f.deny = func(string, string, string, string) bool { return false }
	f.dyn.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.CreateActionImpl).GetObject().(*unstructured.Unstructured).GetName() == "tenant-credential-wiki" {
			return true, nil, apierrors.NewServiceUnavailable("etcd timeout")
		}
		return false, nil, nil
	})
	got = list()
	assert.Equal(t, []struct{ Secret, State string }{{"tenant-credential-jira", "present"}}, got["jira"].Credentials)
	assert.Equal(t, []struct{ Secret, State string }{{"tenant-credential-wiki", "unknown"}}, got["wiki"].Credentials)
}
