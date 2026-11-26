package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/approvalgrants"
	"github.com/giantswarm/agent-manager/internal/chart"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kagent"
	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/kagent/kagenttest"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/muster"
)

// toolsChart is the embedded chart with the tool approval values (tools,
// default) in its schema, as the agent chart from tool approval on.
type toolsChart struct{ embeddedChart }

func (toolsChart) Schema(context.Context) chart.Schema {
	s := chart.EmbeddedSchema()
	doc := runtime.DeepCopyJSONValue(s.Document).(map[string]any)
	props := doc["properties"].(map[string]any)
	props[ToolsValuesKey] = map[string]any{"type": "array", "maxItems": float64(MaxToolEntries)}
	props[DefaultValuesKey] = map[string]any{"type": "string", "enum": []any{"Allow", "Ask", "Block"}}
	s.Document = doc
	return s
}

// fakeTool is one tool muster lists: the selectors that select it and its
// connector's own setting.
type fakeTool struct {
	name       string
	selectedBy []string
	admin      string
	connector  Connector
}

// fakeReporter answers filter_tools with agentTools: the deciding entry of
// each listed tool (tool: before server:, as muster does; the default for a
// tool no setting entry selects).
type fakeReporter struct {
	tools  []fakeTool
	err    error
	calls  int
	onCall func()
}

func (r *fakeReporter) AgentTools(_ context.Context, token string, q AgentToolsQuery) ([]ReportedTool, error) {
	r.calls++
	if r.onCall != nil {
		r.onCall()
	}
	if r.err != nil {
		return nil, r.err
	}
	if token == "" {
		return nil, errors.New("no token")
	}
	var out []ReportedTool
	for _, t := range r.tools {
		entry := ""
		for _, kind := range []string{"tool:", "workflow:", "server:", "preset:"} {
			for _, e := range q.Tools {
				if e.Setting != "" && strings.HasPrefix(e.Selector, kind) && slices.Contains(t.selectedBy, e.Selector) && entry == "" {
					entry = e.Selector
				}
			}
		}
		listed := slices.ContainsFunc(q.Tools, func(e ToolEntry) bool { return slices.Contains(t.selectedBy, e.Selector) })
		if !listed {
			continue
		}
		if entry == "" && q.Default != "" {
			entry = DefaultEntry
		}
		c := t.connector
		c.Tool = strings.TrimPrefix(t.name, "x_"+c.Server+"_")
		rt := ReportedTool{Name: t.name, Entry: entry, Connectors: []Connector{c}}
		if entry != "" {
			rt.Admin = t.admin
		}
		out = append(out, rt)
	}
	return out, nil
}

var (
	github    = Connector{Server: "github", Namespace: "muster", Name: "github", UID: "uid-github"}
	pagerduty = Connector{Server: "pagerduty", Namespace: "connectors", Name: "pagerduty", UID: "uid-pd"}
)

// githubTools: merge asks on the connector, get_issue runs; pagerduty's ack
// asks too.
func githubTools() *fakeReporter {
	return &fakeReporter{tools: []fakeTool{
		{name: "x_github_merge_pull_request", selectedBy: []string{"server:github", "tool:x_github_merge_pull_request"}, admin: "ask", connector: github},
		{name: "x_github_get_issue", selectedBy: []string{"server:github", "tool:x_github_get_issue"}, admin: "allow", connector: github},
		{name: "x_pagerduty_ack", selectedBy: []string{"server:pagerduty"}, admin: "ask", connector: pagerduty},
	}}
}

// approvalBindings records binds and grant drops; it sends the approval
// sender token.
type approvalBindings struct {
	fakeBindings
	mu     sync.Mutex
	drops  []grantDrop
	onBind func()
	// dropFails fails that many grant drops before one goes through.
	dropFails int
	// noSender: binds carry no sender token (tool approval off).
	noSender bool
}

type grantDrop struct {
	Agent string
	Keep  []muster.Grant
}

