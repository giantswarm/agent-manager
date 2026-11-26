package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/approvalgrants"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/muster"
)

// Tool approval per agent. An entry's
// Allow over a connector's Ask holds only with a grant a connector admin
// saved through set_agent_approval; agent-manager keeps the grants in their
// own records (internal/approvalgrants) and marks them in each Session's
// muster binding. Whether an entry is an Allow over an Ask is muster's to
// say: agent-manager asks filter_tools with the agent's list (agentTools),
// as the caller.

// MusterServersGroup and MCPServersResource are what a connector admin may
// update: muster's MCPServers in the connector's namespace.
const (
	MusterServersGroup = "muster.giantswarm.io"
	MCPServersResource = "mcpservers"
)

// ApprovalReporter is muster's filter_tools with agentTools, as the caller.
type ApprovalReporter interface {
	// AgentTools lists the tools the list selects, each with the entry that
	// decides its agent setting, the connector's own setting and its
	// connectors.
	AgentTools(ctx context.Context, token string, q AgentToolsQuery) ([]ReportedTool, error)
}

// AgentToolsQuery is the agent's list as muster evaluates it.
type AgentToolsQuery struct {
	Tools   ToolList
	Default string
}

// ReportedTool is one tool as muster reports it for an agent's list.
type ReportedTool struct {
	Name string
	// Entry is the agent entry that decides the tool's agent setting
	// (DefaultEntry for the default); "" for none.
	Entry string
	// Admin is the connector's own setting (allow, ask, block); "" when no
	// entry decides.
	Admin string
	// Connectors are the tool's server, or every member of a family tool.
	Connectors []Connector
}

// Connector is an MCPServer as muster reports it. Tool is the server's own
// name of the reported tool on it; Tools collects, for a grant, the server's
// own names of every tool the grant covers there.
type Connector struct {
	Server    string   `json:"server"`
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	UID       string   `json:"uid"`
	Tool      string   `json:"tool,omitempty"`
	Tools     []string `json:"-"`
}

// GrantDropper is muster's binding endpoint that removes grant marks from an
// agent's running Sessions (PATCH /session-bindings/grants).
type GrantDropper interface {
	DropGrants(ctx context.Context, token, agent string, keep []muster.Grant) (muster.DropResult, error)
}

// ApprovalSender reports whether binds carry agent-manager's sender token,
// so that muster takes approval settings from them.
type ApprovalSender interface {
	SendsApproval() bool
}

// GrantView is a grant as the tools report it.
type GrantView struct {
	Agent              *approvalgrants.Agent `json:"agent,omitempty"`
	Entry              string                `json:"entry"`
	Server             string                `json:"server"`
	ConnectorNamespace string                `json:"namespace"`
	ConnectorUID       string                `json:"connectorUid"`
	GrantedBy          string                `json:"grantedBy"`
	GrantedAt          time.Time             `json:"grantedAt"`
	Generation         int64                 `json:"generation"`
	// Tools are the server's own names of the tools the grant covers.
	Tools []string `json:"tools"`
}

// DroppedGrant is a grant a save removed.
type DroppedGrant struct {
	Entry  string `json:"entry"`
	Server string `json:"server"`
	Reason string `json:"reason"`

	connectorUID string
}

func grantView(g approvalgrants.Grant) GrantView {
	tools := g.Tools
	if tools == nil {
		tools = []string{}
	}
	return GrantView{Entry: g.Entry, Server: g.Server, ConnectorNamespace: g.ConnectorNamespace, ConnectorUID: g.ConnectorUID, GrantedBy: g.GrantedBy, GrantedAt: g.GrantedAt, Generation: g.Generation, Tools: tools}
}

// The states of Agent.ApprovalEnforcement.
const (
	EnforcementEnforced    = "enforced"
	EnforcementNotEnforced = "not-enforced"
	EnforcementPending     = "pending"
)

// enforcement says whether an agent's settings are bound to its sessions: not without the sender, a release, or with spec.valuesFrom, nor on
// a release without `tools` that runs another chart than the one judged by
// (an older chart); pending until the release ran its current
// generation; enforced otherwise.
func enforcement(a *Agent, sends bool, schemaVersion string) string {
	switch {
	case !sends || !a.releaseFound || a.releaseValuesFrom:
		return EnforcementNotEnforced
	case !a.releaseRan:
		return EnforcementPending
	case !a.releaseHasTools && schemaVersion != "" && strings.TrimPrefix(a.releaseChart, "v") != strings.TrimPrefix(schemaVersion, "v"):
		return EnforcementNotEnforced
	}
	return EnforcementEnforced
}

