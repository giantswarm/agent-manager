package agents

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/toolchainmanager"
)

// fakeCatalog is the toolchain-manager's list: per toolchain its offered
// versions, newest first.
type fakeCatalog struct {
	offers  map[string][]string
	err     error
	bearers []string
}

func (f *fakeCatalog) Offered(_ context.Context, bearer, name string) (toolchainmanager.Offer, error) {
	f.bearers = append(f.bearers, bearer)
	if f.err != nil {
		return toolchainmanager.Offer{}, f.err
	}
	return toolchainmanager.Offer{Versions: f.offers[name]}, nil
}

func TestStartSessionStartsOnlyWhatTheListCarries(t *testing.T) {
	svc, ctrl, reg := newToolchainFixture(t, "")
	catalog := &fakeCatalog{offers: map[string][]string{"go": {"v0.2.0", "v0.1.0"}}}
	svc.cfg.ToolchainCatalog = catalog
	ctx := asPerson(t.Context(), "admin@lab.local")

	started, err := svc.StartSession(ctx, StartSession{Name: "sre", Toolchains: []string{"go"}})
	require.NoError(t, err)
	assert.Equal(t, []string{toolchainRepo + "/go:0.2.0"}, reg.reads, "none named: the newest listed version's tag, not the registry's latest")
	assert.Equal(t, []Toolchain{{Name: "go", Version: "v0.2.0", Image: toolchainImage("go", "0.2.0"), Egress: []string{"https://proxy.golang.org", "https://sum.golang.org"}}}, started.Toolchains)
	assert.Equal(t, []string{callerToken}, catalog.bearers, "the list is asked as the caller")

	for _, named := range []string{"go@0.1.0", "go@v0.1.0"} {
		started, err := svc.StartSession(ctx, StartSession{Name: "sre", Toolchains: []string{named}})
		require.NoError(t, err, named)
		assert.Equal(t, toolchainImage("go", "0.1.0"), started.Toolchains[0].Image, named)
		assert.Equal(t, "v0.1.0", started.Toolchains[0].Version, named)
	}
	assert.Len(t, createRequests(ctrl), 3)
}

func TestADeprecatedVersionStartsNothing(t *testing.T) {
	svc, ctrl, reg := newToolchainFixture(t, "go@0.2.0")
	// 0.2.0 is deprecated: the list carries 0.1.0 alone.
	svc.cfg.ToolchainCatalog = &fakeCatalog{offers: map[string][]string{"go": {"v0.1.0"}, "github": nil}}
	ctx := asPerson(t.Context(), "admin@lab.local")

	for name, test := range map[string]struct {
		named []string
		want  string
	}{
		"named":             {named: []string{"go@0.2.0"}, want: "toolchains: toolchain go: version 0.2.0 is not offered (deprecated, or never published); offered: v0.1.0"},
		"latest":            {named: []string{"go@latest", "github"}, want: "toolchain github is not offered"},
		"never published":   {named: []string{"go@9.9.9"}, want: "version 9.9.9 is not offered"},
		"the agent default": {want: "the agent's defaultToolchains: toolchain go: version 0.2.0 is not offered"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.StartSession(ctx, StartSession{Name: "sre", Toolchains: test.named})
			require.ErrorIs(t, err, ErrInvalid)
			assert.Contains(t, err.Error(), test.want)
		})
	}
	assert.Empty(t, reg.reads, "nothing not offered is resolved")
	assert.Empty(t, createRequests(ctrl), "no session is created")

	started, err := svc.StartSession(ctx, StartSession{Name: "sre", Toolchains: []string{"go"}})
	require.NoError(t, err)
	assert.Equal(t, toolchainImage("go", "0.1.0"), started.Toolchains[0].Image, "none named: the newest offered, the deprecated newest skipped")
}

