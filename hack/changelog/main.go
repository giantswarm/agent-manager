// Command changelog keeps CHANGELOG.md's Unreleased section to the changes no
// tag carries yet.
//
// Every merge to main is a release: Auto Release tags it from the squash
// subject. An entry a PR adds under Unreleased is therefore released with that
// PR's merge, and the CHANGELOG of the tag still lists it under Unreleased.
// This command finds, for every Unreleased entry, the first tag whose
// CHANGELOG.md already holds it, and moves the entry into that tag's version
// section.
//
//	go run ./hack/changelog cut     # rewrite CHANGELOG.md
//	go run ./hack/changelog check   # fail when Unreleased holds a released entry
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
)

const (
	changelogPath = "CHANGELOG.md"
	repoURL       = "https://github.com/giantswarm/agent-manager"
	unreleased    = "Unreleased"
)

// groupOrder is Keep a Changelog's order of change types within a version.
var groupOrder = []string{"Added", "Changed", "Deprecated", "Removed", "Fixed", "Security"}

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "cut" && os.Args[1] != "check") {
		fmt.Fprintln(os.Stderr, "usage: changelog cut|check")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(mode string) error {
	raw, err := os.ReadFile(changelogPath)
	if err != nil {
		return err
	}
	tags, err := gitTags()
	if err != nil {
		return err
	}
	cl := parse(string(raw))
	moved := cut(cl, tags)

	if mode == "check" {
		if len(moved) == 0 {
			fmt.Println("CHANGELOG.md: Unreleased holds only changes no tag carries.")
			return nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "CHANGELOG.md: %d entries under Unreleased are already released:\n", len(moved))
		for _, m := range moved {
			fmt.Fprintf(&b, "  %s: %s\n", m.tag, truncate(m.entry, 100))
		}
		b.WriteString("Run `make changelog-cut` and commit CHANGELOG.md.")
		return fmt.Errorf("%s", b.String())
	}

	if err := os.WriteFile(changelogPath, []byte(cl.render()), 0o644); err != nil { //nolint:gosec // a tracked text file
		return err
	}
	fmt.Printf("CHANGELOG.md: moved %d entries out of Unreleased.\n", len(moved))
	return nil
}

// tag is one release as cut: tags on the same commit are one release, named by
// the highest version (a promoted release candidate by its final version).
type tag struct {
	version   *semver.Version
	date      string // YYYY-MM-DD of the tagged commit
	changelog string // CHANGELOG.md at the tag
}

func (t tag) String() string { return "v" + t.version.Original() }

// gitTags returns the v-prefixed semver tags reachable from HEAD, oldest
// version first.
func gitTags() ([]tag, error) {
	out, err := git("tag", "--merged", "HEAD", "--list", "v*")
	if err != nil {
		return nil, err
	}
	byCommit := map[string]tag{}
	for _, name := range strings.Fields(out) {
		v, err := semver.StrictNewVersion(strings.TrimPrefix(name, "v"))
		if err != nil {
			continue
		}
		commit, err := git("rev-list", "-n1", name)
		if err != nil {
			return nil, err
		}
		commit = strings.TrimSpace(commit)
		if prev, ok := byCommit[commit]; ok {
			if v.GreaterThan(prev.version) {
				prev.version = v
				byCommit[commit] = prev
			}
			continue
		}
		date, err := git("log", "-1", "--format=%cs", commit)
		if err != nil {
			return nil, err
		}
		// A tag older than the file has no CHANGELOG.md, and holds no entry.
		content, _ := git("show", commit+":"+changelogPath)
		byCommit[commit] = tag{version: v, date: strings.TrimSpace(date), changelog: content}
	}
	tags := make([]tag, 0, len(byCommit))
	for _, t := range byCommit {
		tags = append(tags, t)
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].version.LessThan(tags[j].version) })
	return tags, nil
}

func git(args ...string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("git", args...) //nolint:gosec // git with this command's own arguments and the repository's tag names
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

type changelog struct {
	preamble []string // the lines before the first version section
	sections []*section
}

type section struct {
	name   string // "Unreleased" or a version without its v
	date   string
	groups []*group
}

type group struct {
	name    string // "Added", "Changed", …
	entries []string
}

// parse reads a Keep a Changelog file. An entry is a bullet line with the
// lines continuing it; the trailing link references are rebuilt on render.
func parse(text string) *changelog {
	cl := &changelog{}
	var sec *section
	var grp *group
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			sec = parseHeading(strings.TrimPrefix(line, "## "))
			cl.sections = append(cl.sections, sec)
			grp = nil
		case sec == nil:
			cl.preamble = append(cl.preamble, line)
		case strings.HasPrefix(line, "### "):
			grp = sec.group(strings.TrimSpace(strings.TrimPrefix(line, "### ")))
		case isLinkRef(line), strings.TrimSpace(line) == "":
		case strings.HasPrefix(line, "- ") || grp == nil || len(grp.entries) == 0:
			if grp == nil {
				grp = sec.group("")
			}
			grp.entries = append(grp.entries, line)
		default:
			grp.entries[len(grp.entries)-1] += "\n" + line
		}
	}
	for len(cl.preamble) > 0 && strings.TrimSpace(cl.preamble[len(cl.preamble)-1]) == "" {
		cl.preamble = cl.preamble[:len(cl.preamble)-1]
	}
	return cl
}

