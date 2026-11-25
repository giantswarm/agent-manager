package agents

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/approvalgrants"
	"github.com/giantswarm/agent-manager/internal/identity"
)

// SetApproval is set_agent_approval: new settings for entries of the agent's
// tool list, and its default. The only write that grants an Allow over a
// connector's Ask, after a connector-admin check per connector.
type SetApproval struct {
	Location
	Name string `json:"name"`
	// Settings sets each named entry's setting; "" removes it (the entry
	// stays, without a setting). Entries not named keep theirs.
	Settings map[string]string `json:"settings,omitempty"`
	// Default sets the list's default; "" removes it; nil keeps it.
	Default *string `json:"default,omitempty"`
	// Force writes to a suspended release anyway.
	Force bool `json:"force,omitempty"`
	WriteOptions
}

// SetApprovalBefore is the list before the save.
type SetApprovalBefore struct {
	Tools   ToolList `json:"tools"`
	Default string   `json:"default,omitempty"`
}

// SetApprovalResult is set_agent_approval's answer.
type SetApprovalResult struct {
	Agent   approvalgrants.Agent `json:"agent"`
	Tools   ToolList             `json:"tools"`
	Default string               `json:"default,omitempty"`
	Before  SetApprovalBefore    `json:"before"`
	Grants  []GrantView          `json:"grants"`
	Dropped []DroppedGrant       `json:"dropped,omitempty"`
	// RequestedBy is the authenticated caller the write ran as.
	RequestedBy string `json:"requestedBy,omitempty"`
	WriteOutcome
}

// SetApproval changes approval settings of an agent's tool list. An Allow it
// names (or a default Allow) that selects a tool whose connector asks needs
// the caller to be a connector admin of that connector (update mcpservers in
// its namespace, asked as the caller); it is then recorded as a grant at the
// generation the save wrote. Every other grant of the agent follows the save
// as any save's does (settleGrants).
func (s *Service) SetApproval(ctx context.Context, in SetApproval) (*SetApprovalResult, error) {
	if err := ValidateName(in.Name); err != nil {
		return nil, err
	}
	if len(in.Settings) == 0 && in.Default == nil {
		return nil, invalidf("set_agent_approval needs settings (per entry) or default")
	}
	for entry, setting := range in.Settings {
		if setting != "" && !validSetting(setting) {
			return nil, invalidf("settings[%q]: %q is not Allow, Ask, Block or \"\" (removes the setting)", entry, setting)
		}
	}
	if in.Default != nil {
		if err := ValidateDefault(*in.Default); err != nil {
			return nil, err
		}
	}
	mode, err := s.checkMode(in.WriteOptions)
	if err != nil {
		return nil, err
	}
	st, err := s.site(ctx, in.Location)
	if err != nil {
		return nil, err
	}
	if !s.chartTakesTools(ctx) {
		return nil, fmt.Errorf("%w: the agent chart %s has no tool approval settings (tools, default); use a chart that has them", ErrUnsupported, orDefault(s.chartVersion(ctx), "in use"))
	}
	hr, _, err := s.writableHelmRelease(ctx, st, in.Name, in.Force, mode == ModeCommit)
	if err != nil {
		return nil, err
	}
	values, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
	before, beforeDef := toolListFromValues(values)
	if before == nil {
		return nil, invalidf("agent %s has no tool list to set approvals on; give it one with update_agent_access", st.describe(in.Name))
	}
	list := make(ToolList, len(before))
	copy(list, before)
	named := map[string]bool{}
	for _, entry := range sortedKeys(in.Settings) {
		i := indexOfEntry(list, entry)
		if i < 0 {
			return nil, invalidf("set_agent_approval changes the settings of entries the agent lists; %q is not one of them: add it with update_agent_access", entry)
		}
		list[i].Setting = in.Settings[entry]
		if list[i].Setting == SettingAllow {
			named[entry] = true
		}
	}
	def := beforeDef
	if in.Default != nil {
		def = *in.Default
		if def == SettingAllow {
			named[DefaultEntry] = true
		}
	}
	if err := ValidateToolList(list, true); err != nil {
		return nil, err
	}
	needs, err := s.grantNeeds(ctx, list, def, named)
	if err != nil {
		return nil, err
	}
	if len(needs) > 0 {
		if err := s.requireGrantable(ctx, hr, mode, needs); err != nil {
			return nil, err
		}
	}
	upd := Update{Location: in.Location, Name: in.Name, Tools: &list, Default: &def, Force: in.Force, WriteOptions: in.WriteOptions,
		tool: "set_agent_approval", approval: true, granted: needs}
	res, err := s.Update(ctx, upd)
	if err != nil {
		return nil, err
	}
	out := &SetApprovalResult{
		Agent:   approvalgrants.Agent{UID: string(hr.GetUID()), Namespace: st.ns(), Name: in.Name},
		Tools:   list,
		Default: def,
		Before:  SetApprovalBefore{Tools: before, Default: beforeDef},
		Grants:  res.Grants,
		Dropped: res.Dropped,

		RequestedBy:  identity.Caller(ctx),
		WriteOutcome: res.WriteOutcome,
	}
	if out.Grants == nil {
		out.Grants = []GrantView{}
	}
	return out, nil
}

// requireGrantable checks that the grants a save needs can be written: a
// live write of a release without valuesFrom, by a connector admin of each
// connector.
func (s *Service) requireGrantable(ctx context.Context, hr *unstructured.Unstructured, mode string, needs map[string][]Connector) error {
	if mode == ModeCommit {
		return invalidf("a grant is recorded only for a live write (mode apply); a release managed in git carries no grants")
	}
	if from, _, _ := unstructured.NestedSlice(hr.Object, "spec", "valuesFrom"); len(from) > 0 {
		return invalidf("HelmRelease %s/%s takes values from spec.valuesFrom, which agent-manager neither reads nor keeps, so it carries no grants", hr.GetNamespace(), hr.GetName())
	}
	if s.cfg.Grants == nil {
		return fmt.Errorf("%w: this agent-manager keeps no approval grants", ErrUnsupported)
	}
	memo := &adminMemo{s: s}
	for _, entry := range sortedKeys(needs) {
		for _, c := range needs[entry] {
			admin, err := memo.admin(ctx, c.Namespace)
			if err != nil {
				return err
			}
			if !admin {
				return forbiddenf("allowing %s over the Ask of connector %s needs a connector admin of it (update mcpservers in namespace %s); ask one to grant it with set_agent_approval", entry, c.Server, c.Namespace)
			}
		}
	}
	return nil
}

func indexOfEntry(l ToolList, selector string) int {
	for i, e := range l {
		if e.Selector == selector {
			return i
		}
	}
	return -1
}

// UpdateAs is Update for a save through the named tool (update_agent,
// update_agent_access): the tool names the save in messages and logs. The
// split of which fields each tool takes is the MCP layer's.
func (s *Service) UpdateAs(ctx context.Context, tool string, upd Update) (*UpdateResult, error) {
	upd.tool = tool
	return s.Update(ctx, upd)
}

// ValidateUpdateAs is ValidateUpdate for a save through the named tool.
func (s *Service) ValidateUpdateAs(ctx context.Context, tool string, upd Update) (*ValidateResult, error) {
	upd.tool = tool
	return s.ValidateUpdate(ctx, upd)
}
