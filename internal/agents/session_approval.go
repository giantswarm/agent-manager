package agents

import (
	"context"
	"errors"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/approvalgrants"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kagent"
	"github.com/giantswarm/agent-manager/internal/muster"
)

// ToolApprovalHeader is the static header the agent chart renders on the
// agent's RemoteMCPServer with the entries' settings and the default, which
// muster applies to calls of the agent that no Session binding carries
// settings for (contract-slice-3, review fix round, item 7).
const ToolApprovalHeader = "X-Muster-Tool-Approval"

// MusterApprovalVersionHint names the muster a binding with approval
// settings needs.
const MusterApprovalVersionHint = "a muster with tool approval per agent"

// boundApproval is what a Session's binding carries of its agent's approval
// settings.
type boundApproval struct {
	agent   approvalgrants.Agent
	binding *muster.Approval
}

// hasSettings reports whether the binding carries anything muster must
// enforce: an entry's setting, a default or a grant mark.
func (b *boundApproval) hasSettings() bool {
	return b != nil && b.binding != nil && (len(b.binding.Entries) > 0 || b.binding.Default != "" || len(b.binding.Grants) > 0)
}

func approvalUnavailablef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrApprovalUnavailable, fmt.Sprintf(format, args...))
}

// sendsApproval reports whether binds carry agent-manager's sender token, so
// that muster takes approval settings from them.
func (s *Service) sendsApproval() bool {
	sender, ok := s.cfg.Bindings.(ApprovalSender)
	return ok && sender.SendsApproval()
}

