package migrate

import (
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/kube"
	"github.com/giantswarm/agent-manager/internal/skills"
)

// The 1.x -> 2.x hop: Generic chart 1.x renders a kagent.dev/v1alpha3
// AgentTemplate, 2.x an api.kagent.dev/v1alpha3 Agent.

const (
	v1Range  = ">=1.5.0 <2.0.0"
	v2Target = "2.x"
	v2Latest = "2.0.0"
)

var (
	agentGVR          = schema.GroupVersionResource{Group: agents.KagentAPIGroup, Version: "v1alpha3", Resource: "agents"}
	apiTemplateGVR    = schema.GroupVersionResource{Group: agents.KagentAPIGroup, Version: "v1alpha3", Resource: "agenttemplates"}
	apiServerGVR      = schema.GroupVersionResource{Group: agents.KagentAPIGroup, Version: "v1alpha3", Resource: "remotemcpservers"}
	apiHarnessGVR     = schema.GroupVersionResource{Group: agents.KagentAPIGroup, Version: "v1alpha3", Resource: "harnesses"}
	apiModelConfigGVR = schema.GroupVersionResource{Group: agents.KagentAPIGroup, Version: "v1alpha3", Resource: "modelconfigs"}
)

// runnerV2 builds the command's 2.x wiring over the fake cluster: the
// service is the status reader, as in the command.
func (c *cluster) runnerV2(t *testing.T, opts Options, latest string) *Runner {
	t.Helper()
	ch := fakeChart{latest: latest}
	p := pinner{git: skills.NewResolver(fakeGitHub(t).URL, skills.StaticToken(""), nil, nil)}
	svc := agents.New(kube.NewServiceAccountProvider(c.client), ch, nil, p, agents.Config{
		DefaultNamespace: opts.Namespaces[0], ManagedNamespaces: opts.Namespaces, Compose: agents.ComposeConfig{HarnessName: "kagent"}, KagentAPIVersion: "v1alpha3",
	}, nil)
	opts.Version, opts.TargetSemver = "test", v2Target
	r, err := New(c.client, ch, p, svc, opts, nil)
	require.NoError(t, err)
	return r
}

// agentObj is what chart 2.x renders for hrNs/hrName: an Agent on the kagent
// Harness, Ready or still waiting for its golden snapshot.
func agentObj(ns, name, hrName, hrNs string, ready bool) *unstructured.Unstructured {
	readyCond := map[string]any{"type": "Ready", "status": "False", "reason": "ActorTemplatePending", "message": "waiting for the golden snapshot"}
	latest := ""
	if ready {
		readyCond = map[string]any{"type": "Ready", "status": "True", "reason": "Ready", "message": "ActorTemplate golden snapshot is ready"}
		latest = "rev-1"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": agents.KagentAPIGroup + "/v1alpha3", "kind": "Agent",
		"metadata": map[string]any{"name": name, "namespace": ns, "generation": int64(1), "labels": map[string]any{agents.HelmReleaseNameLabel: hrName, agents.HelmReleaseNamespaceLabel: hrNs}},
		"spec":     map[string]any{"harnessRef": map[string]any{"name": "kagent"}, "template": map[string]any{"description": name, "modelConfig": map[string]any{"name": "default-model-config"}}},
		"status": map[string]any{"observedGeneration": int64(1), "desiredRevision": "rev-1", "latestSuccessfulRevision": latest, "conditions": []any{
			map[string]any{"type": "Accepted", "status": "True", "reason": "Accepted"},
			map[string]any{"type": "ResolvedRefs", "status": "True", "reason": "ResolvedRefs"},
			map[string]any{"type": "Compatible", "status": "True", "reason": "Compatible"},
			readyCond,
		}},
	}}
}

func apiTemplate(ns, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": agents.KagentAPIGroup + "/v1alpha3", "kind": "AgentTemplate",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     map[string]any{"description": name},
	}}
}

