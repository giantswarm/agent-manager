package cmd

import (
	"errors"
	"log/slog"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/kube"
)

// envVar is the "(ENV_VAR)" every flag's usage ends with.
var envVar = regexp.MustCompile(`\(([A-Z0-9_]+)\)$`)

// TestMigrateSharesServeFlags: a Job or an operator configures migrate with
// the same flags and environment variables serve is configured with — same
// names, same defaults, same variables — plus the migration's own.
func TestMigrateSharesServeFlags(t *testing.T) {
	serve, migrate := newServeCmd().Flags(), newMigrateCmd().Flags()
	shared := []string{
		"kubeconfig", "kube-context", "in-cluster", "kagent-namespace", "managed-namespaces", "kagent-api-version", "harness-name",
		"flux-helmrelease-api-version", "flux-ocirepository-api-version", "agent-chart-oci-url", "agent-chart-semver", "skills-github-api", "skills-github-token",
	}
	for _, name := range shared {
		s, m := serve.Lookup(name), migrate.Lookup(name)
		require.NotNil(t, s, "serve --%s", name)
		require.NotNil(t, m, "migrate --%s", name)
		assert.Equal(t, s.DefValue, m.DefValue, "--%s default", name)
		assert.Equal(t, envVar.FindStringSubmatch(s.Usage), envVar.FindStringSubmatch(m.Usage), "--%s environment variable", name)
	}
	for name, env := range map[string]string{"dry-run": "AGENT_MANAGER_MIGRATE_DRY_RUN", "report-configmap": "AGENT_MANAGER_MIGRATE_REPORT_CONFIGMAP", "gitops-namespaces": "AGENT_MANAGER_MIGRATE_GITOPS_NAMESPACES"} {
		m := migrate.Lookup(name)
		require.NotNil(t, m, "migrate --%s", name)
		assert.Equal(t, env, envVar.FindStringSubmatch(m.Usage)[1], "--%s environment variable", name)
	}
	assert.Equal(t, "agent-manager-migrate-report", migrate.Lookup("report-configmap").DefValue)
	assert.Equal(t, "false", migrate.Lookup("dry-run").DefValue)
}

// TestMigrateKagentVersionOnThe3xPath: chart 3.x renders the api.kagent.dev
// Agent as 2.x does, so the 2.x -> 3.x path needs that resource served and a
// dry run goes ahead without it.
func TestMigrateKagentVersionOnThe3xPath(t *testing.T) {
	typed := kubefake.NewClientset()
	clients := kube.FromInterfaces(nil, typed, typed.Discovery())

	_, err := migrateKagentVersion(clients, &migrateOptions{kagentAPIVersion: "auto"}, 3, slog.Default())
	require.ErrorContains(t, err, "the target chart 3.x renders agents of group "+agents.KagentAPIGroup)

	version, err := migrateKagentVersion(clients, &migrateOptions{kagentAPIVersion: "auto", dryRun: true}, 3, slog.Default())
	require.NoError(t, err)
	assert.Equal(t, agents.DefaultKagentAPIVersion, version)
}

// TestMigrateOrganizationsOnThe3xPath: auto lists the labelled namespaces on
// the 3.x path only; a list the run may not read fails it.
func TestMigrateOrganizationsOnThe3xPath(t *testing.T) {
	typed := kubefake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "org-acme", Labels: map[string]string{"agent-platform.giantswarm.io/organization": "acme"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kagent"}},
	)
	clients := kube.FromInterfaces(nil, typed, typed.Discovery())
	orgs, err := migrateOrganizations(t.Context(), clients, "auto", 3, []string{"kagent"})
	require.NoError(t, err)
	assert.Equal(t, []string{"org-acme"}, orgs)
	orgs, err = migrateOrganizations(t.Context(), clients, "auto", 2, []string{"kagent"})
	require.NoError(t, err)
	assert.Empty(t, orgs)
	orgs, err = migrateOrganizations(t.Context(), clients, "none", 3, nil)
	require.NoError(t, err)
	assert.Empty(t, orgs)

	typed.PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", errors.New("no"))
	})
	_, err = migrateOrganizations(t.Context(), clients, "auto", 3, nil)
	require.ErrorContains(t, err, "nothing was changed")
}
