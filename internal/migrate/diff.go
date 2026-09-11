package migrate

import (
	"fmt"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// A GitOps-owned release is never written: its rewrite is emitted as a
// unified diff of the manifest for the pull request in the owning repository.
// The diff is taken between the live object and its rewrite, both stripped of
// what the API server and Flux add (status, managedFields, uid, resource
// version, the kustomize provenance labels), so the hunks are the change and
// the context is close to the file in git.

// manifestDiff renders the unified diff of two objects of the same kind and
// name; "" when they are equal.
func manifestDiff(before, after *unstructured.Unstructured) string {
	a := agents.ToYAML(cleanManifest(before))
	b := agents.ToYAML(cleanManifest(after))
	if a == b {
		return ""
	}
	file := fmt.Sprintf("%s-%s-%s.yaml", strings.ToLower(before.GetKind()), before.GetNamespace(), before.GetName())
	out, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(a), B: difflib.SplitLines(b),
		FromFile: "a/" + file, ToFile: "b/" + file, Context: 3,
	})
	if err != nil {
		return fmt.Sprintf("# diff error: %v\n", err)
	}
	return out
}

// Runtime metadata the API server and Flux add; never in a git manifest.
var (
	runtimeMetadata     = []string{"managedFields", "resourceVersion", "uid", "generation", "creationTimestamp", "finalizers", "selfLink"}
	provenanceLabelBase = "kustomize.toolkit.fluxcd.io/"
)

// cleanManifest is a copy of obj as it would sit in git.
func cleanManifest(obj *unstructured.Unstructured) *unstructured.Unstructured {
	out := &unstructured.Unstructured{Object: runtime.DeepCopyJSON(obj.Object)}
	delete(out.Object, "status")
	for _, f := range runtimeMetadata {
		unstructured.RemoveNestedField(out.Object, "metadata", f)
	}
	for _, key := range []string{"labels", "annotations"} {
		m, found, _ := unstructured.NestedMap(out.Object, "metadata", key)
		if !found {
			continue
		}
		for k := range m {
			if strings.HasPrefix(k, provenanceLabelBase) {
				delete(m, k)
			}
		}
		if len(m) == 0 {
			unstructured.RemoveNestedField(out.Object, "metadata", key)
		} else {
			_ = unstructured.SetNestedMap(out.Object, m, "metadata", key)
		}
	}
	return out
}