// upgrade is Flux upgrading a release to chart version.
func (c *cluster) upgrade(t *testing.T, ns, name, version string) {
	t.Helper()
	hr := c.get(t, hrGVR, ns, name)
	require.NoError(t, unstructured.SetNestedSlice(hr.Object, []any{map[string]any{"version": int64(4), "chartVersion": version, "status": "deployed"}}, "status", "history"))
	require.NoError(t, unstructured.SetNestedField(hr.Object, version, "status", "lastAttemptedRevision"))
	c.update(t, hrGVR, hr)
}

// writes are the dynamic client's write actions in ns.
func (c *cluster) writes(ns string) []string {
	var out []string
	for _, a := range c.dyn.Actions() {
		switch a.GetVerb() {
		case "create", "update", "patch", "delete":
			if a.GetNamespace() == ns {
				out = append(out, a.GetVerb()+" "+a.GetResource().Resource)
			}
		}
	}
	return out
}

func gitOpsLabeled(obj *unstructured.Unstructured) *unstructured.Unstructured {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[agents.KustomizationNameLabel] = "flux"
	labels["kustomize.toolkit.fluxcd.io/namespace"] = "flux-giantswarm"
	obj.SetLabels(labels)
	return obj
}

// subAgentValues are 1.x values binding two sub-agents: one raw in
// extraTools (Shared, the 1.x default spelled out), one through the
// extraAgentSpec escape hatch.
func subAgentValues() map[string]any {
	return map[string]any{
		"agent":       map[string]any{"name": "lead", "description": "routes work"},
		"modelConfig": map[string]any{"name": "default-model-config"},
		"extraTools": []any{
			map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": "RemoteMCPServer", "name": "github"}}},
			map[string]any{"agent": map[string]any{"name": "helper", "description": "delegate chores", "templateRef": map[string]any{"name": "helper"}, "isolation": "Shared"}},
		},
		"extraAgentSpec": map[string]any{"tools": []any{
			map[string]any{"agent": map[string]any{"name": "review", "description": "review changes", "templateRef": map[string]any{"name": "reviewer"}}},
		}},
	}
}

func subAgentRewritten() map[string]any {
	return map[string]any{
		"agent":       map[string]any{"name": "lead", "description": "routes work"},
		"modelConfig": map[string]any{"name": "default-model-config"},
		"extraTools": []any{
			map[string]any{"mcp": map[string]any{"server": map[string]any{"kind": "RemoteMCPServer", "name": "github"}}},
			map[string]any{"subAgent": map[string]any{"name": "helper", "description": "delegate chores", "templateRef": map[string]any{"name": "helper"}}},
		},
		"extraAgentSpec": map[string]any{"tools": []any{
			map[string]any{"subAgent": map[string]any{"name": "review", "description": "review changes", "templateRef": map[string]any{"name": "reviewer"}}},
		}},
	}
}

