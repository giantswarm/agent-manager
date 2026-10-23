package oci_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/oci"
	"github.com/giantswarm/agent-manager/internal/oci/ocitest"
)

func TestParseChartURL(t *testing.T) {
	ref, err := oci.ParseChartURL("oci://gsoci.azurecr.io/charts/giantswarm/agent")
	require.NoError(t, err)
	assert.Equal(t, "gsoci.azurecr.io", ref.Host)
	assert.Equal(t, "charts/giantswarm/agent", ref.Repository)
	assert.Equal(t, "agent", ref.Name())
	_, err = oci.ParseChartURL("https://gsoci.azurecr.io/charts/giantswarm/agent")
	assert.Error(t, err)
}

func TestParseImageReference(t *testing.T) {
	digest := "sha256:5b0bcabd1ed22e9fb1310cf6c2dec7cdef19f0ad69efa1f392e94a4333501270"
	for ref, want := range map[string]oci.ImageReference{
		"ghcr.io/giantswarm/skills/kubectl:1.4.0":       {Reference: oci.Reference{Host: "ghcr.io", Repository: "giantswarm/skills/kubectl"}, Tag: "1.4.0"},
		"ghcr.io/giantswarm/skills/kubectl":             {Reference: oci.Reference{Host: "ghcr.io", Repository: "giantswarm/skills/kubectl"}},
		"ghcr.io/giantswarm/skills/kubectl@" + digest:   {Reference: oci.Reference{Host: "ghcr.io", Repository: "giantswarm/skills/kubectl"}, Digest: digest},
		"localhost:5000/skills/kubectl:v1@" + digest:    {Reference: oci.Reference{Host: "localhost:5000", Repository: "skills/kubectl"}, Tag: "v1", Digest: digest},
		"registry.example.io/skills/runbooks:1.4.0":     {Reference: oci.Reference{Host: "registry.example.io", Repository: "skills/runbooks"}, Tag: "1.4.0"},
		"localhost/skills/runbooks:1.4.0":               {Reference: oci.Reference{Host: "localhost", Repository: "skills/runbooks"}, Tag: "1.4.0"},
		"gsoci.azurecr.io/giantswarm/skills/a:2026.1.0": {Reference: oci.Reference{Host: "gsoci.azurecr.io", Repository: "giantswarm/skills/a"}, Tag: "2026.1.0"},
	} {
		got, err := oci.ParseImageReference(ref)
		require.NoError(t, err, ref)
		assert.Equal(t, want, got, ref)
	}
	pinned, _ := oci.ParseImageReference("ghcr.io/giantswarm/skills/kubectl:1.4.0@" + digest)
	assert.Equal(t, "ghcr.io/giantswarm/skills/kubectl@"+digest, pinned.Pinned(), "the pinned form drops the tag")

	for _, bad := range []string{"", "kubectl:1.4.0", "skills/kubectl:1.4.0", "ghcr.io/", "ghcr.io/x@sha256:short", "ghcr.io/a b:1", "ghcr.io//x"} {
		_, err := oci.ParseImageReference(bad)
		assert.Error(t, err, bad)
	}
}

