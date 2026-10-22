package agents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/kagent/kagenttest"
	"github.com/giantswarm/agent-manager/internal/workspaces"
)

func TestValidateDefaultWorkspace(t *testing.T) {
	for workspace, want := range map[string]string{
		"":                  "",
		"kagent/platform":   "",
		"kagent/a.b-c":      "",
		"platform":          "want <namespace>/<name>",
		"kagent/":           "want <namespace>/<name>",
		"/platform":         "want <namespace>/<name>",
		"kagent/a/b":        "want <namespace>/<name>",
		"Kagent/platform":   "no namespace name",
		"kagent/Platform":   "no workspace name",
		"tenant-b/platform": "cannot be the default of an agent in kagent",
	} {
		err := ValidateDefaultWorkspace(workspace, "kagent", Target{})
		if want == "" {
			assert.NoError(t, err, workspace)
			continue
		}
		require.ErrorIs(t, err, ErrInvalid, workspace)
		assert.Contains(t, err.Error(), want, workspace)
	}
	err := ValidateDefaultWorkspace("kagent/platform", "kagent", Target{Organization: "acme", Cluster: "wc1"})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "workload cluster")
}

func TestCreateKeepsTheDefaultWorkspaceAsAnAnnotation(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	readOnly := []string{"preset:read-only"}

	_, err := f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly, DefaultWorkspace: "tenant-b/platform"})
	require.ErrorIs(t, err, ErrInvalid, "a workspace of another tenant")
	assert.Contains(t, err.Error(), "tenant-b")
	_, err = f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly, Annotations: map[string]string{DefaultWorkspaceAnnotation: "kagent/platform"}})
	require.ErrorIs(t, err, ErrInvalid, "the annotation through annotations")
	assert.Contains(t, err.Error(), "set by defaultWorkspace")
	res, err := f.svc.ValidateCreate(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly, DefaultWorkspace: "tenant-b/platform"})
	require.NoError(t, err)
	assert.False(t, res.Valid, "validate_agent answers as create_agent")

	_, err = f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly, DefaultWorkspace: "kagent/platform", Annotations: map[string]string{"team": "sre"}})
	require.NoError(t, err)
	hr, err := f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "sre", metav1.GetOptions{})
	require.NoError(t, err)
	annotations, _, _ := unstructured.NestedStringMap(hr.Object, "spec", "values", "annotations")
	assert.Equal(t, map[string]string{"team": "sre", DefaultWorkspaceAnnotation: "kagent/platform"}, annotations, "the chart puts it on the Agent")

	got, err := f.svc.Get(ctx, In(""), "sre")
	require.NoError(t, err)
	assert.Equal(t, "kagent/platform", got.DefaultWorkspace)
	list, err := f.svc.List(ctx, In(""))
	require.NoError(t, err)
	for _, s := range Summaries(list) {
		if s.Name == "sre" {
			assert.Equal(t, "kagent/platform", s.DefaultWorkspace, "list_agents reports it")
		} else {
			assert.Empty(t, s.DefaultWorkspace)
		}
	}
}

func TestTheAgentObjectReportsItsDefaultWorkspace(t *testing.T) {
	obj := agentObject("kagent", "bare", "", "", true, true)
	obj.SetAnnotations(map[string]string{DefaultWorkspaceAnnotation: "kagent/platform"})
	assert.Equal(t, "kagent/platform", agentFromObject(obj, nil).DefaultWorkspace)
}

func TestUpdateChangesOrRemovesTheDefaultWorkspace(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	defaultOf := func() (string, map[string]string) {
		t.Helper()
		hr, err := f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "verifier", metav1.GetOptions{})
		require.NoError(t, err)
		annotations, _, _ := unstructured.NestedStringMap(hr.Object, "spec", "values", "annotations")
		return annotations[DefaultWorkspaceAnnotation], annotations
	}
	ptr := func(s string) *string { return &s }

	_, err := f.svc.Update(ctx, Update{Name: "verifier", DefaultWorkspace: ptr("kagent/platform")})
	require.NoError(t, err)
	got, _ := defaultOf()
	assert.Equal(t, "kagent/platform", got)

	_, err = f.svc.Update(ctx, Update{Name: "verifier", Annotations: &map[string]string{"team": "sre"}})
	require.NoError(t, err)
	got, annotations := defaultOf()
	assert.Equal(t, "kagent/platform", got, "replacing the annotations keeps it")
	assert.Equal(t, "sre", annotations["team"])

	_, err = f.svc.Update(ctx, Update{Name: "verifier", Annotations: &map[string]string{DefaultWorkspaceAnnotation: "kagent/other"}})
	require.ErrorIs(t, err, ErrInvalid, "annotations never change it")
	_, err = f.svc.Update(ctx, Update{Name: "verifier", DefaultWorkspace: ptr("tenant-b/platform")})
	require.ErrorIs(t, err, ErrInvalid)

	_, err = f.svc.Update(ctx, Update{Name: "verifier", DefaultWorkspace: ptr("kagent/other")})
	require.NoError(t, err)
	got, _ = defaultOf()
	assert.Equal(t, "kagent/other", got)

	_, err = f.svc.Update(ctx, Update{Name: "verifier", DefaultWorkspace: ptr("")})
	require.NoError(t, err)
	got, annotations = defaultOf()
	assert.Empty(t, got, "\"\" removes it")
	assert.Equal(t, map[string]string{"team": "sre"}, annotations)
}

