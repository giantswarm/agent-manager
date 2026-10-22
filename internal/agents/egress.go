package agents

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// egressFromValues reads agent.egress from a release's values.
func egressFromValues(values map[string]any) []string {
	agentBlock, _ := values["agent"].(map[string]any)
	raw, _ := agentBlock["egress"].([]any)
	return stringsOf(raw)
}

// egressFromObject reads spec.egress from an Agent object.
func egressFromObject(obj *unstructured.Unstructured) []string {
	raw, _, _ := unstructured.NestedSlice(obj.Object, "spec", "egress")
	return stringsOf(raw)
}

func stringsOf(raw []any) []string {
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
