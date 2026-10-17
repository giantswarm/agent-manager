package tenants

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// keySecret names the default ModelConfig's key Secret.
const keySecret = "kagent-anthropic" // #nosec G101 -- a Secret name, not a credential

// releaseNamespace is the namespace the chart is rendered for.
const releaseNamespace = "agent-platform"

// testEnv is a kube-apiserver holding what the chart renders for the tenant
// reconciler (templates/tenants-rbac.yaml) and the CRDs of the kinds it
// keeps.
type testEnv struct {
	cfg        *rest.Config
	admin      kubernetes.Interface
	dyn        dynamic.Interface
	mapper     meta.RESTMapper
	policyName string
	// reconciler is the ServiceAccount's user name the chart binds.
	reconciler string
}

// mintSkillsToken is the --set value that also renders the skills boot
// Secrets and their Role (templates/skills-token.yaml).
const mintSkillsToken = "skills.mintGitAuthSecret=true" // #nosec G101 -- a Helm value, not a credential

// startEnvtest starts the API server and applies the rendered chart, with
// helmSet as further --set values; with mintSkillsToken among them it applies
// templates/skills-token.yaml as well. Skipped without envtest binaries: run via
// `make test-envtest`, which resolves KUBEBUILDER_ASSETS with setup-envtest
// (and needs helm).
func startEnvtest(t *testing.T, helmSet ...string) *testEnv {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via `make test-envtest`")
	}
	args := []string{"template", "agent-manager", "../../helm/agent-manager",
		"--namespace", releaseNamespace,
		"--show-only", "templates/tenants-rbac.yaml",
		"--set", "tenants.enabled=true",
		"--set", "flux.helmReleaseServiceAccount=agents-flux",
		"--set", "tenants.modelConfig.apiKeySecretName=" + keySecret,
		"--set", "oauth.enabled=true,oauth.downstream.enabled=true,oauth.existingSecret=oauth",
		"--set", "oauth.baseURL=https://agent-manager.example.com,oauth.dex.issuerURL=https://dex.example.com,oauth.dex.clientID=platform",
	}
	if slices.Contains(helmSet, mintSkillsToken) {
		args = append(args, "--show-only", "templates/skills-token.yaml")
	}
	for _, s := range helmSet {
		args = append(args, "--set", s)
	}
	rendered, err := exec.Command("helm", args...).Output()
	require.NoError(t, err, "helm template: %s", stderrOf(err))

	env := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{
		crd("helm.toolkit.fluxcd.io", "v2", "HelmRelease", "helmreleases"),
		crd("source.toolkit.fluxcd.io", "v1", "OCIRepository", "ocirepositories"),
		crd("api.kagent.dev", "v1alpha3", "Agent", "agents"),
		crd("api.kagent.dev", "v1alpha3", "Harness", "harnesses"),
		crd("api.kagent.dev", "v1alpha3", "ModelConfig", "modelconfigs"),
		workerPoolCRD(),
	}}
	cfg, err := env.Start()
	require.NoError(t, err, "start envtest")
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	ctx := t.Context()
	te := &testEnv{cfg: cfg}
	admin, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	te.admin = admin
	te.dyn, err = dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	groups, err := restmapper.GetAPIGroupResources(admin.Discovery())
	require.NoError(t, err)
	te.mapper = restmapper.NewDiscoveryRESTMapper(groups)

	_, err = admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: releaseNamespace}}, metav1.CreateOptions{})
	require.NoError(t, err)
	// Apply every rendered object; the API server compiles the policy's CEL
	// when it stores it.
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
	for {
		u := &unstructured.Unstructured{}
		if err := decoder.Decode(&u.Object); errors.Is(err, io.EOF) {
			break
		} else {
			require.NoError(t, err)
		}
		if len(u.Object) == 0 {
			continue
		}
		gvk := u.GroupVersionKind()
		mapping, err := te.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		require.NoError(t, err)
		var res dynamic.ResourceInterface = te.dyn.Resource(mapping.Resource)
		if ns := u.GetNamespace(); ns != "" {
			_, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
			if !apierrors.IsAlreadyExists(err) {
				require.NoError(t, err)
			}
			res = te.dyn.Resource(mapping.Resource).Namespace(ns)
		}
		_, err = res.Create(ctx, u, metav1.CreateOptions{})
		require.NoError(t, err, "create %s %s", gvk.Kind, u.GetName())
		switch gvk.Kind {
		case "ValidatingAdmissionPolicy":
			te.policyName = u.GetName()
		case "ClusterRoleBinding":
			subject, _, _ := unstructured.NestedSlice(u.Object, "subjects")
			sa := subject[0].(map[string]any)
			te.reconciler = "system:serviceaccount:" + sa["namespace"].(string) + ":" + sa["name"].(string)
		}
	}
	require.NotEmpty(t, te.policyName, "the chart renders no ValidatingAdmissionPolicy")
	require.NotEmpty(t, te.reconciler, "the chart renders no ClusterRoleBinding for the reconciler")
	return te
}

// as is the API server's config impersonating user.
func (e *testEnv) as(user string) *rest.Config {
	cfg := rest.CopyConfig(e.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user}
	return cfg
}

// crd is a namespaced CustomResourceDefinition that takes any fields.
func crd(group, version, kind, plural string) *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + "." + group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: kind, ListKind: kind + "List", Plural: plural, Singular: strings.ToLower(kind)},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: &preserve,
				}},
			}},
		},
	}
}

// workerPoolCRD is the WorkerPool CRD with a defaulted spec field, so the API
// server adds a field to every WorkerPool it stores, as the real CRD does.
func workerPoolCRD() *apiextensionsv1.CustomResourceDefinition {
	c := crd("ate.dev", "v1alpha1", "WorkerPool", "workerpools")
	preserve := true
	c.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties = map[string]apiextensionsv1.JSONSchemaProps{
		"spec": {Type: "object", XPreserveUnknownFields: &preserve, Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"sandboxClass": {Type: "string", Default: &apiextensionsv1.JSON{Raw: []byte(`"gvisor"`)}},
		}},
	}
	return c
}

func stderrOf(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(exit.Stderr)
	}
	return ""
}
