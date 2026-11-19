package cmd

import (
	"bytes"
	"log/slog"
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

	for name, mutate := range map[string]func(*serveOptions){
		"OAuth off":            func(options *serveOptions) { options.oauthEnabled = false },
		"downstream OAuth off": func(options *serveOptions) { options.downstreamOAuth = false },
		"Google provider":      func(options *serveOptions) { options.oauthProvider = server.ProviderGoogle },
	} {
		t.Run(name, func(t *testing.T) {
			options := exchange()
			options.githubExchangeClient = "manager-client"
			mutate(options)
			require.EqualError(t, validateCredentialOptions(options), "GitHub token exchange requires Dex OAuth and downstream OAuth")
		})
	}

	require.ErrorContains(t, validateCredentialOptions(&serveOptions{githubExchangeClientFile: "/etc/agent-manager/github-token-exchange/client-id"}), "--github-token-exchange-endpoint")
}

func TestCredentialOptionsWithoutTenants(t *testing.T) {
	options := serveOptions{ // #nosec G101 -- URL fixture with no credential values.
		oauthEnabled:                  true,
		downstreamOAuth:               true,
		credentialUIURL:               "https://portal.example.com/customize/models/credentials/test/{namespace}/{operation}",
		credentialOperationsNamespace: "model-operations",
	}
	require.False(t, options.multiTenant)
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
	require.ErrorContains(t, validateCredentialOptions(&options), "{namespace}")
	options.multiTenant = true
	require.NoError(t, validateCredentialOptions(&options))
}

func TestOrganizationToolsNeedTenancyOrAnInstallationForm(t *testing.T) {
	require.False(t, organizationToolsEnabled(&serveOptions{oauthEnabled: true, downstreamOAuth: true}))
	require.True(t, organizationToolsEnabled(&serveOptions{multiTenant: true, downstreamOAuth: true}))
	withForm := serveOptions{oauthEnabled: true, downstreamOAuth: true, credentialUIURL: "https://portal.example.com/customize/models/credentials/test/{namespace}/{operation}"} // #nosec G101 -- URL fixture with no credential values.
	require.True(t, organizationToolsEnabled(&withForm))
}

func TestCredentialUIURLWithoutTenantsNamesTheNamespace(t *testing.T) {
	options := serveOptions{oauthEnabled: true, downstreamOAuth: true, credentialOperationsNamespace: "model-operations"}
	for template, valid := range map[string]bool{
		"https://portal.example.com/models/{namespace}/{operation}":              true,
		"https://portal.example.com/models?namespace={namespace}&op={operation}": true,
		"https://portal.example.com/models/{operation}":                          false,
		"https://portal.example.com/models/{parent}/{namespace}/{operation}":     false,
		"https://portal.example.com/x/{organization}/{namespace}/{operation}":    false,
		"https://portal.example.com/models{?namespace,operation}":                true,
		"https://portal.example.com/models/{namespace}":                          false,
	} {
		options.credentialUIURL = template
		if valid {
			require.NoError(t, validateCredentialOptions(&options), template)
		} else {
			require.Error(t, validateCredentialOptions(&options), template)
		}
	}
}

func TestWarnDeprecatedOAuthOptions(t *testing.T) {
	tests := []struct {
		name     string
		options  serveOptions
		wantWarn string
	}{
		{name: "allow-private-oauth-urls set", options: serveOptions{dexAllowPrivateIP: true}, wantWarn: "--allow-private-oauth-urls (AGENT_MANAGER_OAUTH_ALLOW_PRIVATE_URLS) has no effect"},
		{name: "sso-allow-private-ips set", options: serveOptions{ssoAllowPrivateIPs: true}, wantWarn: "--sso-allow-private-ips (SSO_ALLOW_PRIVATE_IPS) has no effect"},
		{name: "none set"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			warnDeprecatedOAuthOptions(&tc.options, slog.New(slog.NewTextHandler(&output, nil)))
			if tc.wantWarn == "" {
				assert.Empty(t, output.String())
				return
			}
			assert.Contains(t, output.String(), tc.wantWarn)
		})
	}
}