// parseHeading reads "[2.5.0] - 2026-10-17" or "[Unreleased]".
func parseHeading(h string) *section {
	name, date, _ := strings.Cut(h, " - ")
	return &section{name: strings.Trim(strings.TrimSpace(name), "[]"), date: strings.TrimSpace(date)}
}

func isLinkRef(line string) bool {
	return strings.HasPrefix(line, "[") && strings.Contains(line, "]: ")
}

func (s *section) group(name string) *group {
	for _, g := range s.groups {
		if g.name == name {
			return g
		}
	}
	g := &group{name: name}
	s.groups = append(s.groups, g)
	sort.SliceStable(s.groups, func(i, j int) bool { return groupRank(s.groups[i].name) < groupRank(s.groups[j].name) })
	return g
}

func groupRank(name string) int {
	for i, n := range groupOrder {
		if n == name {
			return i
		}
	}
	if name == "" {
		return -1
	}
	return len(groupOrder)
}

func (cl *changelog) section(name, date string) *section {
	for _, s := range cl.sections {
		if s.name == name {
			return s
		}
	}
	s := &section{name: name, date: date}
	cl.sections = append(cl.sections, s)
	return s
}

type move struct{ tag, entry string }

// cut moves every Unreleased entry a tag's CHANGELOG.md already holds into the
// section of the first such tag, and orders the sections newest first.
func cut(cl *changelog, tags []tag) []move {
	var moved []move
	unrel := cl.section(unreleased, "")
	for _, g := range unrel.groups {
		kept := g.entries[:0]
		for _, e := range g.entries {
			t, ok := firstHolding(tags, e)
			if !ok {
				kept = append(kept, e)
				continue
			}
			target := cl.section(t.version.Original(), t.date).group(g.name)
			target.entries = append(target.entries, e)
			moved = append(moved, move{tag: t.String(), entry: e})
		}
		g.entries = kept
	}
	groups := unrel.groups[:0]
	for _, g := range unrel.groups {
		if len(g.entries) > 0 {
			groups = append(groups, g)
		}
	}
	unrel.groups = groups

	sort.SliceStable(cl.sections, func(i, j int) bool {
		return sectionLess(cl.sections[j], cl.sections[i])
	})
	return moved
}

func firstHolding(tags []tag, entry string) (tag, bool) {
	for _, t := range tags {
		if strings.Contains(t.changelog, entry) {
			return t, true
		}
	}
	return tag{}, false
}

// sectionLess orders by version; Unreleased is the newest.
func sectionLess(a, b *section) bool {
	if a.name == unreleased || b.name == unreleased {
		return b.name == unreleased && a.name != unreleased
	}
	va, errA := semver.StrictNewVersion(a.name)
	vb, errB := semver.StrictNewVersion(b.name)
	if errA != nil || errB != nil {
		return false
	}
	return va.LessThan(vb)
}

func (cl *changelog) render() string {
	var b strings.Builder
	b.WriteString(strings.Join(cl.preamble, "\n"))
	b.WriteString("\n")
	for _, s := range cl.sections {
		b.WriteString("\n## [" + s.name + "]")
		if s.date != "" {
			b.WriteString(" - " + s.date)
		}
		b.WriteString("\n")
		for _, g := range s.groups {
			if g.name != "" {
				b.WriteString("\n### " + g.name + "\n")
			}
			b.WriteString("\n")
			for _, e := range g.entries {
				b.WriteString(e + "\n")
			}
		}
	}
	b.WriteString("\n")
	for i, s := range cl.sections {
		b.WriteString(cl.link(i, s) + "\n")
	}
	return b.String()
}

// link is the section's link reference: the compare from the next older
// section, Unreleased against main.
func (cl *changelog) link(i int, s *section) string {
	head := "v" + s.name
	if s.name == unreleased {
		head = "HEAD"
	}
	if i+1 < len(cl.sections) {
		return fmt.Sprintf("[%s]: %s/compare/v%s...%s", s.name, repoURL, cl.sections[i+1].name, head)
	}
	if s.name == unreleased {
		return fmt.Sprintf("[%s]: %s/tree/main", s.name, repoURL)
	}
	return fmt.Sprintf("[%s]: %s/releases/tag/%s", s.name, repoURL, head)
}

func truncate(s string, n int) string {
	s = strings.SplitN(s, "\n", 2)[0]
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
