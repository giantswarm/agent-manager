package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/kagent/kagenttest"
	"github.com/giantswarm/agent-manager/internal/oci"
)

const toolchainRepo = "gsoci.azurecr.io/giantswarm/agent-toolchains"

// fakeToolchains is a toolchain registry: per toolchain its tags and the
// labels of each tag's image.
type fakeToolchains struct {
	images map[string]map[string]map[string]string // name -> tag -> labels
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

func (f *fakeToolchains) lookup(ref oci.Reference) (string, map[string]map[string]string) {
	name := ref.Repository[len("giantswarm/agent-toolchains/"):]
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