// allowEntries are the entries (and DefaultEntry) whose setting is Allow.
func allowEntries(l ToolList, def string) map[string]bool {
	out := map[string]bool{}
	for _, e := range l {
		if e.Setting == SettingAllow {
			out[e.Selector] = true
		}
	}
	if def == SettingAllow {
		out[DefaultEntry] = true
	}
	return out
}

// grantNeeds asks muster which of the given entries (each an Allow of the
// list) select a tool whose connector asks, and returns their connectors per
// entry. Entries that need none are absent: their Allow changes nothing at
// runtime. Without such entries muster is not asked.
func (s *Service) grantNeeds(ctx context.Context, l ToolList, def string, entries map[string]bool) (map[string][]Connector, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	if s.cfg.Approvals == nil {
		return nil, fmt.Errorf("%w: an Allow in the agent's tool list needs muster's word on what it selects, and this agent-manager has no muster MCP endpoint (--muster-mcp-url)", ErrApprovalUnavailable)
	}
	token, ok := identity.TokenFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("%w: an Allow in the agent's tool list is checked against muster as you, and the request carries no identity token", ErrApprovalUnavailable)
	}
	tools, err := s.cfg.Approvals.AgentTools(ctx, token, AgentToolsQuery{Tools: l, Default: def})
	if err != nil {
		if errors.Is(err, ErrApprovalUnavailable) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: muster could not say what the agent's tool list selects: %v", ErrApprovalUnavailable, err)
	}
	needs := map[string][]Connector{}
	for _, t := range tools {
		if !entries[t.Entry] || t.Admin != "ask" {
			continue
		}
		for _, c := range t.Connectors {
			i := slices.IndexFunc(needs[t.Entry], func(have Connector) bool { return have.Server == c.Server })
			if i < 0 {
				c.Tools = nil
				needs[t.Entry] = append(needs[t.Entry], c)
				i = len(needs[t.Entry]) - 1
			}
			if c.Tool != "" && !slices.Contains(needs[t.Entry][i].Tools, c.Tool) {
				needs[t.Entry][i].Tools = append(needs[t.Entry][i].Tools, c.Tool)
			}
		}
	}
	for entry := range needs {
		sort.Slice(needs[entry], func(i, j int) bool { return needs[entry][i].Server < needs[entry][j].Server })
		for i := range needs[entry] {
			slices.Sort(needs[entry][i].Tools)
		}
	}
	return needs, nil
}

// refuseAllowOverAsk is create_agent's and update_agent_access's check: they
// never grant. A default Allow is refused outright; an entry that is new or
// whose setting changed to Allow is refused when it selects a tool whose
// connector asks. An entry carried over unchanged is kept (the save drops
// its grant unless the saver is a connector admin).
func (s *Service) refuseAllowOverAsk(ctx context.Context, tool string, before, after ToolList, def string, defChanged bool) error {
	if defChanged && def == SettingAllow {
		return invalidf("%s does not take default: Allow: an allowing default is granted only by a connector admin with set_agent_approval", tool)
	}
	changed := map[string]bool{}
	for _, sel := range changedEntries(before, after) {
		if setting, _ := after.Setting(sel); setting == SettingAllow {
			changed[sel] = true
		}
	}
	needs, err := s.grantNeeds(ctx, after, def, changed)
	if err != nil {
		return err
	}
	for _, entry := range sortedKeys(needs) {
		return invalidf("%s does not grant exceptions: %s: Allow selects tools whose connector %s asks first; save the entry without a setting (or with Ask), then a connector admin of %s grants it with set_agent_approval", tool, entry, connectorNames(needs[entry]), connectorNames(needs[entry]))
	}
	return nil
}

func connectorNames(cs []Connector) string {
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		names = append(names, c.Server)
	}
	return strings.Join(names, ", ")
}

