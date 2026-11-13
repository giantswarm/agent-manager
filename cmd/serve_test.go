package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/oci"
	"github.com/giantswarm/agent-manager/internal/server"
)

func TestToolchainSourceReadsTheRegistryConfig(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, ".dockerconfigjson")
	// A fixture: no registry knows these credentials.
	require.NoError(t, os.WriteFile(config, []byte(`{"auths":{"private.example.io":{"username":"fixture","password":"fixture"}}}`), 0o600))

	creds, err := toolchainRegistryCredentials(config)
	require.NoError(t, err)
	assert.Equal(t, map[string]oci.Basic{"private.example.io": {Username: "fixture", Password: "fixture"}}, creds)

	for name, path := range map[string]string{"no flag": "", "no Secret mounted": filepath.Join(dir, "absent")} {
		creds, err := toolchainRegistryCredentials(path)
		require.NoError(t, err, name)
		assert.Nil(t, creds, "%s: every registry is read anonymously", name)
	}

	src, err := toolchainSource("gsoci.azurecr.io/giantswarm/agent-toolchains", config)
	require.NoError(t, err)
	assert.Equal(t, oci.Reference{Host: "gsoci.azurecr.io", Repository: "giantswarm/agent-toolchains"}, src.Repository)

	_, err = toolchainSource("", config)
	require.ErrorContains(t, err, "--toolchain-registry-config needs --toolchain-repository")

	require.NoError(t, os.WriteFile(config, []byte(`auths`), 0o600))
	_, err = toolchainSource("gsoci.azurecr.io/giantswarm/agent-toolchains", config)
	require.ErrorContains(t, err, "--toolchain-registry-config")
}

func TestValidateGitHubTokenExchangeOptions(t *testing.T) {
	exchange := func() *serveOptions {
		return &serveOptions{ // #nosec G101 -- configuration fixture with a path, no secret value
			oauthEnabled:             true,
			downstreamOAuth:          true,
			oauthProvider:            server.ProviderDex,
			dexIssuerURL:             "https://identity.example.com",
			githubExchangeEndpoint:   "https://broker.example.com/oauth/token",
			githubExchangeAudience:   "manager-app",
			githubExchangeSecretFile: "/etc/agent-manager/github-token-exchange/client-secret",
		}
	}

	options := exchange()
	options.githubExchangeClient = "manager-client"
	require.NoError(t, validateCredentialOptions(options), "the client ID flag")

	options = exchange()
	options.githubExchangeClientFile = "/etc/agent-manager/github-token-exchange/client-id"
	require.NoError(t, validateCredentialOptions(options), "the client ID file")

	require.Error(t, validateCredentialOptions(exchange()), "neither client ID nor its file")

	options = exchange()
	options.githubExchangeClient = "manager-client"
	options.githubAuthorizationServer = "https://github.com/apps/giantswarm-agent-manager"
	require.Error(t, validateCredentialOptions(options), "the exchange replaces the pin")

	require.ErrorContains(t, validateCredentialOptions(&serveOptions{githubExchangeClientFile: "/etc/agent-manager/github-token-exchange/client-id"}), "--github-token-exchange-endpoint")
}

func TestPlatformCredentialOptionsWithoutTenants(t *testing.T) {
	options := serveOptions{ // #nosec G101 -- URL fixture with no credential values.
		oauthEnabled:                  true,
		downstreamOAuth:               true,
		platformCredentialUIURL:       "https://portal.example.com/customize/models/credentials/test/{namespace}/{operation}",
		credentialOperationsNamespace: "model-operations",
	}
	require.False(t, options.multiTenant)
	require.Empty(t, options.credentialUIURL)
	require.NoError(t, validateCredentialOptions(&options))
	for _, missing := range []string{"OAuth", "downstream", "namespace"} {
		incomplete := options
		switch missing {
		case "OAuth":
			incomplete.oauthEnabled = false
		case "downstream":
			incomplete.downstreamOAuth = false
		case "namespace":
			incomplete.credentialOperationsNamespace = ""
		}
		require.Error(t, validateCredentialOptions(&incomplete), missing)
	}
	options.credentialUIURL = "https://portal.example.com/organizations/{parent}/{organization}/{operation}"
	require.ErrorContains(t, validateCredentialOptions(&options), "multi-tenant")
}