// fakeWorkspaces answers authorize_workspace and core_auth_login as the
// workspace-manager and muster would.
type fakeWorkspaces struct {
	answer *workspaces.Authorization
	err    error
	link   string
	calls  []string
	tokens []string
}

func (f *fakeWorkspaces) Authorize(_ context.Context, bearer, name string) (*workspaces.Authorization, error) {
	f.calls, f.tokens = append(f.calls, "authorize "+name), append(f.tokens, bearer)
	return f.answer, f.err
}

func (f *fakeWorkspaces) SignInLink(_ context.Context, bearer, server string) (string, error) {
	f.calls, f.tokens = append(f.calls, "login "+server), append(f.tokens, bearer)
	return f.link, nil
}

// newDefaultWorkspaceFixture is agent sre with the default workspace
// kagent/platform, and the workspace-manager answering answer.
func newDefaultWorkspaceFixture(t *testing.T, ws *fakeWorkspaces) (*Service, *kagenttest.Controller, *fakeBindings) {
	t.Helper()
	objs := sessionAgent("sre", []any{"preset:read-only"})
	objs[1].(*unstructured.Unstructured).SetAnnotations(map[string]string{DefaultWorkspaceAnnotation: "kagent/platform"})
	svc, ctrl, bindings := newBoundSessionFixtureWith(t, objs...)
	if ws != nil {
		svc.cfg.Workspaces = ws
	}
	return svc, ctrl, bindings
}

func TestABareStartBeginsInTheAgentsDefaultWorkspaceAsTheCaller(t *testing.T) {
	grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())
	ws := &fakeWorkspaces{answer: &workspaces.Authorization{Outcome: workspaces.Granted, Grant: grant}}
	svc, ctrl, bindings := newDefaultWorkspaceFixture(t, ws)
	ctx := asPerson(t.Context(), "admin@lab.local")

	started, err := svc.StartSession(ctx, StartSession{Name: "sre", RequestID: "req-1"})
	require.NoError(t, err)
	assert.Equal(t, []string{"authorize platform"}, ws.calls, "authorize_workspace for the default, by its name")
	assert.Equal(t, []string{callerToken}, ws.tokens, "as the caller")
	assert.Equal(t, "kagent/platform", started.Workspace)
	reqs := createRequests(ctrl)
	require.Len(t, reqs, 1)
	want := &apiv1alpha1.CreateSessionRequest{
		Agent:          &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: "sre"},
		RequestId:      "req-1",
		VolumeSource:   platformVolume(),
		AdmissionToken: grant,
		BindActor:      true,
	}
	assert.True(t, proto.Equal(want, reqs[0]), "the grant goes to CreateSession as a surface's does: %v", reqs[0])
	_ = bindings
	assert.NotContains(t, string(started.Session), grant)
}

func TestNoWorkspaceStartsWithoutTheDefault(t *testing.T) {
	ws := &fakeWorkspaces{err: errors.New("never called")}
	svc, ctrl, _ := newDefaultWorkspaceFixture(t, ws)
	ctx := asPerson(t.Context(), "admin@lab.local")

	started, err := svc.StartSession(ctx, StartSession{Name: "sre", NoWorkspace: true})
	require.NoError(t, err)
	assert.Empty(t, started.Workspace)
	assert.Empty(t, ws.calls)
	reqs := createRequests(ctrl)
	require.Len(t, reqs, 1)
	assert.Nil(t, reqs[0].GetVolumeSource())
	assert.Empty(t, reqs[0].GetAdmissionToken())

	grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())
	_, err = svc.StartSession(ctx, StartSession{Name: "sre", NoWorkspace: true, WorkspaceGrant: grant})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "exclude each other")
}

func TestAPassedGrantWinsOverTheDefault(t *testing.T) {
	ws := &fakeWorkspaces{err: errors.New("never called")}
	svc, ctrl, _ := newDefaultWorkspaceFixture(t, ws)
	ctx := asPerson(t.Context(), "admin@lab.local")
	grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())

	_, err := svc.StartSession(ctx, StartSession{Name: "sre", WorkspaceGrant: grant})
	require.NoError(t, err)
	assert.Empty(t, ws.calls)
	assert.Len(t, createRequests(ctrl), 1)
}