// isConnectorAdmin asks, as the caller, whether they may update MCPServers in
// namespace: the connector-admin check. It is answered only where agent-
// manager talks to Kubernetes as the caller (downstream OAuth): otherwise a
// review would describe agent-manager's own ServiceAccount, and wherever that
// is bound widely every caller would count as a connector admin,
// so no one is one.
func (s *Service) isConnectorAdmin(ctx context.Context, namespace string) (bool, error) {
	if s.kube.Identity() != kube.IdentityCaller {
		return false, nil
	}
	client, err := s.kube.Client(ctx)
	if err != nil {
		return false, err
	}
	review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Verb: "update", Group: MusterServersGroup, Resource: MCPServersResource},
	}}
	res, err := client.Typed().AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("review whether you may update mcpservers in namespace %s: %w", namespace, err)
	}
	return res.Status.Allowed, nil
}

// requireListReleases refuses a caller who may not list HelmReleases in ns
// (a SelfSubjectAccessReview as the caller).
func (s *Service) requireListReleases(ctx context.Context, ns string) error {
	client, err := s.kube.Client(ctx)
	if err != nil {
		return err
	}
	review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: ns, Verb: "list", Group: s.helmReleaseGVR().Group, Resource: "helmreleases"},
	}}
	res, err := client.Typed().AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("review whether you may list helmreleases in namespace %s: %w", ns, err)
	}
	if !res.Status.Allowed {
		return forbiddenf("you may not list helmreleases in namespace %s, so its agents' approval grants are not shown", ns)
	}
	return nil
}

// adminMemo reviews each namespace once per request.
type adminMemo struct {
	s    *Service
	seen map[string]bool
}

func (m *adminMemo) admin(ctx context.Context, namespace string) (bool, error) {
	if m.seen == nil {
		m.seen = map[string]bool{}
	}
	if v, ok := m.seen[namespace]; ok {
		return v, nil
	}
	v, err := m.s.isConnectorAdmin(ctx, namespace)
	if err != nil {
		return false, err
	}
	m.seen[namespace] = v
	return v, nil
}

// grantSave is one save as the grants see it.
type grantSave struct {
	tool       string
	agent      approvalgrants.Agent
	generation int64
	tools      ToolList
	def        string
	// commit: the save went to git; no grant survives it.
	commit bool
	// granted are the grants this save writes (set_agent_approval's named
	// entries), per entry, after their check.
	granted map[string][]Connector
}

// settleGrants applies a save to the agent's grants: a grant whose entry is
// no longer an Allow, of a save to git, or of an agent's save is dropped; one
// whose connector the saver is no admin of is dropped; the rest are kept,
// re-recorded with the saver and the save's generation; the save's own
// grants are written. Every live save of an agent that has or had grants
// tells muster the grants left (idempotent), so a drop an earlier save could
// not send is sent again; the record carries PendingSessionSync until muster
// confirmed it (session_sync.go).
func (s *Service) settleGrants(ctx context.Context, save grantSave) ([]GrantView, []DroppedGrant, SessionSync, error) {
	if s.cfg.Grants == nil {
		if len(save.granted) > 0 {
			return nil, nil, SessionSync{}, fmt.Errorf("%w: this agent-manager keeps no approval grants (no grants store)", ErrUnsupported)
		}
		return nil, nil, SessionSync{}, nil
	}
	var kept []GrantView
	var dropped []DroppedGrant
	var written approvalgrants.Record
	var notify bool
	for attempt := 0; ; attempt++ {
		rec, err := s.cfg.Grants.Get(ctx, save.agent)
		if err != nil {
			return nil, nil, SessionSync{}, err
		}
		next, k, d, err := s.nextGrants(ctx, save, rec.Grants)
		if err != nil {
			return nil, nil, SessionSync{}, err
		}
		// Muster is told after every save of an agent with a grant history
		// (a record exists): the record is marked until it confirms.
		notify = rec.Version != "" || len(d) > 0
		rec.Grants = next
		rec.PendingSessionSync = notify
		err = s.cfg.Grants.Replace(ctx, rec)
		if errors.Is(err, approvalgrants.ErrConflict) && attempt < 3 {
			continue
		}
		if err != nil {
			return nil, nil, SessionSync{}, err
		}
		kept, dropped, written = k, d, rec
		break
	}
	for _, g := range kept {
		s.log.InfoContext(ctx, "approval grant recorded", append(s.approvalLogAttrs(ctx, save.tool, save.agent.Key()),
			"entry", g.Entry, "server", g.Server, "connectorUid", g.ConnectorUID, "generation", g.Generation, "tools", g.Tools, "kept", !isNew(save, g))...)
	}
	for _, d := range dropped {
		s.log.InfoContext(ctx, "approval grant dropped", append(s.approvalLogAttrs(ctx, save.tool, save.agent.Key()),
			"entry", d.Entry, "server", d.Server, "connectorUid", d.connectorUID, "reason", d.Reason)...)
	}
	var sync SessionSync
	if notify {
		sync = s.syncSessions(ctx, save.tool, written, false)
	}
	return kept, dropped, sync, nil
}