func (b *approvalBindings) SendsApproval() bool { return !b.noSender }

func (b *approvalBindings) Bind(ctx context.Context, token string, binding muster.Binding) (muster.BindResult, error) {
	res, err := b.fakeBindings.Bind(ctx, token, binding)
	if err != nil {
		return res, err
	}
	if b.onBind != nil {
		b.onBind()
	}
	return res, nil
}

func (b *approvalBindings) DropGrants(_ context.Context, _ string, agent string, keep []muster.Grant) (muster.DropResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dropFails > 0 {
		b.dropFails--
		return muster.DropResult{}, errors.New("muster unavailable")
	}
	b.drops = append(b.drops, grantDrop{Agent: agent, Keep: keep})
	return muster.DropResult{Bindings: 2, Dropped: 1}, nil
}

// callerIdentity is a kube provider that talks to Kubernetes as the caller
// (downstream OAuth), where the connector-admin check is answered.
type callerIdentity struct{ kube.Provider }

func (callerIdentity) Identity() string { return kube.IdentityCaller }

type approvalFixture struct {
	*fixture
	reporter *fakeReporter
	grants   *approvalgrants.MemoryStore
	bindings *approvalBindings
	logs     *bytes.Buffer
	// admins are the namespaces whose mcpservers the caller may update.
	admins map[string]bool
	// denyListReleases are the namespaces where the caller may not list
	// HelmReleases (SelfSubjectAccessReview).
	denyListReleases map[string]bool
}

const alertUID = "uid-alert"

// newApprovalFixture is the seeded fixture with the tools chart, muster's
// reporter, a grants store, the binder and an agent alert-review whose
// release (uid alertUID, generation 7) lists github with merge Allowed.
func newApprovalFixture(t *testing.T, values map[string]any) *approvalFixture {
	t.Helper()
	f := seeded(t)
	f.svc.chart = toolsChart{}
	if values == nil {
		values = map[string]any{
			"agent":       map[string]any{"name": "alert-review"},
			"modelConfig": map[string]any{"name": "default-model-config"},
			"tools":       []any{"server:github", map[string]any{"tool:x_github_merge_pull_request": "Allow"}},
		}
	}
	hr := helmRelease("kagent", "alert-review", values, true, nil)
	hr.SetUID(alertUID)
	hr.SetGeneration(7)
	mustCreate(t, f, hrGVR, hr)
	mustCreate(t, f, agentGVR, agentObject("kagent", "alert-review", "alert-review", "kagent", true, true))
	mustCreate(t, f, serverGVR, remoteMCPServer("kagent", "alert-review", "server:github", "tool:x_github_merge_pull_request"))
	a := &approvalFixture{fixture: f, reporter: githubTools(), grants: approvalgrants.NewMemoryStore(), bindings: &approvalBindings{}, logs: &bytes.Buffer{}, admins: map[string]bool{}}
	f.svc.kube = callerIdentity{f.svc.kube}
	syncSleep = func(context.Context, time.Duration) {}
	f.svc.cfg.Approvals = a.reporter
	f.svc.cfg.Grants = a.grants
	f.svc.cfg.Bindings = a.bindings
	f.svc.log = slog.New(slog.NewJSONHandler(a.logs, nil))
	f.typed.PrependReactor("create", "selfsubjectaccessreviews", func(act k8stesting.Action) (bool, runtime.Object, error) {
		review := act.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		switch review.Spec.ResourceAttributes.Resource {
		case MCPServersResource:
			review.Status.Allowed = a.admins[review.Spec.ResourceAttributes.Namespace]
		case "helmreleases":
			review.Status.Allowed = !a.denyListReleases[review.Spec.ResourceAttributes.Namespace]
		default:
			return false, nil, nil
		}
		return true, review, nil
	})
	return a
}

var alertAgent = approvalgrants.Agent{UID: alertUID, Namespace: "kagent", Name: "alert-review"}

