package tenants

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestTenantPolicyEnvtest installs what the chart renders for the tenant
// reconciler (helm/agent-manager/templates/tenants-rbac.yaml) in a real
// kube-apiserver: the API server accepts the ValidatingAdmissionPolicy, the
// type check the policy status controller runs finds nothing to warn about,
// and, as the reconciler's ServiceAccount, the writes the reconciler makes in
// a tenant pass (their CEL evaluates within the cost limit) while a write it
// must not make is refused with the policy's own message.
//
// Requires envtest binaries and helm: run via `make test-envtest`, which
// resolves KUBEBUILDER_ASSETS with setup-envtest. Skipped otherwise.
func TestTenantPolicyEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via `make test-envtest`")
	}

	rendered, err := exec.Command("helm", "template", "agent-manager", "../../helm/agent-manager",
		"--namespace", "agent-platform",
		"--show-only", "templates/tenants-rbac.yaml",
		"--set", "tenants.enabled=true",
		"--set", "flux.helmReleaseServiceAccount=agents-flux",
		"--set", "tenants.modelConfig.apiKeySecretName=kagent-anthropic",
		"--set", "oauth.enabled=true,oauth.downstream.enabled=true,oauth.existingSecret=oauth",
		"--set", "oauth.baseURL=https://agent-manager.example.com,oauth.dex.issuerURL=https://dex.example.com,oauth.dex.clientID=platform",
	).Output()
	require.NoError(t, err, "helm template: %s", stderrOf(err))

	testEnv := &envtest.Environment{}
	cfg, err := testEnv.Start()
	require.NoError(t, err, "start envtest")
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	ctx := t.Context()
	admin, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	groups, err := restmapper.GetAPIGroupResources(admin.Discovery())
	require.NoError(t, err)
	mapper := restmapper.NewDiscoveryRESTMapper(groups)

	// Apply every rendered object; the API server compiles the policy's CEL
	// when it stores it.
	var policyName, reconciler string
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
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		require.NoError(t, err)
		_, err = dyn.Resource(mapping.Resource).Create(ctx, u, metav1.CreateOptions{})
		require.NoError(t, err, "create %s %s", gvk.Kind, u.GetName())
		switch gvk.Kind {
		case "ValidatingAdmissionPolicy":
			policyName = u.GetName()
		case "ClusterRoleBinding":
			subject, _, _ := unstructured.NestedSlice(u.Object, "subjects")
			sa := subject[0].(map[string]any)
			reconciler = "system:serviceaccount:" + sa["namespace"].(string) + ":" + sa["name"].(string)
		}
	}
	require.NotEmpty(t, policyName, "the chart renders no ValidatingAdmissionPolicy")
	require.NotEmpty(t, reconciler, "the chart renders no ClusterRoleBinding for the reconciler")

	// The check kube-controller-manager writes to status.typeChecking; envtest
	// runs no controller manager, so it runs here against the same API server.
	policy, err := admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, policyName, metav1.GetOptions{})
	require.NoError(t, err)
	checker := &validating.TypeChecker{
		SchemaResolver: &resolver.ClientDiscoveryResolver{Discovery: admin.Discovery()},
		RestMapper:     mapper,
	}
	require.Empty(t, checker.Check(policy), "type check of %s", policyName)
	// The checker skips a rule it cannot resolve to a kind (apiVersions "*",
	// for one), so an empty result only counts if a wrong field is caught.
	probe := policy.DeepCopy()
	probe.Spec.Validations = append(probe.Spec.Validations, admissionv1.Validation{Expression: "object.noSuchField == 'x'"})
	require.NotEmpty(t, checker.Check(probe), "the type check resolved no kind of %s", policyName)

	for _, ns := range []*corev1.Namespace{
		{ObjectMeta: metav1.ObjectMeta{Name: "org-acme", Labels: map[string]string{"giantswarm.io/organization": "acme"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "plain"}},
	} {
		_, err := admin.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	asReconciler := rest.CopyConfig(cfg)
	asReconciler.Impersonate = rest.ImpersonationConfig{UserName: reconciler}
	sa, err := kubernetes.NewForConfig(asReconciler)
	require.NoError(t, err)
	dryRun := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	secret := func(ns, name string) error {
		_, err := sa.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name}}, dryRun)
		return err
	}
	fluxBinding := func(subjectNamespace string) error {
		_, err := sa.RbacV1().RoleBindings("org-acme").Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "agents-flux"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "agent-platform-tenant-flux"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "agents-flux", Namespace: subjectNamespace}},
		}, dryRun)
		return err
	}

	// The binding takes effect once the API server's policy informer sees
	// it: wait for the first refusal.
	require.Eventually(t, func() bool {
		err := secret("org-acme", "cluster-kubeconfig")
		return err != nil && strings.Contains(err.Error(), "agent-manager copies only the Secret")
	}, 30*time.Second, 200*time.Millisecond, "the policy never refused a Secret the reconciler does not copy")

	tests := []struct {
		name     string
		write    func() error
		rejected string
	}{
		{name: "the model key into a tenant", write: func() error { return secret("org-acme", keySecret) }},
		{name: "the flux binding to the tenant's ServiceAccount", write: func() error { return fluxBinding("org-acme") }},
		{
			name:     "another Secret into a tenant",
			write:    func() error { return secret("org-acme", "cluster-kubeconfig") },
			rejected: "agent-manager copies only the Secret " + keySecret + " into a tenant, not cluster-kubeconfig",
		},
		{
			name:     "the model key into an unlabelled namespace",
			write:    func() error { return secret("plain", keySecret) },
			rejected: "only in namespaces labelled giantswarm.io/organization; plain is not one",
		},
		{
			name:     "the flux binding to a ServiceAccount of another namespace",
			write:    func() error { return fluxBinding("kube-system") },
			rejected: "agent-manager binds only the ClusterRole agent-platform-tenant-flux to the ServiceAccount agents-flux of the tenant",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.write()
			if tt.rejected == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, "ValidatingAdmissionPolicy '"+policyName+"'")
			require.ErrorContains(t, err, tt.rejected)
		})
	}
}

func stderrOf(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(exit.Stderr)
	}
	return ""
}
