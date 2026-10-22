package chart

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/common/util"
	"helm.sh/helm/v4/pkg/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Render renders the chart of s as helm-controller installs it: release name
// and namespace as given, values as the HelmRelease carries them. It returns
// the objects in template order, or the chart's own refusal (a template's
// fail) as the error. The values are validated against the schema
// separately (ValidateValues), so Helm's schema check is skipped here.
func (s Schema) Render(ctx context.Context, release, namespace string, values map[string]any) ([]*unstructured.Unstructured, error) {
	if s.chart == nil {
		return nil, fmt.Errorf("agent chart %s (%s) has no archive to render", s.Version, s.Source)
	}
	opts := common.ReleaseOptions{Name: release, Namespace: namespace, IsInstall: true}
	vals, err := util.ToRenderValuesWithSchemaValidation(s.chart, values, opts, nil, true)
	if err != nil {
		return nil, err
	}
	files, err := engine.Engine{}.RenderWithContext(ctx, s.chart, vals)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		if path.Base(name) == "NOTES.txt" || strings.HasPrefix(path.Base(name), "_") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var out []*unstructured.Unstructured
	for _, name := range names {
		decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(files[name]), 4096)
		for {
			obj := &unstructured.Unstructured{}
			if err := decoder.Decode(&obj.Object); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return nil, fmt.Errorf("decode %s: %w", name, err)
			}
			if len(obj.Object) == 0 {
				continue
			}
			if obj.GetNamespace() == "" {
				obj.SetNamespace(namespace)
			}
			out = append(out, obj)
		}
	}
	return out, nil
}