// sessionApproval reads the agent's approval settings and live grants for a
// Session binding and returns the toolset to bind with them. Settings that
// cannot be bound refuse the start (fail closed): agent-
// manager sending none (tool approval off), settings that may come from
// spec.valuesFrom (which agent-manager does not read), or a HelmRelease-
// managed agent whose release is gone. With settings, the toolset bound is
// the release's own list, not the live Agent and RemoteMCPServer: an
// edit of those objects never widens what a grant covers. An agent without
// a release (a bare Agent) carries none.
func (s *Service) sessionApproval(ctx context.Context, ref kagent.Ref, liveToolset []string) ([]string, *boundApproval, error) {
	sends := s.sendsApproval()
	reader := s.cfg.ApprovalReader
	if reader == nil {
		client, err := s.kube.Client(ctx)
		if err != nil {
			return nil, nil, err
		}
		reader = client
	}
	dyn := reader.Dynamic()
	hr, err := dyn.Resource(s.helmReleaseGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsForbidden(err) && s.cfg.ApprovalReader != nil {
		// agent-manager's ServiceAccount may not read releases in every
		// namespace (an organization's: the chart grants the reader Role in
		// the configured namespaces only); the person starting
		// the Session usually may. Refused only when neither can.
		if caller, callerErr := s.kube.Client(ctx); callerErr == nil {
			dyn = caller.Dynamic()
			hr, err = dyn.Resource(s.helmReleaseGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		}
	}
	switch {
	case apierrors.IsNotFound(err):
		obj, objErr := s.getAgentObject(ctx, dyn, ref.Namespace, ref.Name)
		if objErr == nil && obj != nil && obj.GetLabels()[HelmReleaseNameLabel] != "" {
			return nil, nil, approvalUnavailablef("agent %s is rendered by HelmRelease %s, which cannot be found, so its tool approval settings are unknown; a session without them would drop the agent's Ask and Block", ref.Tenant(), obj.GetLabels()[HelmReleaseNameLabel])
		}
		return liveToolset, nil, nil
	case apierrors.IsForbidden(err) && !sends:
		s.log.WarnContext(ctx, "the agent's HelmRelease may not be read; the session starts without checking for tool approval settings", identity.LogAttr(ctx), "agent", ref.Tenant())
		return liveToolset, nil, nil
	case apierrors.IsForbidden(err):
		return nil, nil, forbiddenf("the agent's HelmRelease %s/%s, which holds its tool approval settings, may be read neither by agent-manager's ServiceAccount nor by you (get helmreleases); a session without them would drop the agent's Ask and Block", ref.Namespace, ref.Name)
	case err != nil:
		return nil, nil, wrapKube(err, fmt.Sprintf("get HelmRelease %s/%s", ref.Namespace, ref.Name))
	}
	values, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
	list, def := toolListFromValues(values)
	inline := list.HasSettings() || def != ""
	if from, _, _ := unstructured.NestedSlice(hr.Object, "spec", "valuesFrom"); len(from) > 0 {
		server, _ := s.getMCPServer(ctx, dyn, ref.Namespace, ref.Name)
		if inline || (server != nil && headerOf(server, ToolApprovalHeader) != "") {
			return nil, nil, approvalUnavailablef("agent %s takes values from spec.valuesFrom and carries tool approval settings: agent-manager binds settings only from the release's inline values, so a session would run without some of them; move the tool list into spec.values", ref.Tenant())
		}
	}
	if !sends {
		if inline {
			return nil, nil, approvalUnavailablef("agent %s carries tool approval settings (tools entries with Allow/Ask/Block, default), and this installation does not bind them to sessions (agent-manager's muster.toolApproval is off): a session would run without the agent's Ask and Block", ref.Tenant())
		}
		return liveToolset, nil, nil
	}
	toolset := liveToolset
	if inline && list != nil {
		toolset = list.Selectors()
	}
	agent := approvalgrants.Agent{UID: string(hr.GetUID()), Namespace: ref.Namespace, Name: ref.Name}
	if def == "" && requireApprovalBinds(values) {
		// kagent's flag (agent chart 2.x) pauses the agent before every call
		// through muster; the binding asks too, so a Session running when the
		// 3.0 upgrade drops the flag keeps asking: muster ignores the agent's
		// X-Muster-Tool-Approval header for a binding with settings.
		def = SettingAsk
	}
	out := &boundApproval{agent: agent, binding: &muster.Approval{Default: def}}
	for _, e := range list {
		if e.Setting != "" && slices.Contains(toolset, e.Selector) {
			out.binding.Entries = append(out.binding.Entries, muster.ApprovalEntry{Entry: e.Selector, Setting: e.Setting})
		}
	}
	grants, err := s.liveGrants(ctx, "start_session", agent, hr.GetGeneration())
	if err != nil {
		return nil, nil, err
	}
	allow := allowEntries(list, def)
	for _, g := range grants {
		if allow[g.Entry] && (g.Entry == DefaultEntry || slices.Contains(toolset, g.Entry)) {
			out.binding.Grants = append(out.binding.Grants, muster.Grant{Entry: g.Entry, Server: g.Server, UID: g.ConnectorUID, Tools: g.Tools})
		}
	}
	return toolset, out, nil
}

// bindWithApproval binds the Session with its agent's approval settings and
// checks that muster took them (SettingsHeader). A muster that predates
// approval settings refuses the unknown fields: a binding without settings
// is sent again without them, one with settings refuses the start. A muster that took the bind but not the settings refuses it
// too; the caller abandons the Session.
func (s *Service) bindWithApproval(ctx context.Context, token string, binding *muster.Binding, approval *boundApproval) error {
	res, err := s.cfg.Bindings.Bind(ctx, token, *binding)
	if errors.Is(err, muster.ErrUnknownField) && (binding.Agent != "" || binding.Approval != nil) {
		if approval.hasSettings() {
			return approvalUnavailablef("muster does not take an agent's tool approval settings with a session (it refused them as unknown fields), and agent %s carries some; it needs %s", binding.Agent, MusterApprovalVersionHint)
		}
		s.log.InfoContext(ctx, "muster takes no agent with a session binding; bound without it", identity.LogAttr(ctx), "agent", binding.Agent)
		binding.Agent, binding.Approval = "", nil
		res, err = s.cfg.Bindings.Bind(ctx, token, *binding)
	}
	if err != nil {
		return err
	}
	if approval.hasSettings() && res.Settings != muster.SettingsBound {
		if unbindErr := s.cfg.Bindings.Unbind(ctx, token, binding.Actor); unbindErr != nil {
			s.log.WarnContext(ctx, "session bound without its approval settings could not be unbound", identity.LogAttr(ctx), "actor", binding.Actor, "error", unbindErr.Error())
		}
		answer := res.Settings
		if answer == "" {
			answer = "no answer"
		}
		return approvalUnavailablef("muster did not take agent %s's tool approval settings with the session (%s: %s); it needs %s, and agent-manager's sender token must pass its review", binding.Agent, muster.SettingsHeader, answer, MusterApprovalVersionHint)
	}
	return nil
}

// recheckBoundGrants reads the agent's grants again after a bind: a save
// that ran meanwhile may have dropped one this bind still carried. muster is
// then told the grants left (marked pending first, so a failed call is sent
// again), this Session included; no lock across replicas is needed.
func (s *Service) recheckBoundGrants(ctx context.Context, bound *boundApproval) {
	if s.cfg.Grants == nil || bound == nil || len(bound.binding.Grants) == 0 {
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		rec, err := s.cfg.Grants.Get(ctx, bound.agent)
		if err != nil {
			s.log.WarnContext(ctx, "approval grants unreadable after a bind; the session keeps the grants it was bound with", "agent", bound.agent.Key(), "error", err.Error())
			return
		}
		// Compared as muster compares marks: entry, server, uid and the
		// sorted tools (a regrant that narrowed the tools changed the mark).
		kept := keepMarks(rec.Grants)
		held := func(m muster.Grant) bool {
			return slices.ContainsFunc(kept, func(k muster.Grant) bool { return markKey(k) == markKey(m) })
		}
		if !slices.ContainsFunc(bound.binding.Grants, func(m muster.Grant) bool { return !held(m) }) {
			return
		}
		rec.PendingSessionSync = true
		if err := s.cfg.Grants.Replace(ctx, rec); errors.Is(err, approvalgrants.ErrConflict) {
			continue
		} else if err != nil {
			s.log.WarnContext(ctx, "approval grants could not be marked for muster after a bind", "agent", bound.agent.Key(), "error", err.Error())
		}
		s.syncSessions(ctx, "start_session", rec, false)
		return
	}
}

// requireApprovalBinds reports whether release values carry
// muster.requireApproval: true on a muster binding the 2.x chart renders
// (muster on, a tool list other than exactly ["preset:none"]).
func requireApprovalBinds(values map[string]any) bool {
	if on, _, _ := unstructured.NestedBool(values, "muster", "requireApproval"); !on {
		return false
	}
	if enabled, found, _ := unstructured.NestedFieldNoCopy(values, "muster", "enabled"); found {
		if b, _ := enabled.(bool); !b {
			return false
		}
	}
	list := values[ToolsValuesKey]
	if list == nil {
		list = values[ToolsetValuesKey]
	}
	items, _ := list.([]any)
	return len(items) != 1 || items[0] != "preset:none"
}
