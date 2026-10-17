package tenants

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/kubernetes"
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

	env := startEnvtest(t)
	ctx := t.Context()
	admin, mapper, policyName, reconciler := env.admin, env.mapper, env.policyName, env.reconciler

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

	sa, err := kubernetes.NewForConfig(env.as(reconciler))
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

// TestSkillsTokenPolicyEnvtest: while minting, agent-manager refreshes the
// skills boot Secret under its current name and every previous name in each
// managed namespace (templates/skills-token.yaml). The tenant policy admits
// those updates in the platform namespace, which is no tenant, and refuses
// the update of any other Secret there.
func TestSkillsTokenPolicyEnvtest(t *testing.T) {
	const previous = "old-skills-token" // #nosec G101 -- a Secret name, not a credential
	env := startEnvtest(t, mintSkillsToken, "skills.github.app.secretName=skills-app",
		"skills.previousGitAuthSecretNames={"+previous+"}")
	ctx := t.Context()

	_, err := env.admin.CoreV1().Secrets("kagent").Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "other"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	// RBAC alone would refuse the update of another Secret before admission:
	// grant it, so the policy is what refuses it.
	_, err = env.admin.RbacV1().Roles("kagent").Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "every-secret"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"update"}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = env.admin.RbacV1().RoleBindings("kagent").Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "every-secret"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "every-secret"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: env.reconciler}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	sa, err := kubernetes.NewForConfig(env.as(env.reconciler))
	require.NoError(t, err)
	refresh := func(name string) error {
		_, err := sa.CoreV1().Secrets("kagent").Update(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			StringData: map[string]string{"token": "fresh"},
		}, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
		return err
	}

	// The binding takes effect once the API server's policy informer sees
	// it: wait for the first refusal.
	require.Eventually(t, func() bool {
		err := refresh("other")
		return err != nil && strings.Contains(err.Error(), "only in namespaces labelled giantswarm.io/organization; kagent is not one")
	}, 30*time.Second, 200*time.Millisecond, "the policy never refused another Secret in the platform namespace")

	for _, name := range []string{"agent-manager-skills-token", "tenant-credential-agent-manager-skills-token", previous} {
		t.Run("refresh "+name, func(t *testing.T) {
			require.NoError(t, refresh(name))
		})
	}
}
