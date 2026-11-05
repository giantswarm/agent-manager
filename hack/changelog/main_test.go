package main

import (
	"os"
	"testing"

	"github.com/Masterminds/semver/v3"
)

const header = `# Changelog

All notable changes to this project will be documented in this file.
`

func TestCutMovesReleasedEntriesToTheirFirstTag(t *testing.T) {
	atV1 := header + `
## [Unreleased]

### Added

- one
`
	atV2RC := header + `
## [Unreleased]

### Added

- one
- two

### Fixed

- three
`
	head := atV2RC + `- four
`
	tags := []tag{
		{version: semver.MustParse("1.0.0"), date: "2026-10-01", changelog: atV1},
		{version: semver.MustParse("2.0.0-rc.1"), date: "2026-10-02", changelog: atV2RC},
	}

	cl := parse(head)
	moved := cut(cl, tags)

	if len(moved) != 3 {
		t.Fatalf("moved %d entries, want 3: %v", len(moved), moved)
	}
	want := header + `
## [Unreleased]

### Fixed

- four

## [2.0.0-rc.1] - 2026-10-02

### Added

- two

### Fixed

- three

## [1.0.0] - 2026-10-01

### Added

- one

[Unreleased]: https://github.com/giantswarm/agent-manager/compare/v2.0.0-rc.1...HEAD
[2.0.0-rc.1]: https://github.com/giantswarm/agent-manager/compare/v1.0.0...v2.0.0-rc.1
[1.0.0]: https://github.com/giantswarm/agent-manager/releases/tag/v1.0.0
`
	if got := cl.render(); got != want {
		t.Errorf("render:\n%s\nwant:\n%s", got, want)
	}

	// A second cut is a no-op: what check runs.
	if again := cut(parse(cl.render()), tags); len(again) != 0 {
		t.Errorf("a second cut moved %v", again)
	}
}

func TestCutKeepsAnExistingVersionSection(t *testing.T) {
	released := header + `
## [Unreleased]

### Changed

- new

## [1.0.0] - 2026-10-01

### Added

- old
`
	tags := []tag{{version: semver.MustParse("1.0.0"), date: "2026-10-01", changelog: header + "\n## [Unreleased]\n\n### Changed\n\n- new\n"}}

	cl := parse(released)
	if moved := cut(cl, tags); len(moved) != 1 {
		t.Fatalf("moved %v, want the one entry", moved)
	}
	if len(cl.sections) != 2 || cl.sections[1].name != "1.0.0" || len(cl.sections[1].groups) != 2 {
		t.Fatalf("sections: %+v", cl.sections[1])
	}
	if g := cl.sections[1].groups; g[0].name != "Added" || g[1].name != "Changed" {
		t.Errorf("groups out of Keep a Changelog order: %s, %s", g[0].name, g[1].name)
	}
}

// gitRepo makes a repository in a temporary directory, isolated from the
// user's and the checkout's git configuration, and runs the test inside it.
func gitRepo(t *testing.T) func(args ...string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	t.Chdir(t.TempDir())
	run := func(args ...string) {
		t.Helper()
		if _, err := git(args...); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "--quiet", "--initial-branch=main")
	return run
}

func commit(t *testing.T, run func(...string), file, content string) {
	t.Helper()
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", file)
	run("commit", "--quiet", "-m", file)
}

func TestChangelogTouched(t *testing.T) {
	tests := []struct {
		name   string
		branch func(t *testing.T, run func(...string))
		want   bool
	}{
		{
			name:   "a dependency bump",
			branch: func(t *testing.T, run func(...string)) { commit(t, run, "go.mod", "module x\n\ngo 1.27\n") },
			want:   false,
		},
		{
			name: "a change with an entry",
			branch: func(t *testing.T, run func(...string)) {
				commit(t, run, changelogPath, header+"\n## [Unreleased]\n\n- two\n")
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := gitRepo(t)
			commit(t, run, changelogPath, header)
			run("update-ref", "refs/remotes/origin/main", "HEAD")
			run("checkout", "--quiet", "-b", "branch")
			tt.branch(t, run)

			if got := changelogTouched(); got != tt.want {
				t.Errorf("changelogTouched() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChangelogTouchedWithoutMainChecksInFull(t *testing.T) {
	run := gitRepo(t)
	commit(t, run, changelogPath, header)

	if !changelogTouched() {
		t.Error("changelogTouched() = false without origin/main, want true")
	}
}