func TestV2MovesANamespaceOn1xAndLeavesOneOn2x(t *testing.T) {
	ctx := t.Context()
	c := newCluster(
		ociRepository("kagent", v1Range), helmRelease("kagent", "sre", portalRewritten(), "1.5.3"),
		agentTemplate("kagent", "sre", "sre", "kagent", true), agentTemplate("kagent", "by-hand", "", "", true),
		ociRepository("tenant", v2Target), helmRelease("tenant", "bot", narrowRewritten(), "2.0.0"), agentObj("tenant", "narrow", "bot", "tenant", true),
	)

	res, err := c.runnerV2(t, twoNamespaces, v2Latest).Run(ctx)
	require.NoError(t, err)
	kagent, tenant := res.Reports[0], res.Reports[1]
	require.Equal(t, "1.x -> 2.x", kagent.Run.Path)
	rel := byRelease(kagent)["kagent/sre"]
	require.Equal(t, ActionUnchanged, rel.Action, "1.x values without a sub-agent are 2.x values")
	require.Equal(t, "already on the 2.x values", rel.Reason)
	require.Equal(t, SourceMoved, bySource(kagent)["kagent/agent"].Action)
	require.Equal(t, v2Target, c.semver(t, "kagent", "agent"))
	require.Equal(t, PhaseWait, kagent.Phase)
	require.Contains(t, strings.Join(kagent.Pending, "\n"), `release kagent/sre is deployed from chart "1.5.3"; waiting for Flux to upgrade it to 2.x`)
	leftovers := byAgent(kagent)
	require.Equal(t, AgentAwaitingUpgrade, leftovers["sre"].Action)
	require.Equal(t, "kagent.dev/v1alpha3 AgentTemplate", leftovers["sre"].Kind)
	require.Contains(t, leftovers["sre"].Reason, "Helm replaces it with the api.kagent.dev Agent")
	require.Equal(t, AgentNotMigratable, leftovers["by-hand"].Action)

	require.Equal(t, PhaseComplete, tenant.Phase, tenant.Summary)
	require.False(t, tenant.Changed)
	require.Equal(t, ActionUnchanged, byRelease(tenant)["tenant/bot"].Action)
	require.Equal(t, ActionUnchanged, bySource(tenant)["tenant/agent"].Action)
	require.Empty(t, c.writes("tenant"), "a namespace already on 2.x is not written")

	// Flux upgrades the release: Helm replaces the AgentTemplate with the
	// Agent, which compiles first.
	c.upgrade(t, "kagent", "sre", "2.0.0")
	c.delete(t, tplGVR, "kagent", "sre")
	c.create(t, agentGVR, agentObj("kagent", "sre", "sre", "kagent", false))
	res, err = c.runnerV2(t, twoNamespaces, v2Latest).Run(ctx)
	require.NoError(t, err)
	kagent = res.Reports[0]
	require.Equal(t, PhaseWait, kagent.Phase)
	require.Contains(t, strings.Join(kagent.Pending, "\n"), "api.kagent.dev Agent kagent/sre is progressing")
	require.Nil(t, kagent.Contract)

	c.delete(t, agentGVR, "kagent", "sre")
	c.create(t, agentGVR, agentObj("kagent", "sre", "sre", "kagent", true))
	res, err = c.runnerV2(t, twoNamespaces, v2Latest).Run(ctx)
	require.NoError(t, err)
	kagent = res.Reports[0]
	require.Equal(t, PhaseContract, kagent.Phase, kagent.Summary)
	require.Equal(t, []string{"by-hand"}, kagent.Contract.AgentsDeleted)
	require.Nil(t, kagent.Contract.CRDs, "the kagent.dev CRDs are the platform's to remove")
	require.False(t, c.exists(tplGVR, "kagent", "by-hand"))
	require.Equal(t, agents.VerdictReady, byRelease(kagent)["kagent/sre"].Template.Verdict)

	res, err = c.runnerV2(t, twoNamespaces, v2Latest).Run(ctx)
	require.NoError(t, err)
	require.Equal(t, PhaseComplete, res.Reports[0].Phase)
	require.False(t, res.Reports[0].Changed, "a second run on a migrated namespace changes nothing")
	require.Equal(t, PhaseComplete, c.reportConfigMap(t, "kagent")[ReportKeyPhase])
}

