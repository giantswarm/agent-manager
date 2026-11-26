package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/agent-manager/internal/approvalgrants"
	"github.com/giantswarm/agent-manager/internal/kagent"
	"github.com/giantswarm/agent-manager/internal/muster"
)

// PlatformAgent is an agent whose release agent-manager cannot read because
// it lives on another cluster: the platform's default agent where the
// agent-platform chart installs its components into a target cluster from
// the management cluster (gitops.target.kubeConfig). agent-manager runs on
// the target, the release stays on the management cluster. The platform
// passes the approval relevant values of that release in agent-manager's own
// configuration (muster.toolApproval.platformAgents), rendered from the
// same values as the release in the same chart render: whoever may change
// them may change the release. A Session of such an agent binds these
// values; the Agent's Flux labels must name Release, and the agent chart's
// X-Muster-Tool-Approval header on its RemoteMCPServer, where rendered, must
// carry the same settings (a release changed past its configuration
// refuses the start until agent-manager is upgraded with it).
type PlatformAgent struct {
	// Namespace and Name are the Agent's.
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Release is the HelmRelease that renders the Agent, as the Agent's
	// helm.toolkit.fluxcd.io/namespace and /name labels name it.
	Release ReleaseRef `json:"release"`
	// Values are the release's spec.values that carry its approval
	// settings: tools, toolset, default and muster.requireApproval /
	// muster.enabled. Empty: the agent has none.
	Values map[string]any `json:"values,omitempty"`
}

// ReleaseRef names a HelmRelease.
type ReleaseRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// platformAgentValueKeys are the release values a PlatformAgent may carry:
// the ones sessionApproval reads, nothing else.
var platformAgentValueKeys = []string{ToolsValuesKey, ToolsetValuesKey, DefaultValuesKey, "muster"}

// ParsePlatformAgents reads the platform agents from JSON (an array; empty
// is none) and checks every entry strictly: an entry agent-manager would
// bind other than written is refused here, not at a Session's start.
func ParsePlatformAgents(raw string) ([]PlatformAgent, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var out []PlatformAgent
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("platform agents: %w", err)
	}
	seen := map[string]bool{}
	for i, p := range out {
		if p.Namespace == "" || p.Name == "" || p.Release.Namespace == "" || p.Release.Name == "" {
			return nil, fmt.Errorf("platform agents[%d]: namespace, name, release.namespace and release.name are required", i)
		}
		key := p.Namespace + "/" + p.Name
		if seen[key] {
			return nil, fmt.Errorf("platform agents[%d]: agent %s is listed twice", i, key)
		}
		seen[key] = true
		if err := p.validate(); err != nil {
			return nil, fmt.Errorf("platform agents[%d] (agent %s): %w", i, key, err)
		}
	}
	return out, nil
}

func (p PlatformAgent) validate() error {
	for k := range p.Values {
		if !slices.Contains(platformAgentValueKeys, k) {
			return fmt.Errorf("values.%s is not an approval value (%s)", k, strings.Join(platformAgentValueKeys, ", "))
		}
	}
	if raw, ok := p.Values[ToolsValuesKey]; ok {
		items, isList := raw.([]any)
		if !isList {
			return fmt.Errorf("values.tools must be a list")
		}
		list, err := toolListFrom(items)
		if err != nil {
			return err
		}
		if err := ValidateToolList(list, false); err != nil {
			return err
		}
	}
	if raw, ok := p.Values[ToolsetValuesKey]; ok {
		if _, isList := raw.([]any); !isList {
			return fmt.Errorf("values.toolset must be a list")
		}
	}
	if raw, ok := p.Values[DefaultValuesKey]; ok {
		def, isString := raw.(string)
		if !isString {
			return fmt.Errorf("values.default must be a string")
		}
		if err := ValidateDefault(def); err != nil {
			return err
		}
	}
	if raw, ok := p.Values["muster"]; ok {
		m, isMap := raw.(map[string]any)
		if !isMap {
			return fmt.Errorf("values.muster must be an object")
		}
		for k, v := range m {
			if _, isBool := v.(bool); !isBool || (k != "requireApproval" && k != "enabled") {
				return fmt.Errorf("values.muster takes requireApproval and enabled (booleans) only, got %s=%v", k, v)
			}
		}
	}
	return nil
}

// platformAgent is the configured PlatformAgent of ref, if any.
func (s *Service) platformAgent(ref kagent.Ref) (PlatformAgent, bool) {
	for _, p := range s.cfg.PlatformAgents {
		if p.Namespace == ref.Namespace && p.Name == ref.Name {
			return p, true
		}
	}
	return PlatformAgent{}, false
}

