package agents

import (
	"context"
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
	"github.com/giantswarm/agent-manager/internal/muster"
)

// Tool approval per agent (giantswarm/giantswarm#38902, slice 3). An entry's
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

// Connector is an MCPServer as muster reports it.
type Connector struct {
	Server    string `json:"server"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// GrantDropper is muster's binding endpoint that removes grant marks from an
// agent's running Sessions (PATCH /session-bindings/grants).
type GrantDropper interface {
	DropGrants(ctx context.Context, token, agent string, keep []muster.Grant) error
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
}

// DroppedGrant is a grant a save removed.
type DroppedGrant struct {
	Entry  string `json:"entry"`
	Server string `json:"server"`
	Reason string `json:"reason"`

	connectorUID string
}

func grantView(g approvalgrants.Grant) GrantView {
	return GrantView{Entry: g.Entry, Server: g.Server, ConnectorNamespace: g.ConnectorNamespace, ConnectorUID: g.ConnectorUID, GrantedBy: g.GrantedBy, GrantedAt: g.GrantedAt, Generation: g.Generation}
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
			if !slices.ContainsFunc(needs[t.Entry], func(have Connector) bool { return have.Server == c.Server }) {
				needs[t.Entry] = append(needs[t.Entry], c)
			}
		}
	}
	for entry := range needs {
		sort.Slice(needs[entry], func(i, j int) bool { return needs[entry][i].Server < needs[entry][j].Server })
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
// namespace: the connector-admin check. Without downstream OAuth the review
// asks for agent-manager's ServiceAccount, which holds no such right, so no
// one is a connector admin.
func (s *Service) isConnectorAdmin(ctx context.Context, namespace string) (bool, error) {
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
// grants are written. Dropped grants leave the agent's running Sessions.
func (s *Service) settleGrants(ctx context.Context, save grantSave) ([]GrantView, []DroppedGrant, error) {
	if s.cfg.Grants == nil {
		if len(save.granted) > 0 {
			return nil, nil, fmt.Errorf("%w: this agent-manager keeps no approval grants (no grants store)", ErrUnsupported)
		}
		return nil, nil, nil
	}
	var kept []GrantView
	var dropped []DroppedGrant
	var removed bool
	for attempt := 0; ; attempt++ {
		rec, err := s.cfg.Grants.Get(ctx, save.agent)
		if err != nil {
			return nil, nil, err
		}
		next, k, d, err := s.nextGrants(ctx, save, rec.Grants)
		if err != nil {
			return nil, nil, err
		}
		rec.Grants = next
		err = s.cfg.Grants.Replace(ctx, rec)
		if errors.Is(err, approvalgrants.ErrConflict) && attempt < 3 {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		kept, dropped, removed = k, d, len(d) > 0
		break
	}
	for _, g := range kept {
		s.log.InfoContext(ctx, "approval grant recorded", append(s.approvalLogAttrs(ctx, save.tool, save.agent.Key()),
			"entry", g.Entry, "server", g.Server, "connectorUid", g.ConnectorUID, "generation", g.Generation, "kept", !isNew(save, g))...)
	}
	for _, d := range dropped {
		s.log.InfoContext(ctx, "approval grant dropped", append(s.approvalLogAttrs(ctx, save.tool, save.agent.Key()),
			"entry", d.Entry, "server", d.Server, "connectorUid", d.connectorUID, "reason", d.Reason)...)
	}
	if removed {
		s.dropFromSessions(ctx, save.tool, save.agent.Key(), kept)
	}
	return kept, dropped, nil
}

// isNew reports whether g is one of the save's own grants.
func isNew(save grantSave, g GrantView) bool {
	return slices.ContainsFunc(save.granted[g.Entry], func(c Connector) bool { return c.Server == g.Server })
}

// nextGrants is settleGrants' decision over the stored grants.
func (s *Service) nextGrants(ctx context.Context, save grantSave, current []approvalgrants.Grant) ([]approvalgrants.Grant, []GrantView, []DroppedGrant, error) {
	saver := identity.Caller(ctx)
	now := time.Now().UTC().Truncate(time.Second)
	allow := allowEntries(save.tools, save.def)
	memo := &adminMemo{s: s}
	var next []approvalgrants.Grant
	var dropped []DroppedGrant
	for _, g := range current {
		if _, regranted := save.granted[g.Entry]; regranted {
			continue // replaced by this save's own grants below
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
				reason = fmt.Sprintf("saved by %s, not a connector admin of %s", orDefault(saver, "an unknown caller"), g.Server)
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
			next = append(next, approvalgrants.Grant{Entry: entry, Server: c.Server, ConnectorNamespace: c.Namespace, ConnectorUID: c.UID, GrantedBy: saver, GrantedAt: now, Generation: save.generation})
		}
	}
	views := make([]GrantView, 0, len(next))
	for _, g := range next {
		views = append(views, grantView(g))
	}
	return next, views, dropped, nil
}

// dropFromSessions removes the dropped grants' marks from the agent's running
// Sessions: muster keeps only the grants left. A failure is logged; the
// records are gone regardless, and the marks end with the Sessions.
func (s *Service) dropFromSessions(ctx context.Context, tool, agent string, left []GrantView) {
	dropper, ok := s.cfg.Bindings.(GrantDropper)
	if !ok || s.cfg.Bindings == nil {
		return
	}
	token, _ := identity.TokenFromContext(ctx)
	keep := make([]muster.Grant, 0, len(left))
	for _, g := range left {
		keep = append(keep, muster.Grant{Entry: g.Entry, Server: g.Server, UID: g.ConnectorUID})
	}
	attrs := s.approvalLogAttrs(ctx, tool, agent)
	if err := dropper.DropGrants(ctx, token, agent, keep); err != nil {
		s.log.WarnContext(ctx, "approval grants removed from running sessions", append(attrs, "kept", len(keep), "error", err.Error())...)
		return
	}
	s.log.InfoContext(ctx, "approval grants removed from running sessions", append(attrs, "kept", len(keep))...)
}

// approvalLogAttrs are the fields every approval log line carries.
func (s *Service) approvalLogAttrs(ctx context.Context, tool, agent string) []any {
	return []any{"agent", agent, "tool", tool, "savedBy", identity.Caller(ctx), "callerKind", identity.CallerKind(ctx)}
}

// liveGrants are the agent's grants that hold for generation: a grant
// written at another generation was written before values changed past
// agent-manager and is dropped (from the records and the Sessions).
func (s *Service) liveGrants(ctx context.Context, tool string, agent approvalgrants.Agent, generation int64) ([]approvalgrants.Grant, error) {
	if s.cfg.Grants == nil || agent.UID == "" {
		return nil, nil
	}
	for attempt := 0; ; attempt++ {
		rec, err := s.cfg.Grants.Get(ctx, agent)
		if err != nil {
			return nil, err
		}
		var live []approvalgrants.Grant
		var stale []approvalgrants.Grant
		for _, g := range rec.Grants {
			if g.Generation == generation {
				live = append(live, g)
			} else {
				stale = append(stale, g)
			}
		}
		if len(stale) == 0 {
			return live, nil
		}
		rec.Grants = live
		if err := s.cfg.Grants.Replace(ctx, rec); err != nil {
			if errors.Is(err, approvalgrants.ErrConflict) && attempt < 3 {
				continue
			}
			return nil, err
		}
		views := make([]GrantView, 0, len(live))
		for _, g := range live {
			views = append(views, grantView(g))
		}
		for _, g := range stale {
			s.log.InfoContext(ctx, "approval grant dropped", append(s.approvalLogAttrs(ctx, tool, agent.Key()),
				"entry", g.Entry, "server", g.Server, "connectorUid", g.ConnectorUID,
				"reason", fmt.Sprintf("values changed outside agent-manager (release generation %d, grant written at %d)", generation, g.Generation))...)
		}
		s.dropFromSessions(ctx, tool, agent.Key(), views)
		return live, nil
	}
}

// attachGrants reports each agent's live grants (those written at its
// release's current generation) and whether the chart takes approval
// settings. A store that cannot be read leaves the grants out, logged: the
// listing is not refused for it.
func (s *Service) attachGrants(ctx context.Context, namespace string, list []Agent) {
	takes := s.chartTakesTools(ctx)
	for i := range list {
		list[i].ApprovalSettings = takes
	}
	if s.cfg.Grants == nil {
		return
	}
	records, err := s.cfg.Grants.ListByNamespace(ctx, namespace)
	if err != nil {
		s.log.WarnContext(ctx, "approval grants unreadable; agents listed without them", "namespace", namespace, "error", err.Error())
		return
	}
	byUID := map[string][]approvalgrants.Grant{}
	for _, r := range records {
		byUID[r.Agent.UID] = r.Grants
	}
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
	kept, dropped, err := s.settleGrants(ctx, grantSave{
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
	res.Grants, res.Dropped = kept, dropped
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
		s.log.InfoContext(ctx, "agent access changed", append(slices.Clone(attrs), "field", field, "before", old[field], "after", next[field])...)
	}
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

// deleteGrants removes a deleted agent's grants. A failure is logged: a
// recreated agent has a new UID, so the record holds for nothing.
func (s *Service) deleteGrants(ctx context.Context, agent approvalgrants.Agent) {
	if s.cfg.Grants == nil || agent.UID == "" {
		return
	}
	rec, err := s.cfg.Grants.Get(ctx, agent)
	if err == nil {
		err = s.cfg.Grants.Delete(ctx, agent.UID)
	}
	if err != nil {
		s.log.WarnContext(ctx, "approval grants of a deleted agent left behind", "agent", agent.Key(), "error", err.Error())
		return
	}
	for _, g := range rec.Grants {
		s.log.InfoContext(ctx, "approval grant dropped", append(s.approvalLogAttrs(ctx, "delete_agent", agent.Key()),
			"entry", g.Entry, "server", g.Server, "connectorUid", g.ConnectorUID, "reason", "agent deleted")...)
	}
}

// ListApprovalGrants is list_approval_grants: the live grants of the agents
// of a namespace, optionally on one connector (its muster server key). The
// caller must be able to list the namespace's HelmReleases (the list is read
// as the caller, which is the check); a grant whose agent's release moved to
// another generation does not hold and is not listed.
func (s *Service) ListApprovalGrants(ctx context.Context, namespace, server string) ([]GrantView, error) {
	st, err := s.site(ctx, Location{Namespace: namespace})
	if err != nil {
		return nil, err
	}
	releases, err := s.agentHelmReleases(ctx, st)
	if err != nil {
		return nil, err
	}
	out := []GrantView{}
	if s.cfg.Grants == nil {
		return out, nil
	}
	records, err := s.cfg.Grants.ListByNamespace(ctx, st.ns())
	if err != nil {
		return nil, err
	}
	for _, r := range records {
		hr := releases[r.Agent.Name]
		if hr == nil || string(hr.GetUID()) != r.Agent.UID {
			continue
		}
		for _, g := range r.Grants {
			if g.Generation != hr.GetGeneration() || (server != "" && g.Server != server) {
				continue
			}
			view := grantView(g)
			agent := r.Agent
			view.Agent = &agent
			out = append(out, view)
		}
	}
	return out, nil
}