func TestV2GitOpsOwnedReleaseGetsTheDiff(t *testing.T) {
	values := map[string]any{
		"agent":       map[string]any{"name": "sre-agent"},
		"modelConfig": map[string]any{"name": "default-model-config"},
		"labels":      map[string]any{"team": "sre", HarnessAdmissionLabel: "claude"},
	}
	hr := gitOpsLabeled(helmRelease("flux-giantswarm", "sre-agent", values, "1.5.3"))
	require.NoError(t, unstructured.SetNestedField(hr.Object, "kagent", "spec", "targetNamespace"))
	c := newCluster(
		gitOpsLabeled(ociRepository("flux-giantswarm", v1Range)), hr,
		agentTemplate("kagent", "sre-agent", "sre-agent", "flux-giantswarm", true),
	)

	res, err := c.runnerV2(t, Options{Namespaces: []string{"kagent"}}, v2Latest).Run(t.Context())
	require.NoError(t, err)
	rep := res.Reports[0]
	rel := byRelease(rep)["flux-giantswarm/sre-agent"]
	require.Equal(t, ActionDiff, rel.Action)
	require.Equal(t, OwnershipExternal, rel.Ownership)
	require.Equal(t, []string{"labels." + HarnessAdmissionLabel}, rel.Changes.Removed)
	require.Equal(t, []string{"agent.harness=claude"}, rel.Changes.Set, "the label chose the Harness on 1.x")
	require.Contains(t, rel.Diff, "-      "+HarnessAdmissionLabel+": claude\n")
	require.Contains(t, rel.Diff, "+      harness: claude\n")
	src := bySource(rep)["flux-giantswarm/agent"]
	require.Equal(t, ActionDiff, src.Action)
	require.Contains(t, src.Diff, "-    semver: '"+v1Range+"'\n")
	require.Contains(t, src.Diff, "+    semver: "+v2Target+"\n")

	require.Equal(t, values, c.values(t, "flux-giantswarm", "sre-agent"), "a GitOps-owned release is never written")
	require.Equal(t, v1Range, c.semver(t, "flux-giantswarm", "agent"))
	require.Empty(t, c.writes("flux-giantswarm"))
	require.Equal(t, PhaseExpand, rep.Phase)
	require.Contains(t, strings.Join(rep.Pending, "\n"), "release flux-giantswarm/sre-agent: its rewrite (the diff in this report) is not applied in the owning repository yet")
}

func TestV2SubAgentBindingsMoveToSubAgent(t *testing.T) {
	c := newCluster(
		ociRepository("kagent", v1Range),
		helmRelease("kagent", "lead", subAgentValues(), "1.5.3"), helmRelease("kagent", "helper", narrowRewritten(), "1.5.3"),
		apiTemplate("kagent", "reviewer"),
	)

	res, err := c.runnerV2(t, Options{Namespaces: []string{"kagent"}}, v2Latest).Run(t.Context())
	require.NoError(t, err)
	rep := res.Reports[0]
	lead := byRelease(rep)["kagent/lead"]
	require.Equal(t, ActionRewritten, lead.Action, lead.Reason)
	require.Equal(t, subAgentRewritten(), c.values(t, "kagent", "lead"))
	require.Equal(t, []string{"extraTools[1].agent -> extraTools[1].subAgent", "extraAgentSpec.tools[0].agent -> extraAgentSpec.tools[0].subAgent"}, lead.Changes.Renamed)
	require.Equal(t, []string{"extraTools[1].agent.isolation"}, lead.Changes.Removed)
	require.Len(t, lead.Warnings, 1, "reviewer resolves; helper is rendered as an Agent, not an AgentTemplate")
	require.Contains(t, lead.Warnings[0], `sub-agent templateRef "helper" names no agenttemplates.api.kagent.dev in namespace kagent`)
	require.Equal(t, ActionUnchanged, byRelease(rep)["kagent/helper"].Action)
	require.Equal(t, SourceMoved, bySource(rep)["kagent/agent"].Action)
}

