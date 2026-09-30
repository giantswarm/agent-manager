package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/agent-manager/internal/kube"
)

const (
	targetMuster = "https://muster.installation.example/mcp"
	wcKubeconfig = "apiVersion: v1\nkind: Config\n# the workload cluster wc1\n"
)

var wc1 = Target{Organization: "acme", Cluster: "wc1"}

// targetFixture is an installation (mc) with the organization namespace
// org-acme holding cluster wc1's kubeconfig Secret, and the workload cluster
// (wc) running the runtime slice: a ModelConfig and the platform Harness in
// kagent. The installation has no ModelConfig of that name, so a check on the
// wrong cluster fails.
type targetFixture struct {
	mc, wc *dynamicfake.FakeDynamicClient
	svc    *Service
	// kubeconfigs are the kubeconfigs the remote clients were built from.
	kubeconfigs []string
}

func newTargetFixture(t *testing.T, targetMusterURL string) *targetFixture {
	t.Helper()
	f := &targetFixture{}
	f.mc = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	f.wc = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds,
		modelConfig("kagent", "wc-model", "OpenAI", "qwen3.5:9b"),
		harness("kagent", "kagent", "kagent"),
	)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wc1-kubeconfig", Namespace: "org-acme"},
		Data:       map[string][]byte{KubeconfigSecretKey: []byte(wcKubeconfig)},
	}
	mcTyped := kubefake.NewClientset(secret)
	wcTyped := kubefake.NewClientset()
	mc := kube.FromInterfaces(f.mc, mcTyped, mcTyped.Discovery())
	wc := kube.FromInterfaces(f.wc, wcTyped, wcTyped.Discovery())
	provider := kube.NewServiceAccountProvider(mc).WithRemote(func(_ context.Context, kubeconfig []byte) (kube.Client, error) {
		f.kubeconfigs = append(f.kubeconfigs, string(kubeconfig))
		return wc, nil
	})
	f.svc = New(provider, embeddedChart{}, nil, &fakePinner{}, Config{
		DefaultNamespace: "kagent", Version: "test",
		Compose: ComposeConfig{ // #nosec G101 -- test fixture, Secret names only
			MusterURL:               "http://muster.agent-platform.svc.cluster.local:8090/mcp",
			TargetMusterURL:         targetMusterURL,
			ServiceAccountName:      "kagent-flux",
			SkillsGitAuthSecretName: "skills-git-auth",
		},
	}, nil)
	return f
}

func targetSpec(name string) Spec {
	return Spec{
		Location: Location{Namespace: "kagent", Target: wc1}, Name: name, ModelConfig: "wc-model", Toolset: []string{"preset:read-only"},
		Skills: Skills{{Name: "a", Path: "a", Git: &GitSkill{URL: skillsRepo, Commit: mainHead}}},
	}
}

func TestCreateOnATargetClusterPlacesTheReleaseThroughItsKubeconfig(t *testing.T) {
	ctx := context.Background()
	f := newTargetFixture(t, targetMuster)

	res, err := f.svc.Create(ctx, targetSpec("planner"))
	require.NoError(t, err)
	assert.Equal(t, []string{wcKubeconfig}, f.kubeconfigs[:1], "the workload cluster client is built from the cluster's Cluster API Secret")
	assert.True(t, res.Created.OCIRepository)
	assert.Equal(t, wc1, res.Agent.Target)

	hr, err := f.mc.Resource(hrGVR).Namespace("org-acme").Get(ctx, "wc1-planner", metav1.GetOptions{})
	require.NoError(t, err, "the HelmRelease lives on the installation, in the organization namespace, named after cluster and agent")
	spec := hr.Object["spec"].(map[string]any)
	assert.Equal(t, map[string]any{"secretRef": map[string]any{"name": "wc1-kubeconfig", "key": "value"}}, spec["kubeConfig"])
	assert.Equal(t, "planner", spec["releaseName"])
	assert.Equal(t, "kagent", spec["targetNamespace"])
	assert.Equal(t, "kagent", spec["storageNamespace"])
	assert.NotContains(t, spec, "serviceAccountName", "Flux would impersonate it on the workload cluster, where it does not exist")
	assert.Equal(t, "org-acme", spec["chartRef"].(map[string]any)["namespace"])
	assert.Equal(t, "wc1", hr.GetLabels()[ClusterLabel])
	assert.Equal(t, ManagedByValue, hr.GetLabels()[ManagedByLabel])

	values := mustValues(hr)
	assert.Equal(t, map[string]any{"url": targetMuster}, values["muster"], "an agent on a workload cluster reaches muster at the installation's public URL")
	assert.NotContains(t, values, SkillsGitAuthValuesKey, "the installation's skills credential exists on the installation only")

	_, err = f.mc.Resource(ociGVR).Namespace("org-acme").Get(ctx, "agent", metav1.GetOptions{})
	require.NoError(t, err, "the chart source is shared in the organization namespace")
	list, err := f.wc.Resource(hrGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, list.Items, "nothing of Flux is written to the workload cluster")

	_, err = f.svc.Create(ctx, targetSpec("planner"))
	assert.ErrorIs(t, err, ErrConflict, "the name is taken on that cluster")
}

