// Package chart knows the agent chart: where it lives, which version a Flux
// OCIRepository tracking the configured semver range resolves to right now,
// and the values.schema.json of that version — the contract every agent's
// values are validated against before anything is applied. The registry is the
// source of truth; an embedded copy of the schema (the chart version the
// service was built against) is the fallback when the registry cannot be
// reached, and the API says which one is in use.
package chart

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
)

// EmbeddedSchemaVersion is the agent chart version whose values.schema.json is
// compiled into the binary (internal/chart/embedded/).
const EmbeddedSchemaVersion = "0.5.2"

//go:embed embedded/agent-values.schema.json
var embeddedSchema []byte

// Source names where a schema came from.
const (
	SourceRegistry = "registry"
	SourceEmbedded = "embedded"
)

// Info is what GET /info reports about the chart.
type Info struct {
	// OCIURL is the chart's OCI URL as the OCIRepository carries it.
	OCIURL string `json:"ociUrl"`
	// Semver is the range the OCIRepository tracks (x.x.x: every release).
	Semver string `json:"semver"`
	// LatestVersion is the newest published version inside that range, or ""
	// when the registry could not be read.
	LatestVersion string `json:"latestVersion,omitempty"`
	// SchemaVersion is the chart version whose values.schema.json validates
	// agent values right now.
	SchemaVersion string `json:"schemaVersion"`
	// SchemaSource is registry or embedded.
	SchemaSource string `json:"schemaSource"`
	// ResolvedAt is when the registry was last read successfully.
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
	// Error is the last registry error, when the embedded fallback is in use.
	Error string `json:"error,omitempty"`
}

// Schema is a parsed values.schema.json with its provenance.
type Schema struct {
	Document any
	Version  string
	Source   string
}

// Resolver reads the chart from the registry and caches the result.
type Resolver struct {
	ref      Reference
	ociURL   string
	semver   string
	refresh  time.Duration
	registry *Registry
	log      *slog.Logger

	mu       sync.Mutex
	cached   *Schema
	info     Info
	fetched  time.Time
	lastErr  error
	inFlight bool
}

// NewResolver builds a resolver for ociURL tracking semverRange, re-reading the
// registry every refresh.
func NewResolver(ociURL, semverRange string, refresh time.Duration, registry *Registry, log *slog.Logger) (*Resolver, error) {
	ref, err := ParseReference(ociURL)
	if err != nil {
		return nil, err
	}
	if _, err := semver.NewConstraint(semverRange); err != nil {
		return nil, fmt.Errorf("chart semver range %q: %w", semverRange, err)
	}
	if registry == nil {
		registry = NewRegistry(nil)
	}
	if log == nil {
		log = slog.Default()
	}
	if refresh <= 0 {
		refresh = 10 * time.Minute
	}
	return &Resolver{
		ref: ref, ociURL: ociURL, semver: semverRange, refresh: refresh, registry: registry, log: log,
		info: Info{OCIURL: ociURL, Semver: semverRange, SchemaVersion: EmbeddedSchemaVersion, SchemaSource: SourceEmbedded},
	}, nil
}

// OCIURL is the configured chart URL.
func (r *Resolver) OCIURL() string { return r.ociURL }

// SemverRange is the configured range.
func (r *Resolver) SemverRange() string { return r.semver }

// Name is the chart name (the OCIRepository is named after it).
func (r *Resolver) Name() string { return r.ref.Name() }

