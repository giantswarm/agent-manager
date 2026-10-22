package oci_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