// isNew reports whether g is one of the save's own grants.
func isNew(save grantSave, g GrantView) bool {
	return slices.ContainsFunc(save.granted[g.Entry], func(c Connector) bool { return c.Server == g.Server })
}

// nextGrants is settleGrants' decision over the stored grants.
func (s *Service) nextGrants(ctx context.Context, save grantSave, current []approvalgrants.Grant) ([]approvalgrants.Grant, []GrantView, []DroppedGrant, error) {
	saver := identity.VerifiedCaller(ctx)
	now := time.Now().UTC().Truncate(time.Second)
	allow := allowEntries(save.tools, save.def)
	memo := &adminMemo{s: s}
	var next []approvalgrants.Grant
	var dropped []DroppedGrant
	for _, g := range current {
		if regrant, regranted := save.granted[g.Entry]; regranted {
			// Replaced by this save's own grants below; a connector the
			// entry no longer needs a grant on goes, said so.
			if !slices.ContainsFunc(regrant, func(c Connector) bool { return c.Server == g.Server }) {
				dropped = append(dropped, DroppedGrant{Entry: g.Entry, Server: g.Server, Reason: "regranted without this connector: the entry selects no asking tool of it any more", connectorUID: g.ConnectorUID})
			}
			continue
		}
		reason := ""
		switch {
		case !allow[g.Entry]:
			reason = "entry removed or no longer Allow"
		case save.commit:
			reason = "release managed in git (mode commit)"
		case identity.IsAgentCaller(ctx):
			reason = "saved by an agent"
		default:
			admin, err := memo.admin(ctx, g.ConnectorNamespace)
			if err != nil {
				return nil, nil, nil, err
			}
			if !admin {
				reason = fmt.Sprintf("saved by %s, not a connector admin of %s", orDefault(identity.Caller(ctx), "an unknown caller"), g.Server)
			}
		}
		if reason != "" {
			dropped = append(dropped, DroppedGrant{Entry: g.Entry, Server: g.Server, Reason: reason, connectorUID: g.ConnectorUID})
			continue
		}
		g.GrantedBy, g.GrantedAt, g.Generation = saver, now, save.generation
		next = append(next, g)
	}
	for _, entry := range sortedKeys(save.granted) {
		for _, c := range save.granted[entry] {
			next = append(next, approvalgrants.Grant{Entry: entry, Server: c.Server, ConnectorNamespace: c.Namespace, ConnectorUID: c.UID, GrantedBy: saver, GrantedAt: now, Generation: save.generation, Tools: slices.Clone(c.Tools)})
		}
	}
	views := make([]GrantView, 0, len(next))
	for _, g := range next {
		views = append(views, grantView(g))
	}
	return next, views, dropped, nil
}

// approvalLogAttrs are the fields every approval log line carries.
func (s *Service) approvalLogAttrs(ctx context.Context, tool, agent string) []any {
	return []any{"agent", agent, "tool", tool, "savedBy", identity.Caller(ctx), "callerKind", identity.CallerKind(ctx)}
}