func TestV2SchemaFailureBlocksEveryWriteOfTheSource(t *testing.T) {
	broken := map[string]any{"agent": map[string]any{"name": "Not_A_DNS_Name"}, "modelConfig": map[string]any{"name": "default-model-config"}}
	c := newCluster(
		ociRepository("kagent", v1Range),
		helmRelease("kagent", "lead", subAgentValues(), "1.5.3"), helmRelease("kagent", "broken", broken, "1.5.3"),
	)
	one := Options{Namespaces: []string{"kagent"}}

	for _, dryRun := range []bool{true, false} {
		opts := one
		opts.DryRun = dryRun
		res, err := c.runnerV2(t, opts, v2Latest).Run(t.Context())
		require.NoError(t, err)
		rep := res.Reports[0]
		failed := byRelease(rep)["kagent/broken"]
		require.Equal(t, ActionFailed, failed.Action)
		require.Contains(t, failed.Reason, "the 2.x values do not satisfy the agent chart schema")
		require.Contains(t, failed.Reason, "/agent/name")
		held := byRelease(rep)["kagent/lead"]
		require.Equal(t, ActionPending, held.Action, "dryRun=%v", dryRun)
		require.Contains(t, held.Reason, "held: chart source kagent/agent also serves kagent/broken (failed)")
		require.NotNil(t, held.Changes, "the report still shows the rewrite")
		require.Equal(t, SourceNotMoved, bySource(rep)["kagent/agent"].Action)
		require.Equal(t, PhaseExpand, rep.Phase)
	}
	require.Equal(t, subAgentValues(), c.values(t, "kagent", "lead"))
	require.Equal(t, broken, c.values(t, "kagent", "broken"))
	require.Equal(t, v1Range, c.semver(t, "kagent", "agent"))
	require.Empty(t, c.writes("kagent"))
}

func TestV2DryRunBeforeTheKagentUpgrade(t *testing.T) {
	c := newCluster(
		ociRepository("kagent", v1Range),
		helmRelease("kagent", "lead", subAgentValues(), "1.5.3"), agentTemplate("kagent", "lead", "lead", "kagent", true),
	)
	c.dyn.PrependReactor("list", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if gvr := action.GetResource(); gvr.Group == agents.KagentAPIGroup {
			return true, nil, apierrors.NewNotFound(gvr.GroupResource(), "")
		}
		return false, nil, nil
	})

	res, err := c.runnerV2(t, Options{Namespaces: []string{"kagent"}, DryRun: true}, v2Latest).Run(t.Context())
	require.NoError(t, err)
	rep := res.Reports[0]
	lead := byRelease(rep)["kagent/lead"]
	require.Equal(t, ActionRewritten, lead.Action)
	require.Equal(t, "dry run: would be written", lead.Reason)
	require.Equal(t, []string{
		`sub-agent templateRef "helper" not checked: the cluster does not serve agenttemplates.api.kagent.dev yet`,
		`sub-agent templateRef "reviewer" not checked: the cluster does not serve agenttemplates.api.kagent.dev yet`,
	}, lead.Warnings)
	require.Equal(t, SourceMoved, bySource(rep)["kagent/agent"].Action)
	require.Equal(t, PhaseWait, rep.Phase)
	require.Equal(t, []string{"the cluster does not serve agents.api.kagent.dev yet: the kagent upgrade to the line chart 2.x renders for comes first"}, rep.Pending)
	require.Empty(t, c.writes("kagent"))
	require.Nil(t, c.reportConfigMap(t, "kagent"))
}

func TestV2ReleaseOn0xIsRefused(t *testing.T) {
	c := newCluster(ociRepository("kagent", legacyRange), helmRelease("kagent", "sre", portalValues(), "0.6.1"))

	res, err := c.runnerV2(t, Options{Namespaces: []string{"kagent"}}, v2Latest).Run(t.Context())
	require.NoError(t, err)
	rel := byRelease(res.Reports[0])["kagent/sre"]
	require.Equal(t, ActionFailed, rel.Action)
	require.Equal(t, "deployed from chart 0.6.1, older than the 1.x line this migration (1.x -> 2.x) starts from: migrate it to 1.x first", rel.Reason)
	require.Equal(t, portalValues(), c.values(t, "kagent", "sre"))
	require.Equal(t, SourceNotMoved, bySource(res.Reports[0])["kagent/agent"].Action)
}

