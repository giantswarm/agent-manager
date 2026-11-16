package api

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/giantswarm/gitops-commit/commit"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/organizations"
)

// planRacingRemote is the fake forge where another writer commits files to
// the base branch once, when the plan (gitops-commit's layout) first reads
// the repository: after every existence check of the write, before its
// change is decided.
type planRacingRemote struct {
	*commit.Fake
	files map[string][]byte
	done  bool
}

func (r *planRacingRemote) ReadFile(ctx context.Context, repo commit.Repository, ref, path string) ([]byte, error) {
	if !r.done && r.files != nil && calledFromLayout() {
		r.done = true
		if err := r.Commit(ctx, repo, "main", "another writer adds the object", r.files); err != nil {
			return nil, err
		}
	}
	return r.Fake.ReadFile(ctx, repo, ref, path)
}

// calledFromLayout reports whether gitops-commit's layout package is on the
// stack: the read is the plan's, not an existence check of agent-manager.
func calledFromLayout() bool {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		if strings.HasPrefix(frame.Function, "github.com/giantswarm/gitops-commit/layout.") {
			return true
		}
		if !more {
			return false
		}
	}
}

// planRaceSubject is a commit-mode create: its tool, its arguments, and its
// pull request's branch.
type planRaceSubject struct {
	name, tool, branch string
	arguments          map[string]any
	seed               func(t *testing.T, fixture *orgFixture)
	prefix             string
}

func planRaceSubjects() []planRaceSubject {
	var subjects []planRaceSubject
	for _, s := range commitRaceSubjects() {
		for _, create := range []bool{true, false} {
			arguments := map[string]any{"mode": "commit", "create": create}
			for key, value := range s.arguments {
				arguments[key] = value
			}
			subject := planRaceSubject{name: s.tool + "/create", tool: s.tool, branch: s.branch, arguments: arguments, prefix: "already_exists:"}
			if !create {
				subject.name, subject.prefix = s.tool+"/no create", "conflict:"
			}
			subject.seed = func(t *testing.T, fixture *orgFixture) { seedSecret(t, fixture, s.secret, s.key) }
			subjects = append(subjects, subject)
		}
	}
	return append(subjects, planRaceSubject{
		name: ToolCreateOrganization, tool: ToolCreateOrganization, branch: "agent-manager/create-org-acme-payments",
		arguments: map[string]any{"namespace": "org-acme", "name": "payments", "adminGroups": []any{"payments-leads"}, "mode": "commit"},
		seed:      func(*testing.T, *orgFixture) {},
		prefix:    "conflict:",
	})
}

// changed is what branch changes against main: every file it adds or
// changes, and nil for every file it removes.
func changed(forge *commit.Fake, branch string) map[string][]byte {
	base, head := forge.Files(mcRepo, "main"), forge.Files(mcRepo, branch)
	out := map[string][]byte{}
	for p, content := range head {
		if old, ok := base[p]; !ok || !bytes.Equal(old, content) {
			out[p] = content
		}
	}
	for p := range base {
		if _, ok := head[p]; !ok {
			out[p] = nil
		}
	}
	return out
}

// A commit-mode create whose files, identical to its own, and their
// kustomization entries another writer adds to git after the existence
// checks and before the plan reads the repository is refused, not reported
// unchanged: with create already_exists, without it conflict, an
// organization conflict. Nothing is written: no branch, no pull request,
// and git is as the other writer left it. A retry of a create that finds
// its own open pull request keeps its conflict.
func TestCommitModeCreateRefusesIdenticalFilesAddedBeforeThePlan(t *testing.T) {
	for _, subject := range planRaceSubjects() {
		t.Run(subject.name, func(t *testing.T) {
			// The files the write commits: those of its own pull request.
			clean, cleanForge := commitFixture(t, nil)
			subject.seed(t, clean)
			text, failed := clean.call(t, subject.tool, subject.arguments)
			require.False(t, failed, text)
			require.Len(t, cleanForge.PullRequests(), 1)
			files := changed(cleanForge, subject.branch)
			require.NotEmpty(t, files)

			// A retry finds its own open pull request and is refused for it.
			text, failed = clean.call(t, subject.tool, subject.arguments)
			require.True(t, failed, text)
			require.True(t, strings.HasPrefix(text, "conflict:"), text)
			require.Contains(t, text, "is open: merge or close it first")
			require.Len(t, cleanForge.PullRequests(), 1)

			remote := &planRacingRemote{files: files}
			fixture, forge := commitFixtureWith(t, nil, func(fake *commit.Fake) organizations.GitHubRemote {
				remote.Fake = fake
				return remote
			})
			subject.seed(t, fixture)
			text, failed = fixture.call(t, subject.tool, subject.arguments)
			require.True(t, remote.done, "the other writer added the files before the plan")
			require.True(t, failed, text)
			require.True(t, strings.HasPrefix(text, subject.prefix), text)
			require.Empty(t, forge.PullRequests())
			require.Empty(t, forge.Files(mcRepo, subject.branch), "no branch is cut")
			main := forge.Files(mcRepo, "main")
			for p, content := range files {
				require.Equal(t, content, main[p], p)
			}
			commits := forge.Commits(mcRepo, "main")
			require.Len(t, commits, 1, "only the other writer committed")
			require.Equal(t, "another writer adds the object", commits[0].Message)
		})
	}
}