// platformAgentApproval is sessionApproval for a PlatformAgent: the
// configured values, after checking that the live Agent is the one the
// configuration describes (its Flux labels name the configured release) and
// that the release did not change past it (the agent chart's
// X-Muster-Tool-Approval header, where rendered, carries the same
// settings). Anything missing, unreadable or different refuses the start:
// the configuration is the only source of the settings, the live objects
// can only refuse, never loosen. No grants: they are recorded against a
// release agent-manager can read (set_agent_approval), which this one is not.
func (s *Service) platformAgentApproval(ctx context.Context, readers []dynamic.Interface, ref kagent.Ref, p PlatformAgent, sends bool, liveToolset []string) ([]string, *boundApproval, error) {
	agentWhat := fmt.Sprintf("Agent %s/%s", ref.Namespace, ref.Name)
	obj, _, err := readFirst(ctx, readers, s.agentGVR(), ref.Namespace, ref.Name, agentWhat, "agents")
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil, approvalUnavailablef("agent %s is configured as rendered by HelmRelease %s/%s on another cluster, but its Agent cannot be found", ref.Tenant(), p.Release.Namespace, p.Release.Name)
	case err != nil:
		return nil, nil, s.unreadableRefusal(err, ref)
	}
	if name, ns := ownerOf(obj); name != p.Release.Name || ns != p.Release.Namespace {
		return nil, nil, approvalUnavailablef("agent %s is configured as rendered by HelmRelease %s/%s (agent-manager's muster.toolApproval.platformAgents), but its %s and %s labels name %q/%q, so its tool approval settings are unknown; a session without them would drop the agent's Ask and Block", ref.Tenant(), p.Release.Namespace, p.Release.Name, HelmReleaseNamespaceLabel, HelmReleaseNameLabel, ns, name)
	}
	list, def := toolListFromValues(p.Values)
	server, _, err := readFirst(ctx, readers, s.mcpServerGVR(), ref.Namespace, ref.Name, fmt.Sprintf("RemoteMCPServer %s/%s", ref.Namespace, ref.Name), "remotemcpservers")
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, nil, s.unreadableRefusal(err, ref)
	default:
		if header := headerOf(server, ToolApprovalHeader); header != "" {
			if err := sameApproval(header, list, def); err != nil {
				return nil, nil, approvalUnavailablef("agent %s's release %s/%s renders other tool approval settings than agent-manager is configured with (%v): the release changed past agent-manager's muster.toolApproval.platformAgents; upgrade agent-manager with the platform's current values", ref.Tenant(), p.Release.Namespace, p.Release.Name, err)
			}
		}
	}
	inline := list.HasSettings() || def != ""
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
	if def == "" && requireApprovalBinds(p.Values) {
		def = SettingAsk
	}
	out := &boundApproval{agent: approvalgrants.Agent{Namespace: ref.Namespace, Name: ref.Name}, binding: &muster.Approval{Default: def}}
	for _, e := range list {
		if e.Setting != "" && slices.Contains(toolset, e.Selector) {
			out.binding.Entries = append(out.binding.Entries, muster.ApprovalEntry{Entry: e.Selector, Setting: e.Setting})
		}
	}
	return toolset, out, nil
}

// unreadableRefusal answers an object of a platform agent no reader may get.
func (s *Service) unreadableRefusal(err error, ref kagent.Ref) error {
	var u *releaseUnreadableError
	if errors.As(err, &u) {
		return forbiddenf("%s, which agent-manager checks before binding agent %s's tool approval settings, may be read neither by agent-manager's ServiceAccount nor by you (get %s); a session without them would drop the agent's Ask and Block", u.what, ref.Tenant(), u.resource)
	}
	return err
}

// sameApproval compares the agent chart's X-Muster-Tool-Approval header
// ("<selector>=<Setting>,...,default=<Setting>") with a tool list's
// settings and default, as sets: the order is the chart's.
func sameApproval(header string, list ToolList, def string) error {
	got := map[string]string{}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		i := strings.LastIndex(part, "=")
		if i <= 0 {
			return fmt.Errorf("unreadable header item %q", part)
		}
		key, setting := part[:i], part[i+1:]
		if _, dup := got[key]; dup || !validSetting(setting) {
			return fmt.Errorf("unreadable header item %q", part)
		}
		got[key] = setting
	}
	want := map[string]string{}
	for _, e := range list {
		if e.Setting != "" {
			want[e.Selector] = e.Setting
		}
	}
	if def != "" {
		want[DefaultEntry] = def
	}
	if !maps.Equal(got, want) {
		return fmt.Errorf("rendered %s, configured %s", describeSettings(got), describeSettings(want))
	}
	return nil
}

func describeSettings(m map[string]string) string {
	if len(m) == 0 {
		return "none"
	}
	keys := slices.Sorted(maps.Keys(m))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}
