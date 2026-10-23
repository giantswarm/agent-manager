// Package oci is the minimal OCI distribution client agent-manager needs:
// pulls with the registry's bearer-token challenge (RFC 6750 style
// Www-Authenticate), anonymous unless the registry's host has credentials
// (WithCredentials), the tag list of a repository, one file out of a Helm
// chart archive, and the digest a tag resolves to — the way Flux's
// source-controller reads the agent chart, and the way a tagged skill image is
// pinned to the digest kagent requires.
package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// HelmChartLayerMediaType is the media type of a chart archive layer.
	HelmChartLayerMediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

	ociManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	// manifestAccept lists every manifest media type a HEAD may resolve: a
	// skill image can be a single-platform manifest or an index.
	manifestAccept = ociManifestMediaType + ", application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json"

	// maxChartBytes bounds the archive read: the agent chart is a few KiB.
	maxChartBytes = 8 << 20
)

// ErrNotFound is a registry's 404: the repository, tag or blob is not there.
var ErrNotFound = errors.New("not found")

// digestPattern is a sha256 digest as the registry reports it.
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Reference is a parsed registry repository.
type Reference struct {
	// Host is the registry host, e.g. gsoci.azurecr.io.
	Host string
	// Repository is the path within the registry, e.g. charts/giantswarm/agent.
	Repository string
	// Insecure selects plain HTTP (tests).
	Insecure bool
}

// ParseChartURL parses oci://<host>/<repository>, the form a Flux
// OCIRepository carries.
func ParseChartURL(ociURL string) (Reference, error) {
	u, err := url.Parse(ociURL)
	if err != nil {
		return Reference{}, fmt.Errorf("parse chart URL %q: %w", ociURL, err)
	}
	if u.Scheme != "oci" || u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return Reference{}, fmt.Errorf("chart URL %q must look like oci://<registry>/<repository>", ociURL)
	}
	return Reference{Host: u.Host, Repository: strings.Trim(u.Path, "/")}, nil
}

// ImageReference is a parsed image reference: <host>/<repository>[:<tag>][@<digest>].
type ImageReference struct {
	Reference
	Tag    string
	Digest string
}

// Pinned renders the reference by digest, the immutable form.
func (r ImageReference) Pinned() string {
	return r.Host + "/" + r.Repository + "@" + r.Digest
}

// ParseImageReference parses <host>/<repository>[:<tag>][@sha256:<digest>].
// The registry host is required (no Docker Hub defaulting): a skill reference
// names where it comes from.
func ParseImageReference(ref string) (ImageReference, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.ContainsAny(ref, " \t\n") {
		return ImageReference{}, fmt.Errorf("OCI reference %q is empty or contains whitespace", ref)
	}
	out := ImageReference{}
	rest := ref
	if at := strings.Index(rest, "@"); at >= 0 {
		out.Digest = rest[at+1:]
		rest = rest[:at]
		if !digestPattern.MatchString(out.Digest) {
			return ImageReference{}, fmt.Errorf("OCI reference %q: digest must be sha256:<64 hex digits>", ref)
		}
	}
	slash := strings.Index(rest, "/")
	if slash <= 0 {
		return ImageReference{}, fmt.Errorf("OCI reference %q must be <registry>/<repository>[:<tag>][@sha256:<digest>]", ref)
	}
	out.Host, rest = rest[:slash], rest[slash+1:]
	if !strings.ContainsAny(out.Host, ".:") && out.Host != "localhost" {
		return ImageReference{}, fmt.Errorf("OCI reference %q must start with the registry host (e.g. registry.example.io/skills/runbooks:1.4.0)", ref)
	}
	if colon := strings.LastIndex(rest, ":"); colon >= 0 {
		out.Tag, rest = rest[colon+1:], rest[:colon]
	}
	if rest == "" || strings.HasPrefix(rest, "/") || strings.HasSuffix(rest, "/") {
		return ImageReference{}, fmt.Errorf("OCI reference %q has no repository", ref)
	}
	out.Repository = rest
	return out, nil
}

// Name is the last path segment of the repository (the chart name).
func (r Reference) Name() string {
	parts := strings.Split(r.Repository, "/")
	return parts[len(parts)-1]
}

