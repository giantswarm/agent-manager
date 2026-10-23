package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/kagent/kagenttest"
	"github.com/giantswarm/agent-manager/internal/oci"
	"github.com/giantswarm/agent-manager/internal/toolchainmanager"
)

const toolchainRepo = "gsoci.azurecr.io/giantswarm/agent-toolchains"

// fakeToolchains is a toolchain registry: per toolchain of the main source
// its tags and the labels of each tag's image, and the same per repository,
// <host>/<repository>, of another source.
type fakeToolchains struct {
	images map[string]map[string]map[string]string // name -> tag -> labels
	others map[string]map[string]map[string]string // <host>/<repository> -> tag -> labels
	err    error
	reads  []string
	// salt moves every tag to another digest.
	salt string
}

func toolchainDigest(name, tag string) string {
	sum := sha256.Sum256([]byte(name + ":" + tag))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func toolchainImage(name, tag string) string {
	return toolchainRepo + "/" + name + "@" + toolchainDigest(name, tag)
}

// lookup is the repository's tags, keyed for its digests by the
// toolchain's name in the main source, else by the repository.
func (f *fakeToolchains) lookup(ref oci.Reference) (string, map[string]map[string]string) {
	if tags, ok := f.others[ref.Host+"/"+ref.Repository]; ok {
		return ref.Host + "/" + ref.Repository, tags
	}
	name, _ := strings.CutPrefix(ref.Repository, "giantswarm/agent-toolchains/")
	return name, f.images[name]
}

func (f *fakeToolchains) ManifestDigest(_ context.Context, ref oci.Reference, tag string) (string, error) {
	f.reads = append(f.reads, ref.Host+"/"+ref.Repository+":"+tag)
	if f.err != nil {
		return "", f.err
	}
	name, tags := f.lookup(ref)
	if _, ok := tags[tag]; !ok {
		return "", fmt.Errorf("%w: HEAD manifests/%s: HTTP 404", oci.ErrNotFound, tag)
	}
	return toolchainDigest(name, f.salt+tag), nil
}

func (f *fakeToolchains) ImageLabels(_ context.Context, ref oci.Reference, digest string) (map[string]string, error) {
	name, tags := f.lookup(ref)
	for tag, labels := range tags {
		if toolchainDigest(name, f.salt+tag) == digest {
			return labels, nil
		}
	}
	return nil, fmt.Errorf("%w: blob of %s", oci.ErrNotFound, digest)
}

// platformToolchains is the platform's source: go at 0.1.0 (no origins) and
// 0.2.0 = latest (the Go module proxy), github, and kagent-only.
func platformToolchains() *fakeToolchains {
	labels := func(name, harnesses, egress string) map[string]string {
		l := map[string]string{ToolchainNameLabel: name, ToolchainHarnessesLabel: harnesses}
		if egress != "" {
			l[ToolchainEgressLabel] = egress
		}
		return l
	}
	goLatest := labels("go", "kagent,claude,codex,omp", "https://proxy.golang.org, https://sum.golang.org")
	return &fakeToolchains{images: map[string]map[string]map[string]string{
		"go": {
			"0.1.0":  labels("go", "kagent,claude,codex,omp", ""),
			"0.2.0":  goLatest,
			"latest": goLatest,
		},
		"github":      {"latest": labels("github", "kagent,claude,codex,omp", "")},
		"kagent-only": {"latest": labels("kagent-only", "kagent", "")},
		"impostor":    {"latest": labels("go", "claude", "")},
	}}
}

// newToolchainFixture is agent sre on the claude Harness with the given
// default toolchains, sessions through the fake controller, and the
// platform's toolchains.
func newToolchainFixture(t *testing.T, defaults string) (*Service, *kagenttest.Controller, *fakeToolchains) {
	t.Helper()
	objs := sessionAgent("sre", []any{"preset:read-only"})
	agent := objs[1].(*unstructured.Unstructured)
	_ = unstructured.SetNestedField(agent.Object, "claude", "spec", "harnessRef", "name")
	if defaults != "" {
		agent.SetAnnotations(map[string]string{DefaultToolchainsAnnotation: defaults})
	}
	objs = append(objs, runtime.Object(harness("kagent", "claude", ClaudeRuntime)))
	svc, ctrl, _ := newBoundSessionFixtureWith(t, objs...)
	reg := platformToolchains()
	svc.cfg.Toolchains = &ToolchainSource{Repository: oci.Reference{Host: "gsoci.azurecr.io", Repository: "giantswarm/agent-toolchains"}, Registry: reg}
	return svc, ctrl, reg
}

func TestParseToolchains(t *testing.T) {
	refs, err := parseToolchains("toolchains", []string{"go@0.1.0", " github "})
	require.NoError(t, err)
	assert.Equal(t, []toolchainRef{{name: "go", version: "0.1.0"}, {name: "github"}}, refs)

	for entry, want := range map[string]string{
		"Go":          "want name[@version]",
		"go@":         "no image tag",
		"go@v 1":      "no image tag",
		"-go":         "want name[@version]",
		"":            "want name[@version]",
		"go@1@2":      "no image tag",
		"a/b@latest":  "want name[@version]",
		"go@.0.1.0":   "no image tag",
		"go@latest!":  "no image tag",
		"go@0.1.0-rc": "",
	} {
		_, err := parseToolchains("toolchains", []string{entry})
		if want == "" {
			assert.NoError(t, err, entry)
			continue
		}
		require.ErrorIs(t, err, ErrInvalid, entry)
		assert.Contains(t, err.Error(), want, entry)
	}
	_, err = parseToolchains("toolchains", []string{"go", "go@0.1.0"})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "named twice")
	many := make([]string, MaxToolchains+1)
	for i := range many {
		many[i] = fmt.Sprintf("t%d", i)
	}
	_, err = parseToolchains("toolchains", many)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestStartSessionMountsTheNamedToolchainsAtTheirDigests(t *testing.T) {
	svc, ctrl, reg := newToolchainFixture(t, "go")
	ctx := asPerson(t.Context(), "admin@lab.local")

	started, err := svc.StartSession(ctx, StartSession{Name: "sre", Toolchains: []string{"go@0.1.0", "github"}})
	require.NoError(t, err)
	assert.Equal(t, []string{toolchainRepo + "/go:0.1.0", toolchainRepo + "/github:latest"}, reg.reads, "the named ones, not the defaults; github at latest")
	reqs := createRequests(ctrl)
	require.Len(t, reqs, 1)
	images := reqs[0].GetToolImages()
	require.Len(t, images, 2, "exactly those")
	assert.Equal(t, "go", images[0].GetName())
	assert.Equal(t, toolchainImage("go", "0.1.0"), images[0].GetOci(), "the older version, by digest")
	assert.Empty(t, images[0].GetEgressDestinations(), "0.1.0 declares no origins")
	assert.Equal(t, "github", images[1].GetName())
	assert.Equal(t, toolchainImage("github", "latest"), images[1].GetOci())
	assert.Equal(t, []Toolchain{
		{Name: "go", Version: "0.1.0", Image: toolchainImage("go", "0.1.0")},
		{Name: "github", Version: "latest", Image: toolchainImage("github", "latest")},
	}, started.Toolchains)
}

func TestASessionNamingNoneGetsTheAgentsDefaultsAtLatest(t *testing.T) {
	svc, ctrl, _ := newToolchainFixture(t, "go,github")
	ctx := asPerson(t.Context(), "admin@lab.local")

	started, err := svc.StartSession(ctx, StartSession{Name: "sre"})
	require.NoError(t, err)
	reqs := createRequests(ctrl)
	require.Len(t, reqs, 1)
	want := []*apiv1alpha1.SessionToolImage{
		{Name: "go", Oci: toolchainImage("go", "latest"), EgressDestinations: []string{"https://proxy.golang.org", "https://sum.golang.org"}},
		{Name: "github", Oci: toolchainImage("github", "latest")},
	}
	require.Len(t, reqs[0].GetToolImages(), len(want))
	for i, w := range want {
		got := reqs[0].GetToolImages()[i]
		assert.Equal(t, w.GetName(), got.GetName())
		assert.Equal(t, w.GetOci(), got.GetOci())
		assert.Equal(t, w.GetEgressDestinations(), got.GetEgressDestinations(), "the toolchain's declared origins go with it")
	}
	require.Len(t, started.Toolchains, 2)
	assert.Equal(t, "latest", started.Toolchains[0].Version)

	_, err = svc.StartSession(ctx, StartSession{Name: "sre", NoToolchains: true})
	require.NoError(t, err)
	reqs = createRequests(ctrl)
	require.Len(t, reqs, 2)
	assert.Empty(t, reqs[1].GetToolImages(), "noToolchains drops the defaults")

	_, err = svc.StartSession(ctx, StartSession{Name: "sre", NoToolchains: true, Toolchains: []string{"go"}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "exclude each other")
}

func TestAnAgentWithoutDefaultsStartsWithoutToolchains(t *testing.T) {
	svc, ctrl, reg := newToolchainFixture(t, "")
	svc.cfg.Toolchains = nil
	_, err := svc.StartSession(asPerson(t.Context(), "admin@lab.local"), StartSession{Name: "sre"})
	require.NoError(t, err, "no source is needed for a session without toolchains")
	assert.Empty(t, reg.reads)
	assert.Empty(t, createRequests(ctrl)[0].GetToolImages())
}

func TestAnAgentWithoutDefaultsStartsWithGithub(t *testing.T) {
	// sre carries no default-toolchains annotation: an agent created before
	// defaultToolchains existed, unchanged.
	svc, ctrl, reg := newToolchainFixture(t, "")
	ctx := asPerson(t.Context(), "admin@lab.local")

	started, err := svc.StartSession(ctx, StartSession{Name: "sre"})
	require.NoError(t, err)
	assert.Equal(t, []string{toolchainRepo + "/github:latest"}, reg.reads)
	images := createRequests(ctrl)[0].GetToolImages()
	require.Len(t, images, 1)
	assert.Equal(t, "github", images[0].GetName())
	assert.Equal(t, toolchainImage("github", "latest"), images[0].GetOci())
	assert.Equal(t, []Toolchain{{Name: "github", Version: "latest", Image: toolchainImage("github", "latest")}}, started.Toolchains)
	assert.Empty(t, started.Warning)

	_, err = svc.StartSession(ctx, StartSession{Name: "sre", NoToolchains: true})
	require.NoError(t, err)
	assert.Empty(t, createRequests(ctrl)[1].GetToolImages(), "noToolchains drops the implicit default too")

	_, err = svc.StartSession(ctx, StartSession{Name: "sre", Toolchains: []string{"go"}})
	require.NoError(t, err)
	images = createRequests(ctrl)[2].GetToolImages()
	require.Len(t, images, 1, "named toolchains replace it")
	assert.Equal(t, "go", images[0].GetName())

	reg.images["github"]["0.1.0"] = reg.images["github"]["latest"]
	catalog := &fakeCatalog{offers: map[string][]string{"github": {"v0.1.0"}}}
	svc.cfg.ToolchainCatalog = catalog
	started, err = svc.StartSession(ctx, StartSession{Name: "sre"})
	require.NoError(t, err)
	assert.Equal(t, []string{callerToken}, catalog.bearers, "held to the list as the caller")
	assert.Equal(t, "v0.1.0", started.Toolchains[0].Version, "the newest listed version")
}

func TestAnAgentWithDefaultsKeepsThem(t *testing.T) {
	svc, ctrl, _ := newToolchainFixture(t, "go")
	started, err := svc.StartSession(asPerson(t.Context(), "admin@lab.local"), StartSession{Name: "sre"})
	require.NoError(t, err)
	images := createRequests(ctrl)[0].GetToolImages()
	require.Len(t, images, 1)
	assert.Equal(t, "go", images[0].GetName(), "an explicit default list holds, without github")
	assert.Empty(t, started.Warning)
}

func TestTheImplicitDefaultNeverFailsAStart(t *testing.T) {
	for name, test := range map[string]struct {
		regErr  error
		catalog *fakeCatalog
		misfit  bool
		want    string
	}{
		"the registry down":        {regErr: errors.New("dial tcp: connection refused"), want: "connection refused"},
		"github not offered":       {catalog: &fakeCatalog{offers: map[string][]string{}}, want: "toolchain github is not offered"},
		"no grant of the App":      {catalog: &fakeCatalog{err: &toolchainmanager.GrantRequiredError{Message: "grant the App"}}, want: "grant the App"},
		"a Harness github misfits": {misfit: true, want: "does not fit Harness claude"},
	} {
		t.Run(name, func(t *testing.T) {
			svc, ctrl, reg := newToolchainFixture(t, "")
			reg.err = test.regErr
			if test.catalog != nil {
				svc.cfg.ToolchainCatalog = test.catalog
			}
			if test.misfit {
				reg.images["github"]["latest"][ToolchainHarnessesLabel] = "kagent"
			}
			started, err := svc.StartSession(asPerson(t.Context(), "admin@lab.local"), StartSession{Name: "sre", Message: "hello"})
			require.NoError(t, err, "the session starts as before the implicit default")
			require.Len(t, createRequests(ctrl), 1)
			assert.Empty(t, createRequests(ctrl)[0].GetToolImages())
			assert.Empty(t, started.Toolchains)
			assert.Contains(t, started.Warning, "started without toolchain github")
			assert.Contains(t, started.Warning, test.want)
		})
	}
}

func TestGetAndListReportTheEffectiveDefaultToolchains(t *testing.T) {
	svc, _, _ := newToolchainFixture(t, "")
	ctx := asPerson(t.Context(), "admin@lab.local")
	got, err := svc.Get(ctx, In("kagent"), "sre")
	require.NoError(t, err)
	assert.Equal(t, []string{"github"}, got.DefaultToolchains)
	assert.True(t, got.DefaultToolchainsImplicit)
	list, err := svc.List(ctx, In("kagent"))
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, []string{"github"}, list[0].Summary().DefaultToolchains)
	assert.True(t, list[0].Summary().DefaultToolchainsImplicit)

	svc.cfg.Toolchains = nil
	got, err = svc.Get(ctx, In("kagent"), "sre")
	require.NoError(t, err)
	assert.Empty(t, got.DefaultToolchains, "an installation without toolchains has no default")

	svc.cfg.Toolchains = &ToolchainSource{}
	onWC := Agent{Target: Target{Organization: "acme", Cluster: "wc1"}}
	svc.effectiveDefaultToolchains(&onWC)
	assert.Empty(t, onWC.DefaultToolchains, "Sessions start only on the installation's own cluster")
}

func TestAToolchainThatCannotBeMountedStartsNoSession(t *testing.T) {
	for name, test := range map[string]struct {
		named    []string
		defaults string
		noSource bool
		regErr   error
		err      error
		wants    []string
	}{
		"a misfit": {
			named: []string{"kagent-only"}, err: ErrInvalid,
			wants: []string{"kagent-only@latest does not fit Harness claude", "it serves kagent", "runs claude"},
		},
		"a misfit default": {
			defaults: "github,kagent-only", err: ErrInvalid,
			wants: []string{"does not fit Harness claude"},
		},
		"an unpublished version": {
			named: []string{"go@9.9.9"}, err: ErrInvalid,
			wants: []string{"toolchain go: version 9.9.9 is not published", toolchainRepo + "/go:9.9.9"},
		},
		"an unknown toolchain": {
			named: []string{"rust"}, err: ErrInvalid,
			wants: []string{"toolchain rust: version latest is not published"},
		},
		"an image of another name": {
			named: []string{"impostor"}, err: ErrInvalid,
			wants: []string{"is no toolchain image of that name", `"go"`},
		},
		"an installation without toolchains": {
			named: []string{"go"}, noSource: true, err: ErrUnsupported,
			wants: []string{"--toolchain-repository"},
		},
		"defaults on an installation without toolchains": {
			defaults: "go", noSource: true, err: ErrUnsupported,
		},
		"a malformed default": {
			defaults: "Go", err: ErrInvalid,
			wants: []string{"the agent's defaultToolchains", `"Go"`},
		},
		"the registry down": {
			named: []string{"go"}, regErr: errors.New("dial tcp: connection refused"),
			wants: []string{"toolchain go@latest could not be resolved to a digest", "connection refused"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, ctrl, reg := newToolchainFixture(t, test.defaults)
			reg.err = test.regErr
			if test.noSource {
				svc.cfg.Toolchains = nil
			}
			_, err := svc.StartSession(asPerson(t.Context(), "admin@lab.local"), StartSession{Name: "sre", Toolchains: test.named, Message: "hello"})
			require.Error(t, err)
			if test.err != nil {
				require.ErrorIs(t, err, test.err)
			} else {
				assert.False(t, isDomainError(err), "a registry failure is no refusal: %v", err)
			}
			for _, want := range test.wants {
				assert.Contains(t, err.Error(), want)
			}
			assert.Empty(t, createRequests(ctrl), "refused before the Session is created")
		})
	}
}

func TestAControllerWithoutSessionToolImagesHasTheSessionDeletedAgain(t *testing.T) {
	svc, ctrl, _ := newToolchainFixture(t, "")
	ctrl.WithoutToolImages = true

	_, err := svc.StartSession(asPerson(t.Context(), "admin@lab.local"), StartSession{Name: "sre", Toolchains: []string{"go"}, Message: "hello"})
	require.ErrorIs(t, err, ErrUnsupported)
	assert.Contains(t, err.Error(), "mounts no session tool images")
	require.Len(t, createRequests(ctrl), 1)
	var methods []string
	for _, call := range ctrl.Calls() {
		methods = append(methods, call.Method)
	}
	assert.Equal(t, []string{"CreateSession", "DeleteSession"}, methods, "deleted again, and no first message sent")
}

func TestARetriedStartAnswersTheDigestsTheSessionKeeps(t *testing.T) {
	svc, _, reg := newToolchainFixture(t, "")
	ctx := asPerson(t.Context(), "admin@lab.local")
	first, err := svc.StartSession(ctx, StartSession{Name: "sre", RequestID: "r1", Toolchains: []string{"go"}})
	require.NoError(t, err)

	reg.salt = "moved" // latest moves to a new image between the attempts
	retried, err := svc.StartSession(ctx, StartSession{Name: "sre", RequestID: "r1", Toolchains: []string{"go"}})
	require.NoError(t, err, "the session the first attempt created is answered, never deleted")
	assert.Equal(t, first.Toolchains[0].Image, retried.Toolchains[0].Image)
	assert.Empty(t, retried.Toolchains[0].Version, "the version it was named with is not known for that digest")
}

func TestCreateAndUpdateKeepTheDefaultToolchainsAsAnAnnotation(t *testing.T) {
	f := seeded(t)
	ctx := context.Background()
	reg := platformToolchains()
	f.svc.cfg.Toolchains = &ToolchainSource{Repository: oci.Reference{Host: "gsoci.azurecr.io", Repository: "giantswarm/agent-toolchains"}, Registry: reg}
	readOnly := []string{"preset:read-only"}

	_, err := f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly, DefaultToolchains: []string{"go", "go@0.1.0"}})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly, Annotations: map[string]string{DefaultToolchainsAnnotation: "go"}})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "set by defaultToolchains")
	_, err = f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly, DefaultToolchains: []string{"go@9.9.9"}})
	require.ErrorIs(t, err, ErrInvalid, "an unpublished default is refused at write time")
	assert.Contains(t, err.Error(), "defaultToolchains")
	res, err := f.svc.ValidateCreate(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly, DefaultToolchains: []string{"go@9.9.9"}})
	require.NoError(t, err)
	assert.False(t, res.Valid, "validate_agent answers as create_agent")

	_, err = f.svc.Create(ctx, Spec{Name: "sre", ModelConfig: "default-model-config", Toolset: readOnly, DefaultToolchains: []string{"go", "github@latest"}})
	require.NoError(t, err)
	annotationsOf := func() map[string]string {
		t.Helper()
		hr, err := f.dyn.Resource(hrGVR).Namespace("kagent").Get(ctx, "sre", metav1.GetOptions{})
		require.NoError(t, err)
		annotations, _, _ := unstructured.NestedStringMap(hr.Object, "spec", "values", "annotations")
		return annotations
	}
	assert.Equal(t, "go,github@latest", annotationsOf()[DefaultToolchainsAnnotation], "the chart puts it on the Agent")
	got, err := f.svc.Get(ctx, In(""), "sre")
	require.NoError(t, err)
	assert.Equal(t, []string{"go", "github@latest"}, got.DefaultToolchains)
	assert.False(t, got.DefaultToolchainsImplicit, "the agent's own")

	_, err = f.svc.Update(ctx, Update{Name: "sre", Annotations: &map[string]string{"team": "sre"}})
	require.NoError(t, err)
	assert.Equal(t, "go,github@latest", annotationsOf()[DefaultToolchainsAnnotation], "replacing the annotations keeps them")
	_, err = f.svc.Update(ctx, Update{Name: "sre", DefaultToolchains: &[]string{"rust"}})
	require.ErrorIs(t, err, ErrInvalid, "an unpublished default is refused on update")
	_, err = f.svc.Update(ctx, Update{Name: "sre", DefaultToolchains: &[]string{"go@0.1.0"}})
	require.NoError(t, err)
	assert.Equal(t, "go@0.1.0", annotationsOf()[DefaultToolchainsAnnotation])
	_, err = f.svc.Update(ctx, Update{Name: "sre", DefaultToolchains: &[]string{}})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"team": "sre"}, annotationsOf(), "[] removes them")
	got, err = f.svc.Get(ctx, In(""), "sre")
	require.NoError(t, err)
	assert.Equal(t, []string{"github"}, got.DefaultToolchains, "and github is the default again")
	assert.True(t, got.DefaultToolchainsImplicit)
}

func TestTheAgentObjectReportsItsDefaultToolchains(t *testing.T) {
	obj := agentObject("kagent", "bare", "", "", true, true)
	obj.SetAnnotations(map[string]string{DefaultToolchainsAnnotation: "go,node@1.0.0"})
	assert.Equal(t, []string{"go", "node@1.0.0"}, agentFromObject(obj, nil).DefaultToolchains)
}

func TestDefaultToolchainsOnAWorkloadClusterAreRefused(t *testing.T) {
	err := ValidateDefaultToolchains([]string{"go"}, Target{Organization: "acme", Cluster: "wc1"})
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "workload cluster")
}
