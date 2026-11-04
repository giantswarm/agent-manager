package agents

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/skills"
)

// A configured skill source with a ref (the platform's skills at its release
// tag) is what a skill from it pins: on create, on update and on
// refreshSkills, not the default branch's head.
func TestSkillsFromASourceWithARefPinItsCommit(t *testing.T) {
	f := seeded(t)
	ctx := t.Context()
	source := skills.Source{URL: skillsRepo, Ref: "v1.2.0", Path: "skills", ReadOnly: true}
	f.svc.skills = skills.New(skills.Config{Repositories: []skills.Source{source, {URL: "https://github.com/giantswarm/other-skills"}}}, nil)

	info := f.svc.Info(ctx)
	assert.Equal(t, []string{skillsRepo, "https://github.com/giantswarm/other-skills"}, info.SkillsRepositories)
	assert.Equal(t, []skills.Source{source, {URL: "https://github.com/giantswarm/other-skills"}}, info.SkillsSources)

	spec := Spec{Name: "platform-helper", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"},
		Skills: Skills{{Git: &GitSkill{URL: skillsRepo}, Path: "skills/overview"}}}
	res, err := f.svc.Create(ctx, spec)
	require.NoError(t, err)
	require.Len(t, res.Agent.Skills, 1)
	assert.Equal(t, tagHead, res.Agent.Skills[0].Git.Commit, "the source's ref, not the default branch")

	// A skill outside the source's path, or with its own ref, is not the source's.
	upd, err := f.svc.Update(ctx, Update{Name: "platform-helper", Skills: &Skills{
		{Git: &GitSkill{URL: skillsRepo}, Path: "skills/overview"},
		{Git: &GitSkill{URL: skillsRepo}, Path: "demo"},
		{Git: &GitSkill{URL: skillsRepo, Ref: "feature"}, Path: "skills/tools"},
	}})
	require.NoError(t, err)
	assert.Equal(t, []any{
		map[string]any{"name": "overview", "path": "skills/overview", "git": map[string]any{"url": skillsRepo, "commit": tagHead}},
		map[string]any{"name": "demo", "path": "demo", "git": map[string]any{"url": skillsRepo, "commit": mainHead}},
		map[string]any{"name": "tools", "path": "skills/tools", "git": map[string]any{"url": skillsRepo, "commit": featureHead}},
	}, upd.After["skills"])

	// refreshSkills follows the source's ref.
	upd, err = f.svc.Update(ctx, Update{Name: "platform-helper", RefreshSkills: true})
	require.NoError(t, err)
	assert.Equal(t, []any{
		map[string]any{"name": "overview", "path": "skills/overview", "git": map[string]any{"url": skillsRepo, "commit": tagHead}},
		map[string]any{"name": "demo", "path": "demo", "git": map[string]any{"url": skillsRepo, "commit": mainHead}},
		map[string]any{"name": "tools", "path": "skills/tools", "git": map[string]any{"url": skillsRepo, "commit": tagHead}},
	}, upd.After["skills"], "every skill below the source's path moves to its ref's commit")
}

// A git plugin of a configured source follows the source's ref as a skill
// does.
func TestPluginsFromASourceWithARefPinItsCommit(t *testing.T) {
	f := seeded(t)
	ctx := t.Context()
	f.svc.skills = skills.New(skills.Config{Repositories: []skills.Source{{URL: skillsRepo, Ref: "v1.2.0", Path: "plugins"}}}, nil)

	res, err := f.svc.Create(ctx, Spec{Name: "plugged", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"},
		Plugins: Plugins{{Git: &GitSkill{URL: skillsRepo}, Path: "plugins/agent"}}})
	require.NoError(t, err)
	require.Len(t, res.Agent.Plugins, 1)
	assert.Equal(t, tagHead, res.Agent.Plugins[0].Git.Commit)

	upd, err := f.svc.Update(ctx, Update{Name: "plugged", Plugins: &Plugins{{Git: &GitSkill{URL: skillsRepo, Ref: "feature"}, Path: "plugins/agent"}}})
	require.NoError(t, err)
	require.NotNil(t, upd.Agent)
	_, err = f.svc.Update(ctx, Update{Name: "plugged", RefreshSkills: true})
	require.NoError(t, err)
	got, err := f.svc.Get(ctx, In(""), "plugged")
	require.NoError(t, err)
	require.Len(t, got.Plugins, 1)
	assert.Equal(t, tagHead, got.Plugins[0].Git.Commit, "refreshSkills follows the source's ref")
}

// One write resolves each repository ref once, however many skills and
// plugins share it.
func TestAWriteResolvesEachRefOnce(t *testing.T) {
	f := seeded(t)
	_, err := f.svc.Create(t.Context(), Spec{Name: "many", ModelConfig: "default-model-config", Toolset: []string{"preset:read-only"},
		Skills:  Skills{{Git: &GitSkill{URL: skillsRepo}, Path: "a"}, {Git: &GitSkill{URL: skillsRepo}, Path: "b"}, {Git: &GitSkill{URL: skillsRepo, Ref: "feature"}, Path: "c"}},
		Plugins: Plugins{{Git: &GitSkill{URL: skillsRepo}, Path: "plugins/agent"}}})
	require.NoError(t, err)
	var git []string
	for _, c := range f.pinner.calls {
		if strings.HasPrefix(c, "git ") {
			git = append(git, c)
		}
	}
	assert.ElementsMatch(t, []string{"git " + skillsRepo + "@", "git " + skillsRepo + "@feature"}, git)
}
