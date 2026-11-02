// Package tags normalizes the free-form keywords agents and skills carry.
package tags

import (
	"regexp"
	"strings"
)

// maxLen is the longest tag Parse keeps.
const maxLen = 63

var pattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Normalize trims and lowercases every tag, drops empty ones and duplicates,
// and keeps the order of first appearance.
func Normalize(list []string) []string {
	var out []string
	seen := make(map[string]struct{}, len(list))
	for _, tag := range list {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" {
			continue
		}
		if _, dup := seen[tag]; dup {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return out
}

// ParseList normalizes list and drops every tag that is not a lowercase
// '-'-separated word of at most 63 characters. A reader never fails on a bad
// tag.
func ParseList(list []string) []string {
	var out []string
	for _, tag := range Normalize(list) {
		if len(tag) <= maxLen && pattern.MatchString(tag) {
			out = append(out, tag)
		}
	}
	return out
}

// Parse is ParseList for a comma-joined tag list.
func Parse(joined string) []string {
	return ParseList(strings.Split(joined, ","))
}

// Filter is the set of tags a list request asks for; an item matches when it
// carries any of them.
type Filter map[string]struct{}

// NewFilter splits every value on commas and normalizes the result, whether
// the filter came as one comma-joined value or several.
func NewFilter(values []string) Filter {
	f := Filter{}
	for _, v := range values {
		for _, tag := range strings.Split(v, ",") {
			if tag = strings.ToLower(strings.TrimSpace(tag)); tag != "" {
				f[tag] = struct{}{}
			}
		}
	}
	return f
}

// Matches reports whether have holds any tag of f; an empty filter matches
// everything.
func (f Filter) Matches(have []string) bool {
	if len(f) == 0 {
		return true
	}
	for _, tag := range have {
		if _, ok := f[tag]; ok {
			return true
		}
	}
	return false
}