func TestTheListsAnswersReachTheCaller(t *testing.T) {
	for name, test := range map[string]struct {
		err      error
		sentinel error
		want     string
	}{
		"grant required": {
			err:      &toolchainmanager.GrantRequiredError{Apps: []string{"github"}, Message: "grant the toolchain-manager's GitHub App once in muster (core_auth_login app=github)"},
			sentinel: ErrAuthRequired, want: "core_auth_login app=github",
		},
		"muster sign-in": {
			err:      &toolchainmanager.AuthRequiredError{Server: "toolchain-manager", LoginURL: "https://muster.example/auth/toolchain-manager"},
			sentinel: ErrAuthRequired, want: "sign in to muster server toolchain-manager first",
		},
		"unreachable": {err: errors.New("muster: connection refused"), want: "the toolchain-manager's list could not be read: muster: connection refused"},
	} {
		t.Run(name, func(t *testing.T) {
			svc, ctrl, _ := newToolchainFixture(t, "")
			svc.cfg.ToolchainCatalog = &fakeCatalog{err: test.err}
			_, err := svc.StartSession(asPerson(t.Context(), "admin@lab.local"), StartSession{Name: "sre", Toolchains: []string{"go"}})
			require.Error(t, err)
			if test.sentinel != nil {
				require.ErrorIs(t, err, test.sentinel)
			} else {
				assert.False(t, isDomainError(err), "a failure to ask is no refusal of the caller: %v", err)
			}
			assert.Contains(t, err.Error(), test.want)
			assert.Empty(t, createRequests(ctrl))
		})
	}
}

func TestToolchainUsersNamesTheAgentsAndTheSessions(t *testing.T) {
	svc, _, _ := newToolchainFixture(t, "go@0.1.0,github")
	ctx := asPerson(t.Context(), "admin@lab.local")
	older, err := svc.StartSession(ctx, StartSession{Name: "sre", Toolchains: []string{"go@0.1.0"}})
	require.NoError(t, err)
	newer, err := svc.StartSession(ctx, StartSession{Name: "sre", Toolchains: []string{"go@0.2.0", "github"}})
	require.NoError(t, err)
	sessionID := func(s *SessionStart) string {
		var v struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal(s.Session, &v))
		return v.ID
	}

	version, err := svc.ToolchainUsers(ctx, "go", "v0.1.0")
	require.NoError(t, err)
	assert.Equal(t, []ToolchainAgent{{Namespace: "kagent", Name: "sre", Toolchain: "go@0.1.0"}}, version.Agents)
	require.Len(t, version.Sessions, 1, "only the session holding 0.1.0's digest")
	assert.Equal(t, ToolchainSession{ID: sessionID(older), Namespace: "kagent", Agent: "sre", Image: toolchainImage("go", "0.1.0"), State: version.Sessions[0].State}, version.Sessions[0])
	assert.Empty(t, version.Unreadable)

	whole, err := svc.ToolchainUsers(ctx, "go", "")
	require.NoError(t, err)
	assert.Len(t, whole.Agents, 1)
	assert.ElementsMatch(t, []string{sessionID(older), sessionID(newer)}, []string{whole.Sessions[0].ID, whole.Sessions[1].ID})

	newest, err := svc.ToolchainUsers(ctx, "go", "0.2.0")
	require.NoError(t, err)
	assert.Empty(t, newest.Agents, "a default naming another version does not name it")
	require.Len(t, newest.Sessions, 1)
	assert.Equal(t, sessionID(newer), newest.Sessions[0].ID)

	github, err := svc.ToolchainUsers(ctx, "github", "")
	require.NoError(t, err)
	assert.Equal(t, []ToolchainAgent{{Namespace: "kagent", Name: "sre", Toolchain: "github"}}, github.Agents)
	assert.Len(t, github.Sessions, 1)

	gone, err := svc.ToolchainUsers(ctx, "go", "9.9.9")
	require.NoError(t, err)
	assert.Empty(t, gone.Sessions)
	require.Len(t, gone.Unreadable, 1)
	assert.Contains(t, gone.Unreadable[0], "go:9.9.9 is not published")

	_, err = svc.ToolchainUsers(ctx, "Go", "")
	require.ErrorIs(t, err, ErrInvalid)
	_, err = svc.ToolchainUsers(t.Context(), "go", "")
	require.ErrorIs(t, err, ErrUnauthenticated, "read as the caller only")
}