// Schema returns the current schema: the registry's for the latest version in
// range when it can be read (cached for the refresh period), else the
// embedded copy. It never fails: validation must work offline.
func (r *Resolver) Schema(ctx context.Context) Schema {
	r.mu.Lock()
	if r.cached != nil && time.Since(r.fetched) < r.refresh {
		s := *r.cached
		r.mu.Unlock()
		return s
	}
	r.mu.Unlock()

	if err := r.resolve(ctx); err != nil {
		r.log.Warn("agent chart registry unavailable, validating against the embedded schema", "chart", r.ociURL, "embeddedVersion", EmbeddedSchemaVersion, "error", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cached != nil {
		return *r.cached
	}
	return r.embedded()
}

// Info reports the resolution state without forcing a registry read.
func (r *Resolver) Info(ctx context.Context) Info {
	_ = r.Schema(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.info
}

func (r *Resolver) embedded() Schema { return EmbeddedSchema() }

// EmbeddedSchema is the compiled-in values.schema.json of the agent chart at
// EmbeddedSchemaVersion — the offline fallback, and what tests validate with.
func EmbeddedSchema() Schema {
	var doc any
	if err := json.Unmarshal(embeddedSchema, &doc); err != nil {
		// A build-time constant; an invalid document is a programming error.
		panic(fmt.Sprintf("embedded agent chart schema: %v", err))
	}
	return Schema{Document: doc, Version: EmbeddedSchemaVersion, Source: SourceEmbedded}
}

// resolve reads tags and the schema of the newest in-range version.
func (r *Resolver) resolve(ctx context.Context) error {
	r.mu.Lock()
	if r.inFlight {
		r.mu.Unlock()
		return nil
	}
	r.inFlight = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.inFlight = false
		r.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tags, err := r.registry.ListTags(ctx, r.ref)
	if err != nil {
		return r.fail(err)
	}
	latest, err := Latest(tags, r.semver)
	if err != nil {
		return r.fail(err)
	}

	r.mu.Lock()
	sameVersion := r.cached != nil && r.cached.Version == latest && r.cached.Source == SourceRegistry
	r.mu.Unlock()
	if sameVersion {
		// Nothing new: just refresh the timestamps.
		r.mu.Lock()
		now := time.Now()
		r.fetched = now
		r.info.LatestVersion = latest
		r.info.ResolvedAt = &now
		r.info.Error = ""
		r.mu.Unlock()
		return nil
	}

	raw, err := r.registry.ReadChartFile(ctx, r.ref, latest, "values.schema.json")
	if err != nil {
		return r.fail(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return r.fail(fmt.Errorf("values.schema.json of %s:%s: %w", r.ref.Repository, latest, err))
	}
	now := time.Now()
	r.mu.Lock()
	r.cached = &Schema{Document: doc, Version: latest, Source: SourceRegistry}
	r.fetched = now
	r.lastErr = nil
	r.info = Info{OCIURL: r.ociURL, Semver: r.semver, LatestVersion: latest, SchemaVersion: latest, SchemaSource: SourceRegistry, ResolvedAt: &now}
	r.mu.Unlock()
	r.log.Info("agent chart resolved", "chart", r.ociURL, "version", latest)
	return nil
}

func (r *Resolver) fail(err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastErr = err
	// Back off: do not hammer an unreachable registry on every request.
	r.fetched = time.Now().Add(-r.refresh + time.Minute)
	if r.cached == nil {
		r.info = Info{OCIURL: r.ociURL, Semver: r.semver, SchemaVersion: EmbeddedSchemaVersion, SchemaSource: SourceEmbedded, Error: err.Error()}
	} else {
		r.info.Error = err.Error()
	}
	return err
}

// Latest picks the highest stable version among tags that satisfies the
// constraint, the way Flux's OCIRepository ref.semver does (Masterminds
// semver; pre-releases are excluded unless the range names one).
func Latest(tags []string, constraint string) (string, error) {
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return "", fmt.Errorf("semver range %q: %w", constraint, err)
	}
	var matching []*semver.Version
	for _, t := range tags {
		v, err := semver.StrictNewVersion(t)
		if err != nil {
			continue
		}
		if c.Check(v) {
			matching = append(matching, v)
		}
	}
	if len(matching) == 0 {
		return "", fmt.Errorf("no chart version satisfies %q among %d tags", constraint, len(tags))
	}
	sort.Sort(semver.Collection(matching))
	return matching[len(matching)-1].Original(), nil
}
