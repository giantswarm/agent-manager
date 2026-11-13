package chart

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/common/util"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/engine"
)

// Platform 4.175.1 forwards devPortal.url to this component. Its removal
// must not make a compatible component upgrade fail Helm schema validation.
func TestManagerChartLegacyDevPortal(t *testing.T) {
	render := func(t *testing.T, values map[string]any) map[string]string {
		t.Helper()
		c, err := loader.Load("../../helm/agent-manager")
		require.NoError(t, err)
		opts := common.ReleaseOptions{Name: "agent-manager", Namespace: "agent-platform", IsInstall: true}
		vals, err := util.ToRenderValuesWithSchemaValidation(c, values, opts, nil, false)
		require.NoError(t, err)
		files, err := engine.Engine{}.RenderWithContext(t.Context(), c, vals)
		require.NoError(t, err)
		return files
	}

	baseline := render(t, nil)
	for _, url := range []string{"", "https://portal.example.com"} {
		t.Run(url, func(t *testing.T) {
			files := render(t, map[string]any{"devPortal": map[string]any{"url": url}})
			assert.Equal(t, baseline, files, "the deprecated URL must not change any rendered resource")
		})
	}

	for name, values := range map[string]map[string]any{
		"unknown root key":   {"unknownCompatibilityKey": true},
		"unknown portal key": {"devPortal": map[string]any{"unknown": true}},
		"invalid URL type":   {"devPortal": map[string]any{"url": true}},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := loader.Load("../../helm/agent-manager")
			require.NoError(t, err)
			_, err = util.ToRenderValuesWithSchemaValidation(c, values, common.ReleaseOptions{}, nil, false)
			require.Error(t, err, "compatibility must not disable strict schema validation")
		})
	}
}
