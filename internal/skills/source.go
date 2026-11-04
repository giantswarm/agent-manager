package skills

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// Source is one configured skill repository: its URL and, optionally, the
// ref discovery reads and agents pin (empty: the default branch's head), the
// directory discovery scans (empty: the whole repository) and readOnly.
// agent-manager only reports readOnly — it has no write path into skill
// repositories; consumers that write skills (the portal) honour it.
type Source struct {
	URL      string `json:"url"`
	Ref      string `json:"ref,omitempty"`
	Path     string `json:"path,omitempty"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// UnmarshalJSON takes a plain URL string or the object form.
func (s *Source) UnmarshalJSON(data []byte) error {
	var url string
	if err := json.Unmarshal(data, &url); err == nil {
		*s = Source{URL: url}
		return nil
	}
	type plain Source
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("a skill repository is a URL or {url, ref, path, readOnly}: %w", err)
	}
	*s = Source(p)
	return nil
}

// ParseSources reads the configured skill repositories: a JSON array whose
// items are a URL or {url, ref, path, readOnly}, or comma-separated URLs.
func ParseSources(value string) ([]Source, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	var out []Source
	if strings.HasPrefix(value, "[") {
		if err := json.Unmarshal([]byte(value), &out); err != nil {
			return nil, fmt.Errorf("skill repositories: %w", err)
		}
	} else {
		for _, url := range strings.Split(value, ",") {
			out = append(out, Source{URL: url})
		}
	}
	kept := out[:0]
	for i, s := range out {
		s.URL = strings.TrimSpace(s.URL)
		s.Ref = strings.TrimSpace(s.Ref)
		raw := strings.TrimSpace(s.Path)
		switch {
		case s.URL == "" && s.Ref == "" && raw == "":
			continue // an empty item of a comma-separated list
		case s.URL == "":
			return nil, fmt.Errorf("skill repository %d has no url", i)
		case strings.ContainsAny(s.Ref, " \t\n"):
			return nil, fmt.Errorf("skill repository %s: ref %q contains whitespace", s.URL, s.Ref)
		case strings.HasPrefix(raw, "/"):
			return nil, fmt.Errorf("skill repository %s: path %q must be relative to the repository root", s.URL, raw)
		case climbs(raw):
			return nil, fmt.Errorf("skill repository %s: path %q must not climb out of the repository", s.URL, raw)
		}
		if s.Path = path.Clean(raw); s.Path == "." {
			s.Path = ""
		}
		for _, prev := range kept {
			if sameRepository(prev.URL, s.URL) && (within(s.Path, prev.Path) || within(prev.Path, s.Path) || s.Path == prev.Path) {
				return nil, fmt.Errorf("skill repository %s is configured twice with overlapping paths (%q and %q): give it once, or paths that do not contain each other", s.URL, prev.Path, s.Path)
			}
		}
		kept = append(kept, s)
	}
	if len(kept) == 0 {
		return nil, nil
	}
	return kept, nil
}

// sameRepository compares two repository URLs by their canonical form.
func sameRepository(a, b string) bool {
	return canonicalOrRaw(a) == canonicalOrRaw(b)
}

func canonicalOrRaw(url string) string {
	if owner, name, err := parseRepoURL(url); err == nil {
		return strings.ToLower(canonicalRepoURL(owner, name))
	}
	return strings.ToLower(strings.TrimRight(url, "/"))
}

func climbs(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}