func TestRegistryResolvesDigestsAndReadsChartFiles(t *testing.T) {
	f := ocitest.New(t, "charts/giantswarm/agent")
	f.Tags = []string{"0.6.1", "1.0.0"}
	f.Files["1.0.0"] = map[string]string{"Chart.yaml": "name: agent\nversion: 1.0.0\n", "values.schema.json": `{"type":"object"}`}
	reg := oci.NewRegistry(nil)
	ctx := context.Background()

	tags, err := reg.ListTags(ctx, f.Reference())
	require.NoError(t, err)
	assert.Equal(t, []string{"0.6.1", "1.0.0"}, tags)

	digest, err := reg.ManifestDigest(ctx, f.Reference(), "1.0.0")
	require.NoError(t, err)
	assert.Equal(t, ocitest.Digest("1.0.0"), digest, "the HEAD's Docker-Content-Digest is the pin")
	_, err = reg.ManifestDigest(ctx, f.Reference(), "missing")
	assert.Error(t, err)

	raw, err := reg.ReadChartFile(ctx, f.Reference(), "1.0.0", "values.schema.json")
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"object"}`, string(raw))
	assert.Equal(t, 1, f.TokenHits, "the anonymous token is cached across requests")
}

func TestImageLabelsReadsTheConfigOfAnIndexsFirstImage(t *testing.T) {
	const (
		index       = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		attestation = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		amd64       = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
		config      = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
		single      = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
	)
	docs := map[string]any{
		"/v2/tools/go/manifests/" + index: map[string]any{"manifests": []any{
			map[string]any{"digest": attestation, "platform": map[string]any{"os": "unknown"}},
			map[string]any{"digest": amd64, "platform": map[string]any{"os": "linux", "architecture": "amd64"}},
		}},
		"/v2/tools/go/manifests/" + amd64:  map[string]any{"config": map[string]any{"digest": config}},
		"/v2/tools/go/manifests/" + single: map[string]any{"config": map[string]any{"digest": config}},
		"/v2/tools/go/blobs/" + config:     map[string]any{"config": map[string]any{"Labels": map[string]string{"io.giantswarm.toolchain.name": "go"}}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc, ok := docs[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	ref := oci.Reference{Host: strings.TrimPrefix(srv.URL, "http://"), Repository: "tools/go", Insecure: true}
	reg := oci.NewRegistry(nil)

	for _, digest := range []string{index, single} {
		labels, err := reg.ImageLabels(t.Context(), ref, digest)
		require.NoError(t, err, digest)
		assert.Equal(t, map[string]string{"io.giantswarm.toolchain.name": "go"}, labels, digest)
	}
	_, err := reg.ImageLabels(t.Context(), ref, "sha256:6666666666666666666666666666666666666666666666666666666666666666")
	require.ErrorIs(t, err, oci.ErrNotFound)
}

func TestRegistryAsksAPrivateRegistrysTokenWithItsCredentials(t *testing.T) {
	f := ocitest.New(t, "giantswarm/agent-toolchains-lab/lint1")
	f.Tags = []string{"0.1.0"}
	f.Credentials = &oci.Basic{Username: "puller", Password: "secret"}
	ctx := context.Background()

	_, err := oci.NewRegistry(nil).ManifestDigest(ctx, f.Reference(), "0.1.0")
	require.Error(t, err, "anonymously the token endpoint refuses")

	reg := oci.NewRegistry(nil).WithCredentials(map[string]oci.Basic{f.Host(): *f.Credentials})
	digest, err := reg.ManifestDigest(ctx, f.Reference(), "0.1.0")
	require.NoError(t, err)
	assert.Equal(t, ocitest.Digest("0.1.0"), digest)
}

func TestRegistryCachesTokensPerHost(t *testing.T) {
	a := ocitest.New(t, "giantswarm/agent-toolchains/go")
	b := ocitest.New(t, "giantswarm/agent-toolchains/go")
	a.Tags, b.Tags = []string{"0.1.0"}, []string{"0.1.0"}
	reg := oci.NewRegistry(nil)
	ctx := context.Background()
	for _, f := range []*ocitest.Registry{a, b} {
		_, err := reg.ManifestDigest(ctx, f.Reference(), "0.1.0")
		require.NoError(t, err)
	}
	assert.Equal(t, 1, a.TokenHits)
	assert.Equal(t, 1, b.TokenHits, "the same repository on another host has a token of its own")
}

func TestLoadDockerConfig(t *testing.T) {
	path := t.TempDir() + "/config.json"
	require.NoError(t, os.WriteFile(path, []byte(`{"auths":{
		"gsociprivate.azurecr.io":{"auth":"`+base64.StdEncoding.EncodeToString([]byte("puller:pa:ss"))+`"},
		"https://registry.example.io/v1/":{"username":"u","password":"p"}}}`), 0o600))
	creds, err := oci.LoadDockerConfig(path)
	require.NoError(t, err)
	assert.Equal(t, map[string]oci.Basic{
		"gsociprivate.azurecr.io": {Username: "puller", Password: "pa:ss"},
		"registry.example.io":     {Username: "u", Password: "p"},
	}, creds)

	for name, body := range map[string]string{ // #nosec G101 -- malformed Docker config fixtures, no credentials
		"not json":    `auths`,
		"no password": `{"auths":{"r.io":{"username":"u"}}}`,
		"bad auth":    `{"auths":{"r.io":{"auth":"!!"}}}`,
	} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		_, err := oci.LoadDockerConfig(path)
		assert.Error(t, err, name)
	}
}

func TestRegistrySendsCredentialsOnlyToItsOwnRealm(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, ok := r.BasicAuth()
		assert.False(t, ok, "no credentials reach another host")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(elsewhere.Close)
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate", `Bearer realm="`+elsewhere.URL+`/token",service="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(reg.Close)
	ref := oci.Reference{Host: strings.TrimPrefix(reg.URL, "http://"), Repository: "a/b", Insecure: true}
	_, err := oci.NewRegistry(nil).WithCredentials(map[string]oci.Basic{ref.Host: {Username: "u", Password: "p"}}).ManifestDigest(context.Background(), ref, "1")
	require.ErrorContains(t, err, "not on its own host")
}