func (a *approvalFixture) stored(t *testing.T) []approvalgrants.Grant {
	t.Helper()
	rec, err := a.grants.Get(context.Background(), alertAgent)
	require.NoError(t, err)
	return rec.Grants
}

func (a *approvalFixture) values(t *testing.T) map[string]any {
	t.Helper()
	hr, err := a.dyn.Resource(hrGVR).Namespace("kagent").Get(context.Background(), "alert-review", metav1.GetOptions{})
	require.NoError(t, err)
	return mustValues(hr)
}

func dana(t *testing.T) context.Context { return asPerson(t.Context(), "dana@example.com") }

func TestCreateWritesToolsAndDefaultWhereTheChartTakesThem(t *testing.T) {
	a := newApprovalFixture(t, nil)
	res, err := a.svc.Create(dana(t), Spec{Name: "fresh", ModelConfig: "default-model-config",
		Tools: ToolList{{Selector: "server:github", Setting: "Ask"}, {Selector: "tool:x_github_delete_repository", Setting: "Block"}}, Default: "Ask"})
	require.NoError(t, err)
	hr, err := a.dyn.Resource(hrGVR).Namespace("kagent").Get(t.Context(), "fresh", metav1.GetOptions{})
	require.NoError(t, err)
	values := mustValues(hr)
	assert.Equal(t, []any{map[string]any{"server:github": "Ask"}, map[string]any{"tool:x_github_delete_repository": "Block"}}, values[ToolsValuesKey])
	assert.Equal(t, "Ask", values[DefaultValuesKey])
	assert.NotContains(t, values, ToolsetValuesKey, "tools replaces toolset")
	assert.Equal(t, ToolList{{Selector: "server:github", Setting: "Ask"}, {Selector: "tool:x_github_delete_repository", Setting: "Block"}}, res.Agent.ToolSettings)
	assert.Contains(t, a.logs.String(), `"msg":"agent tools changed"`)
	assert.Contains(t, a.logs.String(), `"savedBy":"dana@example.com"`)
}

func TestTheDeprecatedToolsetIsAToolListWithoutSettings(t *testing.T) {
	a := newApprovalFixture(t, nil)
	_, err := a.svc.Create(dana(t), Spec{Name: "fresh", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}})
	require.NoError(t, err)
	hr, _ := a.dyn.Resource(hrGVR).Namespace("kagent").Get(t.Context(), "fresh", metav1.GetOptions{})
	assert.Equal(t, []any{"preset:read-only"}, mustValues(hr)[ToolsValuesKey])
	_, err = a.svc.Create(dana(t), Spec{Name: "both", Toolset: []string{"preset:none"}, Tools: ToolList{{Selector: "preset:none"}}})
	require.ErrorIs(t, err, ErrInvalid)
}

func TestAChartWithoutToolsKeepsToolsetAndRefusesSettings(t *testing.T) {
	f := seeded(t)
	_, err := f.svc.Create(dana(t), Spec{Name: "fresh", ModelConfig: "default-model-config", Tools: ToolList{{Selector: "server:github", Setting: "Ask"}}})
	require.ErrorIs(t, err, ErrUnsupported)
	assert.Contains(t, err.Error(), "no tool approval settings")
	_, err = f.svc.Create(dana(t), Spec{Name: "fresh", ModelConfig: "default-model-config", Tools: ToolList{{Selector: "server:github"}}})
	require.NoError(t, err)
	hr, _ := f.dyn.Resource(hrGVR).Namespace("kagent").Get(t.Context(), "fresh", metav1.GetOptions{})
	assert.Equal(t, []any{"server:github"}, mustValues(hr)[ToolsetValuesKey])
}

