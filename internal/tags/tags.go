// Package tags normalizes the free-form keywords agents and skills carry.
package tags

import (
	"regexp"
	"slices"
	"strings"
)

// maxLen is the longest tag Parse keeps.
const maxLen = 63

var pattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Normalize trims and lowercases every tag, drops empty ones and duplicates,
// and keeps the order of first appearance.
func Normalize(list []string) []string {
	var out []string
	for _, tag := range list {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" || slices.Contains(out, tag) {
			continue
		}
		out = append(out, tag)
	}
	return out
}

// Split splits every value on commas and normalizes the result: how a filter
// is read, whether it came as one comma-joined value or several.
func Split(values []string) []string {
	var all []string
	for _, v := range values {
		all = append(all, strings.Split(v, ",")...)
	}
	return Normalize(all)
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

// HasAny reports whether have holds any of want (both normalized).
func HasAny(have, want []string) bool {
	return slices.ContainsFunc(want, func(tag string) bool { return slices.Contains(have, tag) })
}
