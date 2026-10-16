package agents

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCreateInATenantNamespaceNamesTheDefaultModelConfig: before a tenant's
// first agent its namespace holds no ModelConfig, and the platform default it
// receives with that agent is the one valid name; the refusal names it.
func TestCreateInATenantNamespaceNamesTheDefaultModelConfig(t *testing.T) {
	f := newFixture(t, nil)
	withTenants(f, &fakeTenants{namespaces: []string{"org-acme"}})

	_, err := f.svc.Create(context.Background(), Spec{
		Location: Location{Namespace: "org-acme"}, Name: "sre", ModelConfig: "nope", Toolset: []string{"preset:none"},
	})
	require.ErrorContains(t, err, `modelConfig "nope" does not exist in namespace org-acme; valid: default-model-config (the platform default, provided with the first agent)`)
}
