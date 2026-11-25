package agents

import (
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/approvalgrants"
	"github.com/giantswarm/agent-manager/internal/kagent"
	"github.com/giantswarm/agent-manager/internal/muster"
)

// boundApproval is what a Session's binding carries of its agent's approval
// settings.
type boundApproval struct {
	agent   approvalgrants.Agent
	binding *muster.Approval
}

// sessionApproval reads the agent's approval settings and live grants for a
// Session binding, when muster takes them from agent-manager (the binder
// sends its approval sender token). The release is read with
// Config.ApprovalReader where set, else as the caller; a release that cannot
// be read refuses the start, since binding without the settings would drop
// the agent's Ask and Block. An agent without a release (a bare Agent)
// carries none. Entries outside the bound toolset are left out: muster
// requires every entry to be one of its selectors.
func (s *Service) sessionApproval(ctx context.Context, ref kagent.Ref, toolset []string) (*boundApproval, error) {
	sender, ok := s.cfg.Bindings.(ApprovalSender)
	if !ok || !sender.SendsApproval() {
		return nil, nil
	}
	reader := s.cfg.ApprovalReader
	if reader == nil {
		client, err := s.kube.Client(ctx)
		if err != nil {
			return nil, err
		}
		reader = client
	}
	hr, err := reader.Dynamic().Resource(s.helmReleaseGVR()).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case apierrors.IsForbidden(err):
		return nil, forbiddenf("the agent's HelmRelease %s/%s, which holds its tool approval settings, may not be read (get helmreleases); a session without them would drop the agent's Ask and Block", ref.Namespace, ref.Name)
	case err != nil:
		return nil, wrapKube(err, fmt.Sprintf("get HelmRelease %s/%s", ref.Namespace, ref.Name))
	}
	values, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
	list, def := toolListFromValues(values)
	agent := approvalgrants.Agent{UID: string(hr.GetUID()), Namespace: ref.Namespace, Name: ref.Name}
	out := &boundApproval{agent: agent, binding: &muster.Approval{Default: def}}
	for _, e := range list {
		if e.Setting != "" && slices.Contains(toolset, e.Selector) {
			out.binding.Entries = append(out.binding.Entries, muster.ApprovalEntry{Entry: e.Selector, Setting: e.Setting})
		}
	}
	grants, err := s.liveGrants(ctx, "start_session", agent, hr.GetGeneration())
	if err != nil {
		return nil, err
	}
	allow := allowEntries(list, def)
	for _, g := range grants {
		if allow[g.Entry] && (g.Entry == DefaultEntry || slices.Contains(toolset, g.Entry)) {
			out.binding.Grants = append(out.binding.Grants, muster.Grant{Entry: g.Entry, Server: g.Server, UID: g.ConnectorUID})
		}
	}
	return out, nil
}

// recheckBoundGrants reads the agent's grants again after a bind: a save
// that ran meanwhile may have dropped one this bind still carried. Its mark
// is then removed from the agent's Sessions, this one included, so no lock
// across replicas is needed.
func (s *Service) recheckBoundGrants(ctx context.Context, bound *boundApproval) {
	if s.cfg.Grants == nil || len(bound.binding.Grants) == 0 {
		return
	}
	rec, err := s.cfg.Grants.Get(ctx, bound.agent)
	if err != nil {
		s.log.WarnContext(ctx, "approval grants unreadable after a bind; the session keeps the grants it was bound with", "agent", bound.agent.Key(), "error", err.Error())
		return
	}
	held := func(m muster.Grant) bool {
		return slices.ContainsFunc(rec.Grants, func(g approvalgrants.Grant) bool {
			return g.Entry == m.Entry && g.Server == m.Server && g.ConnectorUID == m.UID
		})
	}
	if !slices.ContainsFunc(bound.binding.Grants, func(m muster.Grant) bool { return !held(m) }) {
		return
	}
	left := make([]GrantView, 0, len(rec.Grants))
	for _, g := range rec.Grants {
		left = append(left, grantView(g))
	}
	s.dropFromSessions(ctx, "start_session", bound.agent.Key(), left)
}
