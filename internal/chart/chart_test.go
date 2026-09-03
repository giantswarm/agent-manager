package chart

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRegistry serves one chart repository with a bearer challenge, the tag
// list and one chart archive per tag, like an OCI distribution registry.
type fakeRegistry struct {
	t         *testing.T
	repo      string
	tags      []string
	schemas   map[string]string // tag -> values.schema.json
	tokenHits int
	failTags  bool
}

func (f *fakeRegistry) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenHits++
		assert.Equal(f.t, "repository:"+f.repo+":pull", r.URL.Query().Get("scope"))
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "anon", "expires_in": 300})
	})
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer anon" {
			w.Header().Set("Www-Authenticate", `Bearer realm="http://`+r.Host+`/oauth2/token",service="fake",scope="repository:`+f.repo+`:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("/v2/"+f.repo+"/tags/list", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if f.failTags {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": f.repo, "tags": f.tags})
	})
	mux.HandleFunc("/v2/"+f.repo+"/manifests/", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		tag := strings.TrimPrefix(r.URL.Path, "/v2/"+f.repo+"/manifests/")
		if _, ok := f.schemas[tag]; !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schemaVersion": 2,
			"config":        map[string]any{"mediaType": "application/vnd.cncf.helm.config.v1+json"},
			"layers": []map[string]any{{
				"mediaType": helmChartLayerMediaType,
				"digest":    "sha256:" + tag,
				"size":      1,
			}},
		})
	})
	mux.HandleFunc("/v2/"+f.repo+"/blobs/", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		tag := strings.TrimPrefix(r.URL.Path, "/v2/"+f.repo+"/blobs/sha256:")
		schema, ok := f.schemas[tag]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(chartArchive(f.t, "agent", map[string]string{
			"Chart.yaml":         "name: agent\nversion: " + tag + "\n",
			"values.schema.json": schema,
		}))
	})
	return mux
}

func chartArchive(t *testing.T, name string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for path, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name + "/" + path, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func newFake(t *testing.T) (*fakeRegistry, *httptest.Server, Reference) {
	f := &fakeRegistry{t: t, repo: "charts/giantswarm/agent",
		tags:    []string{"0.1.0", "0.5.2", "0.6.0-rc.1", "0.6.0", "artifacthub.io", "1.0.0"},
		schemas: map[string]string{"0.6.0": `{"type":"object","properties":{"agent":{"type":"object"}}}`, "1.0.0": `{"type":"object"}`}}
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	ref := Reference{Host: strings.TrimPrefix(ts.URL, "http://"), Repository: f.repo, Insecure: true}
	return f, ts, ref
}

func TestLatestFollowsFluxSemverSemantics(t *testing.T) {
	tags := []string{"0.1.0", "0.5.2", "0.6.0-rc.1", "0.6.0", "artifacthub.io", "1.0.0"}
	v, err := Latest(tags, "x.x.x")
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", v, "x.x.x tracks every stable release")
	v, err = Latest(tags, "0.x")
	require.NoError(t, err)
	assert.Equal(t, "0.6.0", v, "pre-releases are skipped")
	_, err = Latest([]string{"artifacthub.io"}, "x.x.x")
	assert.Error(t, err)
}

func TestRegistryListsTagsAndReadsTheSchemaThroughTheBearerChallenge(t *testing.T) {
	f, _, ref := newFake(t)
	reg := NewRegistry(nil)
	tags, err := reg.ListTags(context.Background(), ref)
	require.NoError(t, err)
	assert.Contains(t, tags, "0.6.0")
	raw, err := reg.ReadChartFile(context.Background(), ref, "0.6.0", "values.schema.json")
	require.NoError(t, err)
	assert.JSONEq(t, f.schemas["0.6.0"], string(raw))
	assert.Equal(t, 1, f.tokenHits, "the anonymous token is cached across requests")
}

func TestResolverPrefersTheRegistryAndFallsBackToTheEmbeddedSchema(t *testing.T) {
	f, ts, _ := newFake(t)
	r, err := NewResolver("oci://"+strings.TrimPrefix(ts.URL, "http://")+"/"+f.repo, "0.x", time.Hour, NewRegistry(nil), nil)
	require.NoError(t, err)
	r.ref.Insecure = true

	s := r.Schema(context.Background())
	assert.Equal(t, SourceRegistry, s.Source)
	assert.Equal(t, "0.6.0", s.Version)
	info := r.Info(context.Background())
	assert.Equal(t, "0.6.0", info.LatestVersion)
	assert.Empty(t, info.Error)

	// A broken registry on a fresh resolver: the embedded copy validates.
	f.failTags = true
	r2, err := NewResolver("oci://"+strings.TrimPrefix(ts.URL, "http://")+"/"+f.repo, "0.x", time.Hour, NewRegistry(nil), nil)
	require.NoError(t, err)
	r2.ref.Insecure = true
	s2 := r2.Schema(context.Background())
	assert.Equal(t, SourceEmbedded, s2.Source)
	assert.Equal(t, EmbeddedSchemaVersion, s2.Version)
	info2 := r2.Info(context.Background())
	assert.NotEmpty(t, info2.Error)
	assert.Empty(t, info2.LatestVersion)
	props, ok := s2.Document.(map[string]any)["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, props, "modelConfig", "the embedded copy is the real agent chart schema")
}

func TestParseReference(t *testing.T) {
	ref, err := ParseReference("oci://gsoci.azurecr.io/charts/giantswarm/agent")
	require.NoError(t, err)
	assert.Equal(t, "gsoci.azurecr.io", ref.Host)
	assert.Equal(t, "charts/giantswarm/agent", ref.Repository)
	assert.Equal(t, "agent", ref.Name())
	_, err = ParseReference("https://gsoci.azurecr.io/charts/giantswarm/agent")
	assert.Error(t, err)
}
