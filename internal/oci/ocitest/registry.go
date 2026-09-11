// Package ocitest is a fake OCI distribution registry for tests: one
// repository behind a bearer challenge, a tag list, one manifest per tag with
// a Docker-Content-Digest, and a chart archive per tag as the single blob.
package ocitest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/oci"
)

// Registry is the fake's state; mutate it between requests.
type Registry struct {
	// Repo is the repository path the fake serves.
	Repo string
	// Tags is the tag list.
	Tags []string
	// Files maps tag -> chart files (path -> content) served as the archive.
	Files map[string]map[string]string
	// TokenHits counts token-endpoint calls (bearer caching).
	TokenHits int
	// FailTags makes the tag list answer 500.
	FailTags bool

	t      *testing.T
	server *httptest.Server
}

// New starts a fake registry serving repo. The server stops with the test.
func New(t *testing.T, repo string) *Registry {
	t.Helper()
	f := &Registry{t: t, Repo: repo, Files: map[string]map[string]string{}}
	f.server = httptest.NewServer(f.handler())
	t.Cleanup(f.server.Close)
	return f
}

// Host is the registry's host:port.
func (f *Registry) Host() string { return strings.TrimPrefix(f.server.URL, "http://") }

// Reference is the repository as an insecure oci.Reference.
func (f *Registry) Reference() oci.Reference {
	return oci.Reference{Host: f.Host(), Repository: f.Repo, Insecure: true}
}

// Digest is the digest the fake reports for a tag's manifest.
func Digest(tag string) string {
	sum := sha256.Sum256([]byte("manifest:" + tag))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (f *Registry) auth(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer anon" {
		w.Header().Set("Www-Authenticate", `Bearer realm="http://`+r.Host+`/oauth2/token",service="fake",scope="repository:`+f.Repo+`:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

func (f *Registry) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		f.TokenHits++
		require.Equal(f.t, "repository:"+f.Repo+":pull", r.URL.Query().Get("scope"))
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "anon", "expires_in": 300})
	})
	mux.HandleFunc("/v2/"+f.Repo+"/tags/list", func(w http.ResponseWriter, r *http.Request) {
		if !f.auth(w, r) {
			return
		}
		if f.FailTags {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": f.Repo, "tags": f.Tags})
	})
	mux.HandleFunc("/v2/"+f.Repo+"/manifests/", func(w http.ResponseWriter, r *http.Request) {
		if !f.auth(w, r) {
			return
		}
		tag := strings.TrimPrefix(r.URL.Path, "/v2/"+f.Repo+"/manifests/")
		if !f.hasTag(tag) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Docker-Content-Digest", Digest(tag))
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schemaVersion": 2,
			"config":        map[string]any{"mediaType": "application/vnd.cncf.helm.config.v1+json"},
			"layers": []map[string]any{{
				"mediaType": oci.HelmChartLayerMediaType,
				"digest":    "sha256:" + tag,
				"size":      1,
			}},
		})
	})
	mux.HandleFunc("/v2/"+f.Repo+"/blobs/", func(w http.ResponseWriter, r *http.Request) {
		if !f.auth(w, r) {
			return
		}
		tag := strings.TrimPrefix(r.URL.Path, "/v2/"+f.Repo+"/blobs/sha256:")
		files, ok := f.Files[tag]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(ChartArchive(f.t, f.Reference().Name(), files))
	})
	return mux
}

func (f *Registry) hasTag(tag string) bool {
	if _, ok := f.Files[tag]; ok {
		return true
	}
	for _, t := range f.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// ChartArchive builds a gzipped tar the way helm package does.
func ChartArchive(t *testing.T, name string, files map[string]string) []byte {
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
