package main

import (
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