// liveGrants are the agent's grants that hold for generation, the release's
// current one. A grant written at a lower generation was written before the
// release changed past agent-manager (any change of its spec: values, chart
// version, suspend) and is dropped, from the records and the Sessions. One
// written at a higher generation belongs to a save that ran after the
// release was read: it is left alone and not bound now,
// never dropped.
func (s *Service) liveGrants(ctx context.Context, tool string, agent approvalgrants.Agent, generation int64) ([]approvalgrants.Grant, error) {
	if s.cfg.Grants == nil || agent.UID == "" {
		return nil, nil
	}
	for attempt := 0; ; attempt++ {
		rec, err := s.cfg.Grants.Get(ctx, agent)
		if err != nil {
			return nil, err
		}
		var live, stale, newer []approvalgrants.Grant
		for _, g := range rec.Grants {
			switch {
			case g.Generation == generation:
				live = append(live, g)
			case g.Generation < generation:
				stale = append(stale, g)
			default:
				newer = append(newer, g)
			}
		}
		if len(stale) == 0 {
			if rec.PendingSessionSync {
				s.syncSessions(ctx, tool, rec, false)
			}
			return live, nil
		}
		rec.Grants = append(slices.Clone(live), newer...)
		rec.PendingSessionSync = true
		if err := s.cfg.Grants.Replace(ctx, rec); err != nil {
			if errors.Is(err, approvalgrants.ErrConflict) && attempt < 3 {
				continue
			}
			return nil, err
		}
		for _, g := range stale {
			s.log.InfoContext(ctx, "approval grant dropped", append(s.approvalLogAttrs(ctx, tool, agent.Key()),
				"entry", g.Entry, "server", g.Server, "connectorUid", g.ConnectorUID,
				"reason", fmt.Sprintf("values changed outside agent-manager (release generation %d, grant written at %d)", generation, g.Generation))...)
		}
		s.syncSessions(ctx, tool, rec, false)
		return live, nil
	}
}

// attachGrants reports each agent's live grants (those written at its
// release's current generation) and whether the chart takes approval
// settings. A store that cannot be read leaves the grants out, logged: the
// listing is not refused for it.
func (s *Service) attachGrants(ctx context.Context, namespace string, list []Agent) {
	// Settings are reported as taken only where the chart takes them and
	// this installation binds them to sessions.
	sends := s.sendsApproval()
	takes := s.chartTakesTools(ctx) && sends
	schemaVersion := s.chart.Info(ctx).SchemaVersion
	for i := range list {
		list[i].ApprovalSettings = takes
		list[i].ApprovalEnforcement = enforcement(&list[i], sends, schemaVersion)
		if list[i].ApprovalEnforcement != EnforcementPending {
			enforced := list[i].ApprovalEnforcement == EnforcementEnforced
			list[i].ApprovalEnforced = &enforced
		}
	}
	if s.cfg.Grants == nil {
		return
	}
	var records []approvalgrants.Record
	if len(list) == 1 && list[0].releaseUID != "" {
		// One agent (get_agent): its record by name, not the namespace's.
		rec, err := s.cfg.Grants.Get(ctx, approvalgrants.Agent{UID: list[0].releaseUID, Namespace: namespace, Name: list[0].Name})
		if err != nil {
			s.log.WarnContext(ctx, "approval grants unreadable; the agent is reported without them", "agent", namespace+"/"+list[0].Name, "error", err.Error())
			return
		}
		records = []approvalgrants.Record{rec}
	} else {
		var err error
		if records, err = s.cfg.Grants.ListByNamespace(ctx, namespace); err != nil {
			s.log.WarnContext(ctx, "approval grants unreadable; agents listed without them", "namespace", namespace, "error", err.Error())
			return
		}
	}
	byUID := map[string][]approvalgrants.Grant{}
	live := map[string]bool{}
	for _, r := range records {
		byUID[r.Agent.UID] = r.Grants
	}
	for i := range list {
		if list[i].releaseUID != "" {
			live[list[i].releaseUID] = true
		}
	}
	// A read carries a person's token: a sync an earlier write could not
	// send goes now. A record of an agent not in a namespace listing was
	// deleted; for get_agent only the agent's own record is read.
	if len(list) == 1 {
		live = map[string]bool{list[0].releaseUID: true}
	}
	s.resendPendingIn(ctx, "read", records, live)
	for i := range list {
		a := &list[i]
		if a.releaseUID == "" {
			continue
		}
		for _, g := range byUID[a.releaseUID] {
			if g.Generation == a.releaseGeneration {
				a.Grants = append(a.Grants, grantView(g))
			}
		}
	}
}

