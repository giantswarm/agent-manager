package agents

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/giantswarm/agent-manager/internal/chart"
	"github.com/giantswarm/agent-manager/internal/kube"
)

// TestEnvtestDryRunAgainstTheAgentCRD runs create, update and their
// validations against a kube-apiserver serving kagent's own Agent and
// RemoteMCPServer CRDs (testdata/crds, from giantswarm/kagent-upstream's
// helm/kagent-crds): what the CRD accepts passes, what it refuses comes back
// as the API server's field error, and a dry-run writes nothing. Skipped
// without envtest binaries: run via `make test-envtest`.
func TestEnvtestDryRunAgainstTheAgentCRD(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via `make test-envtest`")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"testdata/crds"},
		ErrorIfCRDPathMissing: true,
		CRDs: []*apiextensionsv1.CustomResourceDefinition{
			openCRD(hrGVR, "HelmRelease"), openCRD(ociGVR, "OCIRepository"),
			openCRD(harnessGVR, "Harness"), openCRD(mcGVR, "ModelConfig"),
		},
	}
	cfg, err := env.Start()
	require.NoError(t, err, "start envtest")
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	ctx := t.Context()
	typed, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	_, err = typed.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kagent"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	// No status subresource: the seeded status is stored as given.
	_, err = dyn.Resource(mcGVR).Namespace("kagent").Create(ctx, modelConfig("kagent", "default-model-config", "Anthropic", "claude-sonnet-4-6"), metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = dyn.Resource(harnessGVR).Namespace("kagent").Create(ctx, harness("kagent", "kagent", "kagent"), metav1.CreateOptions{})
	require.NoError(t, err)
	svc := New(kube.NewServiceAccountProvider(kube.FromInterfaces(dyn, typed, typed.Discovery())), embeddedChart{}, nil, &fakePinner{}, Config{
		DefaultNamespace: "kagent", Version: "test",
		Compose: ComposeConfig{MusterURL: "http://muster.agent-platform.svc.cluster.local:8090/mcp"},
	}, nil)
	agents := dyn.Resource(agentGVR).Namespace("kagent")
	spec := Spec{Name: "dba", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"}}

	t.Run("a tls:// origin the CRD accepts is accepted", func(t *testing.T) {
		spec := spec
		spec.Egress = []string{"tls://db.example.com:5432", "https://github.com"}
		dry, err := svc.ValidateCreate(ctx, spec)
		require.NoError(t, err)
		assert.True(t, dry.Valid, "%v", dry.Errors)
		assert.Empty(t, dry.Notes)
		_, err = svc.Create(ctx, spec)
		require.NoError(t, err)
		_, err = dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "dba", metav1.GetOptions{})
		require.NoError(t, err, "the HelmRelease is written")
		_, err = agents.Get(ctx, "dba", metav1.GetOptions{})
		assert.True(t, apierrors.IsNotFound(err), "the dry-run wrote no Agent: %v", err)
	})

	want := `Agent kagent/%s is invalid: spec.egress[0]: Too long: may not be more than 270 bytes`
	t.Run("a CRD-invalid agent returns the API server's field error", func(t *testing.T) {
		spec := spec
		spec.Name, spec.Egress = "too-long", []string{longOrigin}
		dry, err := svc.ValidateCreate(ctx, spec)
		require.NoError(t, err)
		assert.False(t, dry.Valid)
		require.Len(t, dry.Errors, 1)
		assert.Equal(t, strings.Replace(want, "%s", "too-long", 1), dry.Errors[0])
		_, err = svc.Create(ctx, spec)
		require.ErrorIs(t, err, ErrInvalid)
		assert.ErrorContains(t, err, strings.Replace(want, "%s", "too-long", 1))
		_, err = dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "too-long", metav1.GetOptions{})
		assert.True(t, apierrors.IsNotFound(err), "nothing is written: %v", err)
	})

	t.Run("an update is dry-run over the installed objects", func(t *testing.T) {
		// What helm-controller installs for dba, under its own field manager.
		hr, err := dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "dba", metav1.GetOptions{})
		require.NoError(t, err)
		values, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
		objs, err := chart.EmbeddedSchema().Render(ctx, "dba", "kagent", values)
		require.NoError(t, err)
		for _, obj := range objs {
			gvr := agentGVR
			if obj.GetKind() == "RemoteMCPServer" {
				gvr = serverGVR
			}
			_, err := dyn.Resource(gvr).Namespace("kagent").Create(ctx, obj, metav1.CreateOptions{FieldManager: "helm-controller"})
			require.NoError(t, err)
		}
		installed, err := agents.Get(ctx, "dba", metav1.GetOptions{})
		require.NoError(t, err)

		_, err = svc.Update(ctx, Update{Name: "dba", Egress: &[]string{longOrigin}})
		require.ErrorIs(t, err, ErrInvalid)
		assert.ErrorContains(t, err, strings.Replace(want, "%s", "dba", 1))
		_, err = svc.Update(ctx, Update{Name: "dba", Egress: &[]string{"tls://*.kafka.example.com:9093"}})
		require.NoError(t, err)
		after, err := agents.Get(ctx, "dba", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, installed.GetResourceVersion(), after.GetResourceVersion(), "the dry-run changed nothing")
		assert.Len(t, after.GetManagedFields(), len(installed.GetManagedFields()), "the dry-run left no field manager behind")
	})
}

// openCRD serves a kind with an open schema: the kinds agent-manager only
// reads or Flux owns.
func openCRD(gvr schema.GroupVersionResource, kind string) *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: gvr.Resource + "." + gvr.Group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: gvr.Group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: kind, ListKind: kind + "List", Plural: gvr.Resource, Singular: strings.ToLower(kind)},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: gvr.Version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &preserve}},
			}},
		},
	}
}
