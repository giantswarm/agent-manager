package oci_test

import (
	"context"
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
