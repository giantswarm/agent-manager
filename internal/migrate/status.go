package migrate

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// TemplateStatus is the wait phase's verdict on a kagent.dev AgentTemplate,
// what Generic chart 1.x renders: Ready when the Harness's entry in
// status.harnesses[] is Ready with the desired revision its latest
// successful one, failed when a condition of that entry is False for another
// reason than the pending golden snapshot, progressing otherwise. The
// service reads api.kagent.dev Agents only, so the 0.x -> 1.x hop reads its
// templates here.
type TemplateStatus struct {
	Client     dynamic.Interface
	APIVersion string
	Harness    string
}

// Status implements StatusReader.
func (s TemplateStatus) Status(ctx context.Context, loc agents.Location, name string) (*agents.Status, error) {
	ns := loc.Namespace
	gvr := schema.GroupVersionResource{Group: "kagent.dev", Version: orDefault(s.APIVersion, "v1alpha3"), Resource: "agenttemplates"}
	harness := orDefault(s.Harness, agents.DefaultHarnessName)
	out := &agents.Status{Name: name, Namespace: ns}
	tpl, err := s.Client.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		out.Verdict, out.Summary = agents.VerdictProgressing, "the AgentTemplate has not been rendered yet"
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	entries, _, _ := unstructured.NestedSlice(tpl.Object, "status", "harnesses")
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		if entry["harness"] != harness {
			continue
		}
		desired, _ := entry["desiredRevision"].(string)
		latest, _ := entry["latestSuccessfulRevision"].(string)
		conds, _ := entry["conditions"].([]any)
		for _, c := range conds {
			cond, _ := c.(map[string]any)
			typ, _ := cond["type"].(string)
			status, _ := cond["status"].(string)
			reason, _ := cond["reason"].(string)
			message, _ := cond["message"].(string)
			switch {
			case typ == "Ready" && status == "True" && desired == latest:
				out.Verdict, out.Summary = agents.VerdictReady, fmt.Sprintf("AgentTemplate is Ready on Harness %s", harness)
				return out, nil
			case status == "False" && reason != "ActorTemplatePending":
				out.Verdict, out.Summary = agents.VerdictFailed, fmt.Sprintf("Harness %s reports %s False (%s): %s", harness, typ, reason, message)
				return out, nil
			}
		}
		out.Verdict, out.Summary = agents.VerdictProgressing, fmt.Sprintf("Harness %s is compiling revision %s", harness, desired)
		return out, nil
	}
	out.Verdict, out.Summary = agents.VerdictProgressing, fmt.Sprintf("Harness %s has not reported on the AgentTemplate yet", harness)
	return out, nil
}