func TestAnAgentWithoutADefaultStartsWithoutAWorkspace(t *testing.T) {
	svc, ctrl, _ := newBoundSessionFixture(t)
	ws := &fakeWorkspaces{err: errors.New("never called")}
	svc.cfg.Workspaces = ws
	ctx := asPerson(t.Context(), "admin@lab.local")

	_, err := svc.StartSession(ctx, StartSession{Name: "sre"})
	require.NoError(t, err)
	assert.Empty(t, ws.calls)
	reqs := createRequests(ctrl)
	require.Len(t, reqs, 1)
	assert.Empty(t, reqs[0].GetAdmissionToken())
}

func TestARefusedDefaultWorkspaceStartsNoSession(t *testing.T) {
	for name, test := range map[string]struct {
		ws    *fakeWorkspaces
		err   error
		wants []string
		calls []string
	}{
		"unreadable repositories": {
			ws:    &fakeWorkspaces{answer: &workspaces.Authorization{Outcome: workspaces.Refused, Unreadable: []string{"giantswarm/secret", "giantswarm/hidden"}}},
			err:   ErrForbidden,
			wants: []string{"kagent/platform", "giantswarm/secret, giantswarm/hidden", "noWorkspace"},
		},
		"a provider to connect": {
			ws: &fakeWorkspaces{
				answer: &workspaces.Authorization{Outcome: workspaces.ConnectRequired, Connect: []workspaces.Connect{{Provider: "github", SignIn: "call core_auth_login server=github-app"}}},
				link:   "https://muster.example/auth/github-app",
			},
			err:   ErrAuthRequired,
			wants: []string{"kagent/platform", "provider github at muster server github-app: https://muster.example/auth/github-app", "not agent-manager's GitHub one"},
			calls: []string{"authorize platform", "login github-app"},
		},
		"muster's sign-in to the workspace-manager": {
			ws:    &fakeWorkspaces{err: &workspaces.AuthRequiredError{Server: "workspace-manager", LoginURL: "https://muster.example/auth/workspace-manager"}},
			err:   ErrAuthRequired,
			wants: []string{"kagent/platform", "muster server workspace-manager", "https://muster.example/auth/workspace-manager"},
		},
		"a deleted default": {
			ws:    &fakeWorkspaces{err: &workspaces.ToolError{Tool: "x_workspace-manager_authorize_workspace", Message: `authorize workspace: workspace "platform" not found`, Dispatched: true}},
			err:   ErrNotFound,
			wants: []string{"kagent/platform", "not found"},
		},
		"not synced yet": {
			ws:    &fakeWorkspaces{answer: &workspaces.Authorization{Outcome: workspaces.Unavailable, Reason: "workspace platform has not synced yet"}},
			err:   ErrConflict,
			wants: []string{"kagent/platform", "has not synced yet"},
		},
		"no workspace-manager configured": {
			err:   ErrUnsupported,
			wants: []string{"kagent/platform", "noWorkspace"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, ctrl, bindings := newDefaultWorkspaceFixture(t, test.ws)
			ctx := asPerson(t.Context(), "admin@lab.local")

			_, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello"})
			require.ErrorIs(t, err, test.err)
			for _, want := range test.wants {
				assert.Contains(t, err.Error(), want)
			}
			assert.Empty(t, createRequests(ctrl), "no Session is created")
			assert.Empty(t, bindings.Bound)
			if test.calls != nil {
				assert.Equal(t, test.calls, test.ws.calls)
			}
		})
	}
}

func TestABareStartWithoutBindingsIsRefusedNamingTheDefault(t *testing.T) {
	svc, ctrl, _ := newDefaultWorkspaceFixture(t, &fakeWorkspaces{err: errors.New("never called")})
	svc.cfg.Bindings = nil
	ctx := asPerson(t.Context(), "admin@lab.local")

	_, err := svc.StartSession(ctx, StartSession{Name: "sre"})
	require.ErrorIs(t, err, ErrUnsupported)
	assert.Contains(t, err.Error(), "kagent/platform")
	assert.Empty(t, createRequests(ctrl), "it never falls back to no workspace")
}

func TestAGrantOfAnotherWorkspaceIsRefused(t *testing.T) {
	other := kagenttest.WorkspaceGrant()
	other.Workspace.Name = "other"
	grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, other)
	ws := &fakeWorkspaces{answer: &workspaces.Authorization{Outcome: workspaces.Granted, Grant: grant}}
	svc, ctrl, _ := newDefaultWorkspaceFixture(t, ws)

	_, err := svc.StartSession(asPerson(t.Context(), "admin@lab.local"), StartSession{Name: "sre"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kagent/other")
	assert.NotContains(t, err.Error(), grant)
	assert.Empty(t, createRequests(ctrl))
}
