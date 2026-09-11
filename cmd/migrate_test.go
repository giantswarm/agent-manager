package cmd

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