func TestToolListRules(t *testing.T) {
	for name, l := range map[string]ToolList{
		"twice":           {{Selector: "server:a"}, {Selector: "server:a", Setting: "Ask"}},
		"lowercase":       {{Selector: "server:a", Setting: "ask"}},
		"none with more":  {{Selector: "preset:none"}, {Selector: "server:a"}},
		"none setting":    {{Selector: "preset:none", Setting: "Block"}},
		"bad grammar":     {{Selector: "label:x"}},
		"empty is absent": {},
	} {
		assert.ErrorIs(t, ValidateToolList(l, true), ErrInvalid, name)
	}
	assert.NoError(t, ValidateToolList(ToolList{{Selector: "server:a", Setting: "Allow"}, {Selector: "tool:x"}}, true))
}

func TestCreateAndUpdateAccessNeverGrant(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.admins["muster"] = true // even a connector admin: grants go through set_agent_approval

	_, err := a.svc.Create(dana(t), Spec{Name: "fresh", ModelConfig: "default-model-config", Tools: ToolList{{Selector: "tool:x_github_merge_pull_request", Setting: "Allow"}}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "create_agent does not grant exceptions: tool:x_github_merge_pull_request: Allow")
	_, err = a.svc.Create(dana(t), Spec{Name: "fresh", ModelConfig: "default-model-config", Tools: ToolList{{Selector: "server:github"}}, Default: "Allow"})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "default: Allow")

	// An Allow over a connector Allow changes nothing at runtime: taken.
	_, err = a.svc.Create(dana(t), Spec{Name: "fresh", ModelConfig: "default-model-config", Tools: ToolList{{Selector: "tool:x_github_get_issue", Setting: "Allow"}}})
	require.NoError(t, err)

	// A carried-over Allow stays; a new one over an Ask is refused.
	_, err = a.svc.UpdateAs(dana(t), "update_agent_access", Update{Name: "alert-review", Tools: &ToolList{{Selector: "server:github"}, {Selector: "tool:x_github_merge_pull_request", Setting: "Allow"}, {Selector: "server:pagerduty"}}})
	require.NoError(t, err)
	_, err = a.svc.UpdateAs(dana(t), "update_agent_access", Update{Name: "alert-review", Tools: &ToolList{{Selector: "server:github"}, {Selector: "tool:x_github_merge_pull_request", Setting: "Allow"}, {Selector: "server:pagerduty", Setting: "Allow"}}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "update_agent_access does not grant exceptions: server:pagerduty: Allow selects tools whose connector pagerduty asks")
}

func TestAnAllowNeedsMusterToBeChecked(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.svc.cfg.Approvals = nil
	_, err := a.svc.Create(dana(t), Spec{Name: "fresh", ModelConfig: "default-model-config", Tools: ToolList{{Selector: "server:github", Setting: "Allow"}}})
	require.ErrorIs(t, err, ErrApprovalUnavailable)
	// Without an Allow muster is never asked.
	_, err = a.svc.Create(dana(t), Spec{Name: "fresh", ModelConfig: "default-model-config", Tools: ToolList{{Selector: "server:github", Setting: "Ask"}}})
	require.NoError(t, err)
}

func TestSetAgentApprovalGrantsForAConnectorAdmin(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.admins["muster"] = true
	res, err := a.svc.SetApproval(dana(t), SetApproval{Name: "alert-review", Settings: map[string]string{"tool:x_github_merge_pull_request": "Allow"}})
	require.NoError(t, err)
	require.Len(t, res.Grants, 1)
	g := res.Grants[0]
	assert.Equal(t, "tool:x_github_merge_pull_request", g.Entry)
	assert.Equal(t, "github", g.Server)
	assert.Equal(t, "uid-github", g.ConnectorUID)
	assert.Equal(t, "dana@example.com", g.GrantedBy)
	assert.Equal(t, int64(7), g.Generation, "the generation of the release the save left (unchanged values: no write)")
	assert.Equal(t, []approvalgrants.Grant{{Entry: g.Entry, Server: "github", ConnectorNamespace: "muster", ConnectorUID: "uid-github", GrantedBy: "dana@example.com", GrantedAt: g.GrantedAt, Generation: 7, Tools: []string{"merge_pull_request"}}}, a.stored(t))
	assert.Equal(t, []string{"merge_pull_request"}, g.Tools, "the grant names the tools it covers")
	assert.Contains(t, a.logs.String(), `"msg":"approval grant recorded"`)

	// get_agent and list_agents report it.
	agent, err := a.svc.Get(dana(t), Location{}, "alert-review")
	require.NoError(t, err)
	require.Len(t, agent.Grants, 1)
	assert.True(t, agent.ApprovalSettings)
	list, err := a.svc.List(dana(t), Location{}, nil)
	require.NoError(t, err)
	for _, l := range list {
		if l.Name == "alert-review" {
			assert.Len(t, l.Summary().Grants, 1)
		}
	}
}

func TestSetAgentApprovalRefusesWhoIsNoConnectorAdmin(t *testing.T) {
	a := newApprovalFixture(t, nil)
	_, err := a.svc.SetApproval(dana(t), SetApproval{Name: "alert-review", Settings: map[string]string{"tool:x_github_merge_pull_request": "Allow"}})
	require.ErrorIs(t, err, ErrForbidden)
	assert.Contains(t, err.Error(), "needs a connector admin of it (update mcpservers in namespace muster)")
	assert.Empty(t, a.stored(t))

	_, err = a.svc.SetApproval(dana(t), SetApproval{Name: "alert-review", Settings: map[string]string{"server:pagerduty": "Allow"}})
	require.ErrorIs(t, err, ErrInvalid, "an entry the agent does not list")
	_, err = a.svc.SetApproval(dana(t), SetApproval{Name: "alert-review"})
	require.ErrorIs(t, err, ErrInvalid)
	// Tightening needs no admin.
	_, err = a.svc.SetApproval(dana(t), SetApproval{Name: "alert-review", Settings: map[string]string{"server:github": "Ask"}, Default: new("Block")})
	require.NoError(t, err)
	assert.Equal(t, "Block", a.values(t)[DefaultValuesKey])
}

func TestSetAgentApprovalRecordsNoGrantInGitOrOnValuesFrom(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.admins["muster"] = true
	_, err := a.svc.SetApproval(dana(t), SetApproval{Name: "alert-review", Settings: map[string]string{"tool:x_github_merge_pull_request": "Allow"}, WriteOptions: WriteOptions{Mode: ModeCommit}})
	require.Error(t, err)

	hr, err := a.dyn.Resource(hrGVR).Namespace("kagent").Get(t.Context(), "alert-review", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedSlice(hr.Object, []any{map[string]any{"kind": "ConfigMap", "name": "extra"}}, "spec", "valuesFrom"))
	_, err = a.dyn.Resource(hrGVR).Namespace("kagent").Update(t.Context(), hr, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = a.svc.SetApproval(dana(t), SetApproval{Name: "alert-review", Settings: map[string]string{"tool:x_github_merge_pull_request": "Allow"}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "valuesFrom")
}

// grantMerge stores a grant of merge at generation 7.
func (a *approvalFixture) grantMerge() {
	a.grants.Put(alertAgent, approvalgrants.Grant{Entry: "tool:x_github_merge_pull_request", Server: "github", ConnectorNamespace: "muster", ConnectorUID: "uid-github", GrantedBy: "dana@example.com", Generation: 7})
}

func TestSavingDropsTheGrantsTheSaverIsNoAdminOf(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grantMerge()
	res, err := a.svc.UpdateAs(asPerson(t.Context(), "bob@example.com"), "update_agent", Update{Name: "alert-review", Description: new("reviews alerts")})
	require.NoError(t, err)
	require.Len(t, res.Dropped, 1)
	assert.Equal(t, "saved by bob@example.com, not a connector admin of github", res.Dropped[0].Reason)
	assert.Empty(t, a.stored(t))
	require.Len(t, a.bindings.drops, 1, "running sessions lose the mark")
	assert.Equal(t, grantDrop{Agent: "kagent/alert-review", Keep: []muster.Grant{}}, a.bindings.drops[0])
	assert.Contains(t, a.logs.String(), `"msg":"approval grant dropped"`)
	assert.Contains(t, a.logs.String(), `"msg":"approval grants removed from running sessions"`)
}

func TestSavingKeepsTheGrantsOfAConnectorAdmin(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grantMerge()
	a.admins["muster"] = true
	res, err := a.svc.UpdateAs(asPerson(t.Context(), "erin@example.com"), "update_agent", Update{Name: "alert-review", Description: new("reviews alerts")})
	require.NoError(t, err)
	assert.Empty(t, res.Dropped)
	stored := a.stored(t)
	require.Len(t, stored, 1)
	assert.Equal(t, "erin@example.com", stored[0].GrantedBy, "re-recorded with the saver")
	// Every save of an agent with grants tells muster what is left
	// (idempotent): the grant stays marked.
	require.Len(t, a.bindings.drops, 1)
	require.Len(t, a.bindings.drops[0].Keep, 1)
	assert.Equal(t, "tool:x_github_merge_pull_request", a.bindings.drops[0].Keep[0].Entry)
	require.NotNil(t, res.SessionsUpdated)
	assert.True(t, *res.SessionsUpdated)
}

func TestAnAgentsSaveKeepsNoGrant(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grantMerge()
	a.admins["muster"] = true
	ctx := identity.ContextWithAgentCaller(dana(t))
	res, err := a.svc.UpdateAs(ctx, "update_agent", Update{Name: "alert-review", Description: new("reviews alerts")})
	require.NoError(t, err)
	require.Len(t, res.Dropped, 1)
	assert.Equal(t, "saved by an agent", res.Dropped[0].Reason)
	assert.Contains(t, a.logs.String(), `"callerKind":"agent"`)

	// On set_agent_approval an agent's call (asked of the person by muster)
	// records what it names.
	res2, err := a.svc.SetApproval(ctx, SetApproval{Name: "alert-review", Settings: map[string]string{"tool:x_github_merge_pull_request": "Allow"}})
	require.NoError(t, err)
	require.Len(t, res2.Grants, 1)
}

func TestRemovingAnEntryDropsItsGrant(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grantMerge()
	a.admins["muster"] = true
	res, err := a.svc.UpdateAs(dana(t), "update_agent_access", Update{Name: "alert-review", Tools: &ToolList{{Selector: "server:github"}}})
	require.NoError(t, err)
	require.Len(t, res.Dropped, 1)
	assert.Equal(t, "entry removed or no longer Allow", res.Dropped[0].Reason)
	assert.Contains(t, a.logs.String(), `"msg":"agent tools changed"`)
}

func TestACommitDropsEveryGrant(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grantMerge()
	a.admins["muster"] = true
	upd := Update{Name: "alert-review", Description: new("x"), tool: "update_agent"}
	_, _, _, err := a.svc.settleGrants(dana(t), grantSave{tool: upd.tool, agent: alertAgent, generation: 8, tools: ToolList{{Selector: "tool:x_github_merge_pull_request", Setting: "Allow"}}, commit: true})
	require.NoError(t, err)
	assert.Empty(t, a.stored(t))
}

func TestDeleteRemovesTheGrants(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grantMerge()
	_, err := a.svc.Delete(dana(t), Location{}, "alert-review", false, WriteOptions{})
	require.NoError(t, err)
	assert.Empty(t, a.stored(t))
}

// startApproval starts a session of alert-review as dana, kagent admitting
// it to muster's actor bearer.
func (a *approvalFixture) start(t *testing.T) muster.Binding {
	t.Helper()
	svc, ctrl, _ := newBoundSessionFixtureWithService(t, a.fixture)
	svc.cfg.Bindings = a.bindings
	ctrl.ActorJWTProviders = []*apiv1alpha1.SessionProvider{musterProvider()}
	_, err := svc.StartSession(dana(t), StartSession{Name: "alert-review", Message: "hello"})
	require.NoError(t, err)
	require.NotEmpty(t, a.bindings.Bound)
	return a.bindings.Bound[len(a.bindings.Bound)-1].Binding
}

func TestASessionIsBoundWithTheAgentItsSettingsAndRecordedGrants(t *testing.T) {
	a := newApprovalFixture(t, map[string]any{
		"agent": map[string]any{"name": "alert-review"}, "modelConfig": map[string]any{"name": "default-model-config"},
		"tools":   []any{map[string]any{"server:github": "Ask"}, map[string]any{"tool:x_github_merge_pull_request": "Allow"}},
		"default": "Block",
	})
	a.grantMerge()
	b := a.start(t)
	assert.Equal(t, "kagent/alert-review", b.Agent)
	require.NotNil(t, b.Approval)
	assert.Equal(t, []muster.ApprovalEntry{{Entry: "server:github", Setting: "Ask"}, {Entry: "tool:x_github_merge_pull_request", Setting: "Allow"}}, b.Approval.Entries)
	assert.Equal(t, "Block", b.Approval.Default)
	assert.Equal(t, []muster.Grant{{Entry: "tool:x_github_merge_pull_request", Server: "github", UID: "uid-github"}}, b.Approval.Grants)
}

func TestABindCarriesNoGrantWrittenBeforeAValuesChange(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grants.Put(alertAgent, approvalgrants.Grant{Entry: "tool:x_github_merge_pull_request", Server: "github", ConnectorNamespace: "muster", ConnectorUID: "uid-github", Generation: 6})
	b := a.start(t)
	require.NotNil(t, b.Approval)
	assert.Empty(t, b.Approval.Grants, "generation 6 is not the release's 7: values changed past agent-manager")
	assert.Empty(t, a.stored(t))
	assert.Contains(t, a.logs.String(), "values changed outside agent-manager (release generation 7, grant written at 6)")
}

func TestASessionStartedDuringASaveLosesTheDroppedMark(t *testing.T) {
	a := newApprovalFixture(t, nil)
	a.grantMerge()
	// A save on another replica drops the grant between this start's read
	// and its bind.
	a.bindings.onBind = func() { _ = a.grants.Delete(context.Background(), alertUID) }
	b := a.start(t)
	require.Len(t, b.Approval.Grants, 1, "bound with the grant read before")
	require.Len(t, a.bindings.drops, 1, "re-read after the bind removes it again")
	assert.Equal(t, "kagent/alert-review", a.bindings.drops[0].Agent)
	assert.Empty(t, a.bindings.drops[0].Keep)
}

func TestListApprovalGrants(t *testing.T) {
	a := newApprovalFixture(t, map[string]any{
		"agent": map[string]any{"name": "alert-review", "displayName": "Alert Review"}, "modelConfig": map[string]any{"name": "default-model-config"},
		"tools": []any{"server:github", map[string]any{"tool:x_github_merge_pull_request": "Allow"}},
	})
	a.grantMerge()
	got, err := a.svc.ListApprovalGrants(dana(t), "", "github")
	require.NoError(t, err)
	require.Len(t, got.Grants, 1, "across every managed namespace")
	assert.Equal(t, &alertAgent, got.Grants[0].Agent)
	assert.Equal(t, "Alert Review", got.Grants[0].AgentDisplayName)
	assert.Empty(t, got.Unreadable)
	got, err = a.svc.ListApprovalGrants(dana(t), "kagent", "pagerduty")
	require.NoError(t, err)
	assert.Empty(t, got.Grants)

	a.dyn.PrependReactor("list", "helmreleases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, forbiddenErr("helmreleases")
	})
	_, err = a.svc.ListApprovalGrants(dana(t), "kagent", "")
	require.ErrorIs(t, err, ErrForbidden, "a namespace named and unreadable is refused")
	got, err = a.svc.ListApprovalGrants(dana(t), "", "")
	require.NoError(t, err, "without a namespace an unreadable one is skipped")
	assert.Empty(t, got.Grants)
	assert.Contains(t, got.Unreadable, "kagent")
}

// approvalEnforced says whether an agent's settings are bound to its
// sessions: not without the sender, not on a release with valuesFrom.
func TestApprovalEnforced(t *testing.T) {
	a := newApprovalFixture(t, nil)
	agent, err := a.svc.Get(dana(t), Location{}, "alert-review")
	require.NoError(t, err)
	assert.Equal(t, new(true), agent.ApprovalEnforced)
	assert.Equal(t, new(true), agent.Summary().ApprovalEnforced)

	hr, err := a.dyn.Resource(hrGVR).Namespace("kagent").Get(t.Context(), "alert-review", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedSlice(hr.Object, []any{map[string]any{"kind": "ConfigMap", "name": "more"}}, "spec", "valuesFrom"))
	_, err = a.dyn.Resource(hrGVR).Namespace("kagent").Update(t.Context(), hr, metav1.UpdateOptions{})
	require.NoError(t, err)
	agent, err = a.svc.Get(dana(t), Location{}, "alert-review")
	require.NoError(t, err)
	assert.Equal(t, new(false), agent.ApprovalEnforced, "valuesFrom")

	b := newApprovalFixture(t, nil)
	b.bindings.noSender = true
	list, err := b.svc.List(dana(t), Location{}, nil)
	require.NoError(t, err)
	for _, l := range list {
		assert.Equal(t, new(false), l.ApprovalEnforced, "%s: this agent-manager binds no settings", l.Name)
	}
	raw, _ := json.Marshal(list[0].Summary())
	assert.Contains(t, string(raw), `"approvalEnforced":false`, "false is reported, not omitted")
}

// newBoundSessionFixtureWithService gives f's service a kagent controller.
func newBoundSessionFixtureWithService(t *testing.T, f *fixture) (*Service, *kagenttest.Controller, struct{}) {
	t.Helper()
	ctrl, conn := kagenttest.New(t)
	f.svc.cfg.Sessions = kagent.NewClient(conn)
	return f.svc, ctrl, struct{}{}
}

func forbiddenErr(resource string) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", errors.New("denied"))
}

// A Session of an agent on muster.requireApproval: true is bound with
// default: Ask: when agent chart 3.0 drops kagent's flag the
// Session's binding already asks, and a binding written with empty settings
// would make muster ignore the agent's X-Muster-Tool-Approval header.
func TestASessionOfARequireApprovalAgentIsBoundAskingBeforeEveryCall(t *testing.T) {
	for name, tc := range map[string]struct {
		values map[string]any
		want   string
	}{
		"the binding renders": {map[string]any{"agent": map[string]any{"name": "alert-review"}, "modelConfig": map[string]any{"name": "m"},
			"muster": map[string]any{"requireApproval": true}, "toolset": []any{"server:github"}}, "Ask"},
		"requireApproval false": {map[string]any{"agent": map[string]any{"name": "alert-review"}, "modelConfig": map[string]any{"name": "m"},
			"muster": map[string]any{"requireApproval": false}, "toolset": []any{"server:github"}}, ""},
		"muster off: the flag sat on no binding": {map[string]any{"agent": map[string]any{"name": "alert-review"}, "modelConfig": map[string]any{"name": "m"},
			"muster": map[string]any{"requireApproval": true, "enabled": false}, "toolset": []any{"server:github"}}, ""},
		"chat-only: the flag sat on no binding": {map[string]any{"agent": map[string]any{"name": "alert-review"}, "modelConfig": map[string]any{"name": "m"},
			"muster": map[string]any{"requireApproval": true}, "toolset": []any{"preset:none"}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			a := newApprovalFixture(t, tc.values)
			b := a.start(t)
			require.NotNil(t, b.Approval)
			assert.Equal(t, tc.want, b.Approval.Default)
		})
	}
}
