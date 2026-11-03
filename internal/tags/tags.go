// Package tags normalizes the free-form keywords agents and skills carry. The
// rule a tag must satisfy is not kept here: it is the agent chart's, loaded
// as a Rule from the chart's tag.schema.json and agent.tags.
package tags

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Rule is the agent chart's rule for one tag (tag.schema.json: pattern and
// maxLength) and for a tag list (agent.tags: maxItems). A nil Rule is a chart
// without tags: it keeps no tag.
type Rule struct {
	pattern   *regexp.Regexp
	maxLength int
	maxItems  int
}

// NewRule reads the chart's tag.schema.json and the maxItems of agent.tags;
// zero maxLength or maxItems means no limit.
func NewRule(tagSchema []byte, maxItems int) (*Rule, error) {
	var doc struct {
		Pattern   string `json:"pattern"`
		MaxLength int    `json:"maxLength"`
	}
	if err := json.Unmarshal(tagSchema, &doc); err != nil {
		return nil, fmt.Errorf("tag schema: %w", err)
	}
	r := &Rule{maxLength: doc.MaxLength, maxItems: maxItems}
	if doc.Pattern != "" {
		re, err := regexp.Compile(doc.Pattern)
		if err != nil {
			return nil, fmt.Errorf("tag schema pattern %q: %w", doc.Pattern, err)
		}
		r.pattern = re
	}
	return r, nil
}

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

// ParseList normalizes list, drops every tag the rule refuses and keeps at
// most the rule's maxItems. A reader never fails on a bad tag; a nil Rule
// keeps none.
func (r *Rule) ParseList(list []string) []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, tag := range Normalize(list) {
		if r.maxItems > 0 && len(out) == r.maxItems {
			break
		}
		if r.allows(tag) {
			out = append(out, tag)
		}
	}
	return out
}

// Parse is ParseList for a comma-joined tag list.
func (r *Rule) Parse(joined string) []string {
	return r.ParseList(strings.Split(joined, ","))
}

func (r *Rule) allows(tag string) bool {
	if r.maxLength > 0 && utf8.RuneCountInString(tag) > r.maxLength {
		return false
	}
	return r.pattern == nil || r.pattern.MatchString(tag)
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
