package organizations

import (
	"encoding/base64"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/giantswarm/agent-manager/internal/agents"
)

const envtestKagentAPI = agents.KagentAPIGroup + "/v1alpha3"

var (
	envtestModelGVR     = schema.GroupVersionResource{Group: agents.KagentAPIGroup, Version: "v1alpha3", Resource: "modelconfigs"}
	envtestConnectorGVR = schema.GroupVersionResource{Group: agents.KagentAPIGroup, Version: "v1alpha3", Resource: "remotemcpservers"}
	envtestEgressGVR    = schema.GroupVersionResource{Group: agents.KagentAPIGroup, Version: "v1alpha3", Resource: "egresspolicies"}
)

// TestEnvtestOrganizationWritesAreStrict sends the organization writes to a
// kube-apiserver that serves kagent's own CRDs (internal/agents/testdata/crds)
// and the AgentPlatformOrganization CRD (testdata/crds, from
// agent-platform-operator's chart). A field the schema does not have is
// refused by the dry run, which apply mode and the server-side check of
// commit mode both run, as an invalid request that names the field; the
// objects the organization tools write today pass, created and updated.
// Skipped without envtest binaries: run via `make test-envtest`.
func TestEnvtestOrganizationWritesAreStrict(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via `make test-envtest`")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../agents/testdata/crds", "testdata/crds"},
		ErrorIfCRDPathMissing: true,
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
	const ns, parent = "agents-payments-x7k2q", "org-acme"
	for _, name := range []string{ns, parent} {
		_, err = typed.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	t.Run("an unknown ModelConfig field is refused and names the field", func(t *testing.T) {
		// What a model file in git, or a live object changed by hand, may
		// carry: OpenAI's block has no promptCaching.
		obj := newObject(envtestKagentAPI, "ModelConfig", ns, "gpt")
		obj.Object["spec"] = map[string]any{"provider": "OpenAI", "model": "gpt-5", "openAI": map[string]any{"promptCaching": true}}
		w := write{gvr: envtestModelGVR, kind: "ModelConfig", op: opCreate, obj: obj.Object, ns: ns, name: "gpt", what: "ModelConfig " + ns + "/gpt"}
		for _, dryRun := range []bool{true, false} {
			err := apply(ctx, dyn, []write{w}, dryRun)
			require.ErrorIs(t, err, agents.ErrInvalid, "dryRun %v", dryRun)
			assert.ErrorContains(t, err, "spec.openAI.promptCaching")
			assert.ErrorContains(t, err, "dry run of create ModelConfig "+ns+"/gpt")
		}
		_, err := dyn.Resource(envtestModelGVR).Namespace(ns).Get(ctx, "gpt", metav1.GetOptions{})
		assert.True(t, apierrors.IsNotFound(err), "nothing is written: %v", err)

		// The update of a live model is refused the same way.
		obj.Object["spec"] = map[string]any{"provider": "OpenAI", "model": "gpt-5"}
		w.obj = obj.Object
		require.NoError(t, apply(ctx, dyn, []write{w}, false))
		live, err := dyn.Resource(envtestModelGVR).Namespace(ns).Get(ctx, "gpt", metav1.GetOptions{})
		require.NoError(t, err)
		require.NoError(t, unstructured.SetNestedField(live.Object, true, "spec", "openAI", "promptCaching"))
		update := write{gvr: envtestModelGVR, kind: "ModelConfig", op: opUpdate, obj: live.Object, ns: ns, name: "gpt", what: "ModelConfig " + ns + "/gpt"}
		err = apply(ctx, dyn, []write{update}, false)
		require.ErrorIs(t, err, agents.ErrInvalid)
		assert.ErrorContains(t, err, "spec.openAI.promptCaching")
	})

	// Each write below is built by the code the organization tools use, or
	// in the shape they write; each is created, then updated from the live
	// object, as an update of the tools does.
	writes := map[string]write{}
	for name, req := range map[string]ModelRequest{
		"claude":  {Provider: "Anthropic", Model: "claude-sonnet-5-5", MaxTokens: ptr(int64(32000)), DisplayName: ptr("Claude Sonnet")},
		"gpt-4":   {Provider: "OpenAI", Model: "gpt-4o", MaxTokens: ptr(int64(4096)), Endpoint: ptr("http://llm.example.com/v1"), InsecureSkipTLSVerify: ptr(true)},
		"local":   {Provider: "Ollama", Model: "qwen3", Endpoint: ptr("http://ollama:11434")},
		"vertex":  {Provider: "GeminiVertexAI", Model: "gemini-2.5-pro", ProjectID: "my-project", Location: "europe-west1"},
		"mistral": {Provider: "Mistral", Model: "mistral-large", MaxTokens: ptr(int64(2048))},
	} {
		obj := newObject(envtestKagentAPI, "ModelConfig", ns, name)
		require.NoError(t, unstructured.SetNestedField(obj.Object, req.Provider, "spec", "provider"))
		require.NoError(t, unstructured.SetNestedField(obj.Object, req.Model, "spec", "model"))
		require.NoError(t, configureModel(obj, req), name)
		require.NoError(t, unstructured.SetNestedField(obj.Object, CredentialPrefix+name, "spec", "apiKeySecret"))
		require.NoError(t, unstructured.SetNestedField(obj.Object, modelCredentialKey(obj), "spec", "apiKeySecretKey"))
		require.NoError(t, setModelSettings(obj, req, req.Provider, true, nil), name)
		setMark(obj)
		writes["ModelConfig "+name] = write{gvr: envtestModelGVR, kind: "ModelConfig", obj: obj.Object, ns: ns, name: name}
	}
	connector := newObject(envtestKagentAPI, "RemoteMCPServer", ns, "github")
	connector.Object["spec"] = map[string]any{
		"url": "https://mcp.example.com/mcp", "description": "MCP server github",
		"headersFrom": []any{
			map[string]any{"name": "X-Team", "value": "payments"},
			map[string]any{"name": "Authorization", "valueFrom": map[string]any{"type": "Secret", "name": CredentialPrefix + "github", "key": defaultTokenKey}},
		},
	}
	writes["RemoteMCPServer github"] = write{gvr: envtestConnectorGVR, kind: "RemoteMCPServer", obj: connector.Object, ns: ns, name: "github"}
	egress := newObject(envtestKagentAPI, "EgressPolicy", ns, "apis")
	egress.Object["spec"] = map[string]any{"destinations": []any{
		map[string]any{"origin": "https://api.example.com"},
		map[string]any{"origin": "https://api.github.com", "authentication": map[string]any{
			"header": "Authorization", "prefix": "Bearer ", "secretKeyRef": map[string]any{"name": CredentialPrefix + "apis", "key": defaultTokenKey},
		}},
	}}
	writes["EgressPolicy apis"] = write{gvr: envtestEgressGVR, kind: "EgressPolicy", obj: egress.Object, ns: ns, name: "apis"}
	secret := credentialSecretWrite(ns, CredentialPrefix+"claude", "", map[string]any{defaultModelKey: base64.StdEncoding.EncodeToString([]byte("not-a-key"))})
	writes["Secret "+secret.name] = secret
	writes["RoleBinding organization-admins"] = write{gvr: rbacGVR, kind: kindRoleBinding, ns: ns, name: "organization-admins",
		obj: roleBinding(ns, "organization-admins", RoleAdmin, []Subject{{Kind: "Group", Name: "payments-admins"}, {Kind: "ServiceAccount", Name: "deployer", Namespace: ns}})}
	replicas := int32(2)
	writes["AgentPlatformOrganization payments"] = write{gvr: GVR, kind: orgKind, ns: parent, name: "payments",
		obj: (&Service{}).organizationObject(CreateRequest{Parent: parent, Name: "payments", DisplayName: "Payments", Harnesses: []string{"kagent"}, WorkerReplicas: &replicas})}

	for what, w := range writes {
		t.Run(what+" passes strict validation", func(t *testing.T) {
			w.what, w.op = what, opCreate
			require.NoError(t, apply(ctx, dyn, []write{w}, false), "create")
			live, err := dyn.Resource(w.gvr).Namespace(w.ns).Get(ctx, w.name, metav1.GetOptions{})
			require.NoError(t, err)
			w.op, w.obj = opUpdate, live.Object
			require.NoError(t, apply(ctx, dyn, []write{w}, false), "update of the live object")
		})
	}
}

func ptr[T any](v T) *T { return &v }
