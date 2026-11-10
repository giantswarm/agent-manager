package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/oci"
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

func TestValidateCredentialOptionsGitHubTokenExchange(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "client-secret")
	exchange := func() *serveOptions {
		return &serveOptions{
			oauthEnabled:             true,
			downstreamOAuth:          true,
			oauthProvider:            "dex",
			dexIssuerURL:             "https://identity.example.com",
			githubExchangeEndpoint:   "https://broker.example.com/oauth/token",
			githubExchangeClient:     "manager-client",
			githubExchangeSecretFile: secretFile,
			githubExchangeAudience:   "manager-app",
		}
	}
	require.NoError(t, validateCredentialOptions(exchange()))

	for name, mutate := range map[string]func(*serveOptions){
		"GitHub-pinned registration": func(o *serveOptions) {
			o.githubAuthorizationServer = "https://github.com/apps/giantswarm-agent-manager"
		},
		"OAuth off":            func(o *serveOptions) { o.oauthEnabled = false },
		"downstream OAuth off": func(o *serveOptions) { o.downstreamOAuth = false },
		"Google provider":      func(o *serveOptions) { o.oauthProvider = "google" },
	} {
		t.Run(name, func(t *testing.T) {
			options := exchange()
			mutate(options)
			require.EqualError(t, validateCredentialOptions(options), "GitHub token exchange requires Dex OAuth and downstream OAuth, and replaces the GitHub-pinned MCP registration")
		})
	}
}