func (r Reference) baseURL() string {
	scheme := "https"
	if r.Insecure {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}

// Registry talks to OCI registries. It is safe for concurrent use.
type Registry struct {
	http *http.Client
	// creds are the Basic credentials the token endpoint of a host is asked
	// with; a host without any is asked anonymously.
	creds map[string]Basic
	mu    sync.Mutex
	// tokens caches the bearer per host and scope until it expires.
	tokens map[string]token
}

// Basic is a registry's username and password.
type Basic struct {
	Username string
	Password string
}

type token struct {
	value   string
	expires time.Time
}

// NewRegistry builds a client with sensible timeouts.
func NewRegistry(client *http.Client) *Registry {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &Registry{http: client, tokens: map[string]token{}}
}

// WithCredentials has the registry ask the token endpoint of each host in
// creds with its credentials: a private registry's pulls.
func (c *Registry) WithCredentials(creds map[string]Basic) *Registry {
	c.creds = creds
	return c
}

// ListTags returns the repository's tags.
func (c *Registry) ListTags(ctx context.Context, ref Reference) ([]string, error) {
	var tags []string
	next := fmt.Sprintf("%s/v2/%s/tags/list?n=1000", ref.baseURL(), ref.Repository)
	for next != "" {
		resp, err := c.do(ctx, ref, http.MethodGet, next, "")
		if err != nil {
			return nil, err
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
		link := resp.Header.Get("Link")
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode tags of %s: %w", ref.Repository, err)
		}
		tags = append(tags, body.Tags...)
		next = nextLink(ref, link)
	}
	return tags, nil
}

// nextLink resolves an RFC 5988 pagination link relative to the registry.
func nextLink(ref Reference, header string) string {
	if header == "" || !strings.Contains(header, `rel="next"`) {
		return ""
	}
	start := strings.Index(header, "<")
	end := strings.Index(header, ">")
	if start < 0 || end <= start {
		return ""
	}
	target := header[start+1 : end]
	if strings.HasPrefix(target, "/") {
		return ref.baseURL() + target
	}
	return target
}

// ManifestDigest resolves a tag to the digest of its manifest: the registry's
// Docker-Content-Digest on a HEAD, else the sha256 of the manifest body.
func (c *Registry) ManifestDigest(ctx context.Context, ref Reference, tag string) (string, error) {
	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", ref.baseURL(), ref.Repository, tag)
	resp, err := c.do(ctx, ref, http.MethodHead, manifestURL, manifestAccept)
	if err != nil {
		return "", err
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	_ = resp.Body.Close()
	if digestPattern.MatchString(digest) {
		return digest, nil
	}
	resp, err = c.do(ctx, ref, http.MethodGet, manifestURL, manifestAccept)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read manifest %s:%s: %w", ref.Repository, tag, err)
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ImageLabels reads the labels of the image at digest from its config: for an
// index, the config of its first image manifest (attestations, platform
// unknown, skipped), since a multi-arch image carries one set of labels per
// platform, built from one Dockerfile.
func (c *Registry) ImageLabels(ctx context.Context, ref Reference, digest string) (map[string]string, error) {
	manifest, err := c.imageManifest(ctx, ref, digest)
	if err != nil {
		return nil, err
	}
	if len(manifest.Manifests) > 0 {
		child := ""
		for _, m := range manifest.Manifests {
			if m.Platform == nil || m.Platform.OS != "unknown" {
				child = m.Digest
				break
			}
		}
		if child == "" {
			return nil, fmt.Errorf("index %s@%s lists no image manifest", ref.Repository, digest)
		}
		if manifest, err = c.imageManifest(ctx, ref, child); err != nil {
			return nil, err
		}
		digest = child
	}
	if !digestPattern.MatchString(manifest.Config.Digest) {
		return nil, fmt.Errorf("manifest %s@%s names no image config", ref.Repository, digest)
	}
	var config struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}
	if err := c.getJSON(ctx, ref, fmt.Sprintf("%s/v2/%s/blobs/%s", ref.baseURL(), ref.Repository, manifest.Config.Digest), "", &config); err != nil {
		return nil, err
	}
	return config.Config.Labels, nil
}

// imageManifest is an image manifest or an index, as far as ImageLabels
// reads it.
type imageManifest struct {
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform *struct {
			OS string `json:"os"`
		} `json:"platform"`
	} `json:"manifests"`
}

func (c *Registry) imageManifest(ctx context.Context, ref Reference, digest string) (*imageManifest, error) {
	var m imageManifest
	if err := c.getJSON(ctx, ref, fmt.Sprintf("%s/v2/%s/manifests/%s", ref.baseURL(), ref.Repository, digest), manifestAccept, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// getJSON decodes the JSON document at target, at most 1 MiB.
func (c *Registry) getJSON(ctx context.Context, ref Reference, target, accept string, into any) error {
	resp, err := c.do(ctx, ref, http.MethodGet, target, accept)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into); err != nil {
		return fmt.Errorf("decode %s: %w", target, err)
	}
	return nil
}

// ReadChartFile pulls the chart at tag and returns the named file from the
// archive (relative to the chart root, e.g. values.schema.json).
func (c *Registry) ReadChartFile(ctx context.Context, ref Reference, tag, file string) ([]byte, error) {
	archive, err := c.ReadChart(ctx, ref, tag)
	if err != nil {
		return nil, err
	}
	return extractFile(bytes.NewReader(archive), ref.Name(), file)
}

// ReadChart pulls the chart at tag and returns its gzipped tar archive.
func (c *Registry) ReadChart(ctx context.Context, ref Reference, tag string) ([]byte, error) {
	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", ref.baseURL(), ref.Repository, tag)
	resp, err := c.do(ctx, ref, http.MethodGet, manifestURL, ociManifestMediaType)
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
		} `json:"layers"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&manifest)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("decode manifest %s:%s: %w", ref.Repository, tag, err)
	}
	var digest string
	for _, l := range manifest.Layers {
		if l.MediaType == HelmChartLayerMediaType {
			digest = l.Digest
			break
		}
	}
	if digest == "" {
		return nil, fmt.Errorf("%s:%s has no Helm chart layer (%s)", ref.Repository, tag, HelmChartLayerMediaType)
	}
	blobURL := fmt.Sprintf("%s/v2/%s/blobs/%s", ref.baseURL(), ref.Repository, digest)
	blob, err := c.do(ctx, ref, http.MethodGet, blobURL, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = blob.Body.Close() }()
	archive, err := io.ReadAll(io.LimitReader(blob.Body, maxChartBytes))
	if err != nil {
		return nil, fmt.Errorf("read chart %s:%s: %w", ref.Repository, tag, err)
	}
	return archive, nil
}

// extractFile finds <chartName>/<file> in a gzipped tar chart archive.
func extractFile(r io.Reader, chartName, file string) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("chart archive is not gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	want := chartName + "/" + file
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s not found in chart archive", want)
		}
		if err != nil {
			return nil, fmt.Errorf("read chart archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || strings.TrimPrefix(hdr.Name, "./") != want {
			continue
		}
		return io.ReadAll(io.LimitReader(tr, maxChartBytes))
	}
}

// do performs one request, answering a bearer challenge with an anonymous
// token from the registry's token endpoint (cached per scope).
func (c *Registry) do(ctx context.Context, ref Reference, method, target, accept string) (*http.Response, error) {
	scope := "repository:" + ref.Repository + ":pull"
	resp, err := c.request(ctx, method, target, accept, c.cachedToken(ref.Host, scope))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("Www-Authenticate")
		_ = resp.Body.Close()
		tok, err := c.fetchToken(ctx, ref.Host, challenge, scope)
		if err != nil {
			return nil, err
		}
		resp, err = c.request(ctx, method, target, accept, tok)
		if err != nil {
			return nil, err
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		err := fmt.Errorf("%s %s: HTTP %d: %s", method, target, resp.StatusCode, strings.TrimSpace(string(body)))
		if resp.StatusCode == http.StatusNotFound {
			err = fmt.Errorf("%w: %w", ErrNotFound, err)
		}
		return nil, err
	}
	return resp, nil
}

func (c *Registry) request(ctx context.Context, method, target, accept, bearer string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, target, err)
	}
	return resp, nil
}

func (c *Registry) cachedToken(host, scope string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.tokens[host+" "+scope]; ok && time.Now().Before(t.expires) {
		return t.value
	}
	return ""
}

// fetchToken follows a `Bearer realm="…",service="…",scope="…"` challenge
// with host's credentials, else anonymously. The challenge's own scope is used when present (a registry may
// ask for a narrower scope than pull, e.g. metadata_read for the tag list).
func (c *Registry) fetchToken(ctx context.Context, host, challenge, fallbackScope string) (string, error) {
	params := parseChallenge(challenge)
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("registry answered 401 without a bearer realm (%q)", challenge)
	}
	q := url.Values{}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = fallbackScope
	}
	q.Set("scope", scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	if b, ok := c.creds[host]; ok {
		req.SetBasicAuth(b.Username, b.Password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("token endpoint %s: %w", realm, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint %s: HTTP %d", realm, resp.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	tok := body.AccessToken
	if tok == "" {
		tok = body.Token
	}
	if tok == "" {
		return "", fmt.Errorf("token endpoint %s returned no token", realm)
	}
	ttl := time.Duration(body.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	c.mu.Lock()
	c.tokens[host+" "+fallbackScope] = token{value: tok, expires: time.Now().Add(ttl - 10*time.Second)}
	c.mu.Unlock()
	return tok, nil
}

// parseChallenge splits `Bearer k="v",k2="v2"` into its parameters.
func parseChallenge(header string) map[string]string {
	out := map[string]string{}
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(header), "Bearer"))
	for _, part := range strings.Split(rest, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		out[strings.TrimSpace(kv[0])] = strings.Trim(strings.TrimSpace(kv[1]), `"`)
	}
	return out
}