func TestTargetMajor(t *testing.T) {
	for constraint, want := range map[string]uint64{
		"2.x": 2, ">=2.0.0-0 <3.0.0-0": 2, "~2.1": 2, "^2.3.0": 2, "2.0.0": 2,
		"1.x": 1, v1Range: 1, ">=0.2.1 <1.0.0": 0,
	} {
		got, err := TargetMajor(constraint)
		require.NoError(t, err, constraint)
		require.Equal(t, want, got, constraint)
	}
	for constraint, msg := range map[string]string{
		">=1.0.0":        "spans majors [1 2 3",
		">=1.5.0 <3.0.0": "spans majors [1 2]",
		"not a range":    `target chart range "not a range"`,
	} {
		_, err := TargetMajor(constraint)
		require.ErrorContains(t, err, msg, constraint)
	}
	_, err := hopFor(0, "v1alpha3")
	require.ErrorContains(t, err, "no migration path to Generic chart 0.x")
}

func TestRewriteValuesV2(t *testing.T) {
	ctx := t.Context()
	base := func(extra map[string]any) map[string]any {
		v := map[string]any{"agent": map[string]any{"name": "a"}, "modelConfig": map[string]any{"name": "default-model-config"}}
		maps.Copy(v, extra)
		return v
	}
	for _, tc := range []struct {
		name    string
		in      map[string]any
		harness string
		want    map[string]any
		changes *ValueChanges
		err     string
	}{
		{name: "1.x values are 2.x values", in: narrowRewritten(), want: narrowRewritten(), changes: &ValueChanges{}},
		{name: "sub-agents", in: subAgentValues(), want: subAgentRewritten(), changes: &ValueChanges{
			Removed: []string{"extraTools[1].agent.isolation"},
			Renamed: []string{"extraTools[1].agent -> extraTools[1].subAgent", "extraAgentSpec.tools[0].agent -> extraAgentSpec.tools[0].subAgent"},
		}},
		{name: "a rewrite is idempotent", in: subAgentRewritten(), want: subAgentRewritten(), changes: &ValueChanges{}},
		{name: "a Dedicated sub-agent has no 2.x form",
			in:  base(map[string]any{"extraTools": []any{map[string]any{"agent": map[string]any{"name": "h", "description": "d", "templateRef": map[string]any{"name": "h"}, "isolation": "Dedicated"}}}}),
			err: "extraTools[0].agent.isolation is Dedicated"},
		{name: "the admission label on the default Harness goes",
			in:   base(map[string]any{"labels": map[string]any{HarnessAdmissionLabel: "kagent"}}),
			want: base(nil), changes: &ValueChanges{Removed: []string{"labels." + HarnessAdmissionLabel}}},
		{name: "the admission label naming another Harness becomes agent.harness",
			in:      base(map[string]any{"labels": map[string]any{"team": "sre", HarnessAdmissionLabel: "claude"}}),
			want:    map[string]any{"agent": map[string]any{"name": "a", "harness": "claude"}, "modelConfig": map[string]any{"name": "default-model-config"}, "labels": map[string]any{"team": "sre"}},
			changes: &ValueChanges{Removed: []string{"labels." + HarnessAdmissionLabel}, Set: []string{"agent.harness=claude"}}},
		{name: "a platform Harness that is not the chart default is composed", in: base(nil), harness: "platform",
			want:    map[string]any{"agent": map[string]any{"name": "a", "harness": "platform"}, "modelConfig": map[string]any{"name": "default-model-config"}},
			changes: &ValueChanges{Set: []string{"agent.harness=platform"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := runtime.DeepCopyJSON(tc.in)
			got, changes, err := rewriteValuesV2(ctx, tc.in, tc.harness, nil)
			require.Equal(t, before, tc.in, "the input is never modified")
			if tc.err != "" {
				require.ErrorIs(t, err, agents.ErrInvalid)
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.changes, changes)
		})
	}
}
