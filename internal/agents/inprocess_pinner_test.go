package agents

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/skills"
)

// The in-process pinner pins skills and plugins item by item and names the
// item it could not pin, in the list it belongs to.
func TestInProcessPinnerNamesTheRefusedItem(t *testing.T) {
	r := &fakePinner{}
	sk, pl, err := pinSources(t.Context(), InProcessPinner(r),
		Skills{{Git: &GitSkill{URL: skillsRepo, Ref: "feature"}, Path: "runbooks"}},
		Plugins{{Git: &GitSkill{URL: skillsRepo}, Path: "p"}, {OCI: kubectlRef + ":1.4.0"}}, false)
	require.NoError(t, err)
	assert.Equal(t, featureHead, sk[0].Git.Commit)
	assert.Equal(t, mainHead, pl[0].Git.Commit)
	assert.Equal(t, kubectlRef+"@"+kubectlSum, pl[1].OCI)

	_, _, err = pinSources(t.Context(), InProcessPinner(r), nil,
		Plugins{{Git: &GitSkill{URL: skillsRepo}, Path: "p"}, {OCI: "ghcr.io/nobody/plugin:1"}}, false)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "plugins[1]: ")
	assert.NotContains(t, err.Error(), "plugins[1]: plugins[1]", "named once")
}

// Without a resolver nothing is looked up, and no repository is readable:
// readability fails closed.
func TestInProcessPinnerWithoutAResolver(t *testing.T) {
	p := InProcessPinner(nil)
	_, _, err := pinSources(t.Context(), p, Skills{{Git: &GitSkill{URL: skillsRepo, Ref: "main"}}}, nil, false)
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "give git.commit")

	rc, ok := p.(ReadChecker)
	require.True(t, ok)
	require.ErrorIs(t, rc.RequireReadable(t.Context(), skillsRepo, ""), skills.ErrNotReadable)

	formats, ok := p.(PluginFormats)
	require.True(t, ok)
	claude, err := formats.ClaudeFormatPlugin(t.Context(), skillsRepo, mainHead, "claude-format")
	require.NoError(t, err)
	assert.False(t, claude, "a resolver that cannot tell reports none")
}