// settleSave is settleGrants for an update's save: the agent's grants
// follow the values the save wrote (after) and who saved them; a save to git
// keeps none. The result carries the grants left and the ones dropped.
func (s *Service) settleSave(ctx context.Context, st *site, upd Update, hr *unstructured.Unstructured, after map[string]any, res *UpdateResult, commit bool) error {
	list, def := toolListFromValues(after)
	kept, dropped, sync, err := s.settleGrants(ctx, grantSave{
		tool:       upd.tool,
		agent:      approvalgrants.Agent{UID: string(hr.GetUID()), Namespace: st.ns(), Name: upd.Name},
		generation: hr.GetGeneration(),
		tools:      list,
		def:        def,
		commit:     commit,
		granted:    upd.granted,
	})
	if err != nil {
		return fmt.Errorf("the agent was saved, but its approval grants could not be settled: %w", err)
	}
	res.Grants, res.Dropped, res.SessionSync = kept, dropped, sync
	return nil
}

// accessValues are the fields of an agent's values that widen its reach, as
// the access log reports them.
func accessValues(values map[string]any) map[string]any {
	agentBlock, _ := values["agent"].(map[string]any)
	return map[string]any{
		"skills":            values["skills"],
		"plugins":           values["plugins"],
		"egress":            agentBlock["egress"],
		"egressPolicyRefs":  agentBlock["egressPolicyRefs"],
		"connectors":        connectorsFromValues(values),
		"harness":           agentBlock["harness"],
		"gitAuthSecretName": values[SkillsGitAuthValuesKey],
		"defaultWorkspace":  defaultWorkspaceOf(values),
		"defaultToolchains": defaultToolchainsOf(values),
	}
}

// logAccessChanges logs a save's changes to the tool list (old -> new) and to
// each access field, with who saved it; before is nil on a create.
func (s *Service) logAccessChanges(ctx context.Context, tool, agent string, before, after map[string]any) {
	attrs := s.approvalLogAttrs(ctx, tool, agent)
	beforeList, beforeDef := toolListFromValues(before)
	afterList, afterDef := toolListFromValues(after)
	beforeTools, afterTools := toolListLog(beforeList, beforeDef), toolListLog(afterList, afterDef)
	if before == nil || !reflect.DeepEqual(beforeTools, afterTools) {
		if before == nil {
			beforeTools = nil
		}
		s.log.InfoContext(ctx, "agent tools changed", append(attrs, "before", beforeTools, "after", afterTools)...)
	}
	old, next := accessValues(before), accessValues(after)
	for _, field := range sortedKeys(next) {
		if isEmptyValue(old[field]) && isEmptyValue(next[field]) {
			continue
		}
		if reflect.DeepEqual(old[field], next[field]) {
			continue
		}
		s.log.InfoContext(ctx, "agent access changed", append(slices.Clone(attrs), "field", field, "before", redactedValue(old[field]), "after", redactedValue(next[field]))...)
	}
}

// redactedValue is a logged value with any URL's user:password@ replaced: values written before credentials in URLs were refused may
// still carry one.
func redactedValue(v any) any {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	redacted := redactUserinfo(string(raw))
	if redacted == string(raw) {
		return v
	}
	return json.RawMessage(redacted)
}

// isEmptyValue reports a value that says nothing: nil, "", or an empty list.
func isEmptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []any:
		return len(t) == 0
	case []string:
		return len(t) == 0
	}
	return false
}

// deleteGrants removes a deleted agent's grants: the record is emptied and
// marked pending, muster is told to drop every mark of the agent (keep
// empty), and the record goes once it confirmed; otherwise the empty record
// stays pending and is sent again on the next read of the namespace.
func (s *Service) deleteGrants(ctx context.Context, agent approvalgrants.Agent) (SessionSync, []DroppedGrant) {
	if s.cfg.Grants == nil || agent.UID == "" {
		return SessionSync{}, nil
	}
	var rec approvalgrants.Record
	var had []approvalgrants.Grant
	for attempt := 0; ; attempt++ {
		var err error
		rec, err = s.cfg.Grants.Get(ctx, agent)
		if err != nil {
			s.log.WarnContext(ctx, "approval grants of a deleted agent left behind", "agent", agent.Key(), "error", err.Error())
			return SessionSync{}, nil
		}
		if rec.Version == "" {
			return SessionSync{}, nil
		}
		had = rec.Grants
		rec.Grants, rec.PendingSessionSync = nil, true
		err = s.cfg.Grants.Replace(ctx, rec)
		if errors.Is(err, approvalgrants.ErrConflict) && attempt < 3 {
			continue
		}
		if err != nil {
			s.log.WarnContext(ctx, "approval grants of a deleted agent left behind", "agent", agent.Key(), "error", err.Error())
			return SessionSync{}, nil
		}
		break
	}
	dropped := make([]DroppedGrant, 0, len(had))
	for _, g := range had {
		s.log.InfoContext(ctx, "approval grant dropped", append(s.approvalLogAttrs(ctx, "delete_agent", agent.Key()),
			"entry", g.Entry, "server", g.Server, "connectorUid", g.ConnectorUID, "reason", "agent deleted")...)
		dropped = append(dropped, DroppedGrant{Entry: g.Entry, Server: g.Server, Reason: "agent deleted", connectorUID: g.ConnectorUID})
	}
	return s.syncSessions(ctx, "delete_agent", rec, true), dropped
}

