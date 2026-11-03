// Package tagstest is the agent chart's tag rule for tests, until the
// embedded chart carries one: tag.schema.json is a copy of the agent chart's
// helm/agent/tag.schema.json (2.5.0), MaxItems its agent.tags maxItems.
package tagstest

import (
	_ "embed"

	"github.com/giantswarm/agent-manager/internal/tags"
)

// MaxItems is agent.tags maxItems of the agent chart 2.5.0.
const MaxItems = 50

//go:embed tag.schema.json
var tagSchema []byte

// Rule is the agent chart 2.5.0 tag rule.
func Rule() *tags.Rule {
	r, err := tags.NewRule(tagSchema, MaxItems)
	if err != nil {
		panic(err)
	}
	return r
}