func TestAnAgentOnATargetClusterIsReadThere(t *testing.T) {
	ctx := context.Background()
	f := newTargetFixture(t, targetMuster)
	_, err := f.svc.Create(ctx, targetSpec("planner"))
	require.NoError(t, err)
	// helm-controller renders the template on the workload cluster with the
	// provenance labels of the installation's release.
	_, err = f.wc.Resource(tplGVR).Namespace("kagent").Create(ctx, agentTemplate("kagent", "planner", "wc1-planner", "org-acme", true, true), metav1.CreateOptions{})
	require.NoError(t, err)
	at := Location{Namespace: "kagent", Target: wc1}

	a, err := f.svc.Get(ctx, at, "planner")
	require.NoError(t, err)
	assert.True(t, a.Exists)
	require.NotNil(t, a.Ready)
	assert.True(t, *a.Ready)
	assert.Equal(t, wc1, a.Target)
	require.NotNil(t, a.HelmRelease)
	assert.Equal(t, "org-acme", a.HelmRelease.Namespace)
	assert.Equal(t, "wc1-planner", a.HelmRelease.Name)

	st, err := f.svc.Status(ctx, at, "planner")
	require.NoError(t, err)
	assert.Equal(t, VerdictReady, st.Verdict, st.Summary)
	assert.Equal(t, wc1, st.Target)

	agents, err := f.svc.List(ctx, at)
	require.NoError(t, err)
	require.Len(t, agents, 1)
	assert.Equal(t, "planner", agents[0].Name)

	local, err := f.svc.List(ctx, In("kagent"))
	require.NoError(t, err)
	assert.Empty(t, local, "the installation's own cluster has no agent of that name")

	mcs, err := f.svc.ListModelConfigs(ctx, at)
	require.NoError(t, err)
	require.Len(t, mcs, 1)
	assert.Equal(t, "wc-model", mcs[0].Name)

	del, err := f.svc.Delete(ctx, at, "planner", false, WriteOptions{})
	require.NoError(t, err)
	assert.True(t, del.HelmReleaseDeleted)
	assert.True(t, del.OCIRepositoryDeleted)
	_, err = f.mc.Resource(hrGVR).Namespace("org-acme").Get(ctx, "wc1-planner", metav1.GetOptions{})
	assert.Error(t, err)
}

func TestTargetClusterRefusals(t *testing.T) {
	ctx := context.Background()
	f := newTargetFixture(t, targetMuster)
	for name, tc := range map[string]struct {
		spec Spec
		want string
	}{
		"half a target": {
			spec: func() Spec { s := targetSpec("planner"); s.Cluster = ""; return s }(),
			want: "needs both organization and cluster",
		},
		"no kubeconfig Secret": {
			spec: func() Spec { s := targetSpec("planner"); s.Cluster = "wc2"; return s }(),
			want: "has no kubeconfig Secret org-acme/wc2-kubeconfig",
		},
		"release name too long": {
			spec: targetSpec(strings.Repeat("a", 60)),
			want: "longer than 63 characters",
		},
		"model config of the installation only": {
			spec: func() Spec { s := targetSpec("planner"); s.ModelConfig = "default-model-config"; return s }(),
			want: `modelConfig "default-model-config" does not exist in namespace kagent`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.svc.Create(ctx, tc.spec)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalid)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	noMuster := newTargetFixture(t, "")
	_, err := noMuster.svc.Create(ctx, targetSpec("planner"))
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "no muster URL for agents on workload clusters")
}

func TestPlaceOnClusterLeavesALocalReleaseAlone(t *testing.T) {
	values := map[string]any{"agent": map[string]any{"name": "planner"}}
	local := BuildHelmRelease("planner", "kagent", values, ComposeConfig{ServiceAccountName: "kagent-flux"})
	placed := BuildHelmRelease("planner", "kagent", values, ComposeConfig{ServiceAccountName: "kagent-flux"})
	PlaceOnCluster(placed, wc1, "planner", "kagent")

	assert.Equal(t, "kagent-flux", local.Object["spec"].(map[string]any)["serviceAccountName"])
	assert.NotContains(t, local.Object["spec"], "kubeConfig")
	assert.Equal(t, "wc1-planner", placed.GetName())
	assert.Equal(t, "org-acme", placed.GetNamespace())
	secret, ns := placedOn(placed)
	assert.Equal(t, "wc1-kubeconfig", secret)
	assert.Equal(t, "kagent", ns)
	secret, _ = placedOn(local)
	assert.Empty(t, secret)
}