// ListedGrant is a grant as list_approval_grants lists it: with its agent
// and the agent's display name ("" when it has none).
type ListedGrant struct {
	GrantView
	AgentDisplayName string `json:"agentDisplayName"`
}

// GrantList is list_approval_grants' answer. Unreadable names the configured
// namespaces a listing across namespaces skipped (the caller may not list
// their HelmReleases); UnreadableCount counts the organization namespaces it
// could not read for another reason, which are not named (an organization the
// caller is no member of is skipped silently, as toolchain_users does).
type GrantList struct {
	Grants          []ListedGrant `json:"grants"`
	Unreadable      []string      `json:"unreadable,omitempty"`
	UnreadableCount int           `json:"unreadableCount,omitempty"`
}

// ListApprovalGrants is list_approval_grants: the live grants of the agents
// of a namespace, or, without one, of every managed namespace whose
// HelmReleases the caller may list (the others are named in Unreadable),
// optionally on one connector (its muster server key). The releases are
// listed as the caller, which is the check; a grant whose agent's release
// moved to another generation does not hold and is not listed.
func (s *Service) ListApprovalGrants(ctx context.Context, namespace, server string) (*GrantList, error) {
	out := &GrantList{Grants: []ListedGrant{}}
	if namespace != "" {
		grants, err := s.namespaceGrants(ctx, namespace, server)
		if err != nil {
			return nil, err
		}
		out.Grants = grants
		return out, nil
	}
	for _, ns := range s.Managed(ctx) {
		organization := !slices.Contains(s.cfg.ManagedNamespaces, ns)
		grants, err := s.namespaceGrants(ctx, ns, server)
		switch {
		case err == nil:
			out.Grants = append(out.Grants, grants...)
		case organization && errors.Is(err, ErrUnauthenticated):
			return nil, err
		case organization && errors.Is(err, ErrForbidden):
			// An organization the caller does not belong to.
		case organization:
			out.UnreadableCount++
		case errors.Is(err, ErrForbidden):
			out.Unreadable = append(out.Unreadable, ns)
		default:
			return nil, err
		}
	}
	return out, nil
}

// namespaceGrants lists one namespace's live grants, as the caller: who may
// not list its HelmReleases (a SelfSubjectAccessReview, before anything else
// is read) is refused.
func (s *Service) namespaceGrants(ctx context.Context, namespace, server string) ([]ListedGrant, error) {
	st, err := s.site(ctx, Location{Namespace: namespace})
	if err != nil {
		return nil, err
	}
	if err := s.requireListReleases(ctx, st.fluxNS); err != nil {
		return nil, err
	}
	releases, err := s.agentHelmReleases(ctx, st)
	if err != nil {
		return nil, err
	}
	out := []ListedGrant{}
	if s.cfg.Grants == nil {
		return out, nil
	}
	records, err := s.cfg.Grants.ListByNamespace(ctx, st.ns())
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, hr := range releases {
		live[string(hr.GetUID())] = true
	}
	s.resendPendingIn(ctx, "list_approval_grants", records, live)
	for _, r := range records {
		hr := releases[r.Agent.Name]
		if hr == nil || string(hr.GetUID()) != r.Agent.UID {
			continue
		}
		display, _, _ := unstructured.NestedString(hr.Object, "spec", "values", "agent", "displayName")
		for _, g := range r.Grants {
			if g.Generation != hr.GetGeneration() || (server != "" && g.Server != server) {
				continue
			}
			view := grantView(g)
			agent := r.Agent
			view.Agent = &agent
			out = append(out, ListedGrant{GrantView: view, AgentDisplayName: display})
		}
	}
	return out, nil
}
