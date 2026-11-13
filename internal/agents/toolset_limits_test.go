package agents

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/giantswarm/agent-manager/internal/chart"
)

type toolsetChart struct {
	embeddedChart
	maximum float64
}

func (c toolsetChart) Schema(context.Context) chart.Schema {
	schema := chart.EmbeddedSchema()
	document := runtime.DeepCopyJSON(schema.Document.(map[string]any))
	if err := unstructured.SetNestedField(document, c.maximum, "properties", ToolsetValuesKey, "maxItems"); err != nil {
		panic(err)
	}
	schema.Document = document
	return schema
}

func TestToolsetCountComesFromChartSchema(t *testing.T) {
	for _, maximum := range []int{17, 32, 128} {
		t.Run(fmt.Sprint(maximum), func(t *testing.T) {
			source := toolsetChart{maximum: float64(maximum)}
			service := &Service{chart: source}
			require.Equal(t, maximum, service.ToolsetMaxItems(t.Context()))
			selectors := make([]any, maximum+1)
			for index := range selectors {
				selectors[index] = "tool:x"
			}
			_, violations := ValidateValues(t.Context(), source, map[string]any{"toolset": selectors[:maximum]})
			require.Empty(t, violations)
			_, violations = ValidateValues(t.Context(), source, map[string]any{"toolset": selectors})
			require.NotEmpty(t, violations)
			require.Contains(t, strings.Join(violations, "\n"), "maxItems")
		})
	}
}

func TestValidateCreateToolsetLimits(t *testing.T) {
	selectors := func(count int) []string {
		values := make([]string, count)
		for index := range values {
			values[index] = fmt.Sprintf("tool:x_%d", index)
		}
		return values
	}
	for _, test := range []struct {
		name    string
		toolset []string
		invalid string
	}{
		{name: "36 selectors", toolset: selectors(36)},
		{name: "128 selectors", toolset: selectors(128)},
		{name: "129 selectors", toolset: selectors(129), invalid: "maxItems"},
		{name: "8192 bytes", toolset: []string{"tool:" + strings.Repeat("x", 8187)}},
		{name: "8193 bytes", toolset: []string{"tool:" + strings.Repeat("x", 8188)}, invalid: "8193 bytes"},
		{name: "UTF-8 at limit", toolset: []string{"tool:" + strings.Repeat("é", 4093) + "x"}},
		{name: "UTF-8 over limit", toolset: []string{"tool:" + strings.Repeat("é", 4094)}, invalid: "8193 bytes"},
		{name: "commas count", toolset: []string{"tool:" + strings.Repeat("x", 8181), "tool:x"}, invalid: "8193 bytes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := seeded(t)
			result, err := fixture.svc.ValidateCreate(t.Context(), Spec{Name: "fresh", ModelConfig: "default-model-config", Toolset: test.toolset})
			require.NoError(t, err)
			if test.invalid != "" {
				require.False(t, result.Valid)
				require.Contains(t, strings.Join(result.Errors, "\n"), test.invalid)
			} else {
				require.True(t, result.Valid, result.Errors)
			}
		})
	}
}
