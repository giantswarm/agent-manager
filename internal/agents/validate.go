package agents

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/giantswarm/agent-manager/internal/chart"
)

// printer renders the validator's error kinds in English.
var printer = message.NewPrinter(language.English)

// SchemaSource provides the agent chart's values.schema.json.
type SchemaSource interface {
	Schema(ctx context.Context) chart.Schema
}

// ValidateValues checks values against the chart's schema and returns every
// violation as a readable line (path: message). A schema that does not
// compile is reported as one error rather than letting invalid values through.
func ValidateValues(ctx context.Context, src SchemaSource, values map[string]any) (chart.Schema, []string) {
	s := src.Schema(ctx)
	compiled, err := compile(s)
	if err != nil {
		return s, []string{fmt.Sprintf("agent chart schema %s (%s) does not compile: %v", s.Version, s.Source, err)}
	}
	if err := compiled.Validate(values); err != nil {
		return s, flatten(err)
	}
	return s, nil
}

func compile(s chart.Schema) (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	// Helm validates values with a JSON-schema library that ignores format
	// assertions; keep parity so the same values pass here and at install.
	const id = "agent-values.schema.json"
	if err := c.AddResource(id, s.Document); err != nil {
		return nil, err
	}
	return c.Compile(id)
}

// flatten turns the validator's error tree into leaf messages, sorted so
// callers (and tests) see a stable list.
func flatten(err error) []string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []string{err.Error()}
	}
	var out []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := "/" + strings.Join(e.InstanceLocation, "/")
			if loc == "/" {
				loc = "(root)"
			}
			out = append(out, fmt.Sprintf("%s: %s", loc, e.ErrorKind.LocalizedString(printer)))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	sort.Strings(out)
	return out
}
