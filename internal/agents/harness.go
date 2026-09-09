package agents

import (
	"context"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
)

// A Harness is kagent main's runtime: image, worker pool, snapshot policy and
// an admission selector over AgentTemplate labels. The platform renders one
// per runtime (kagent, claude) selecting kagent.dev/harness=<name>, and a
// template nobody admits never compiles. kagent's HarnessService is gRPC-only,
// so the check is done here against the Harness CRs, the same way the
// controller pairs templates and Harnesses (metav1.LabelSelectorAsSelector on
// spec.allowedAgentTemplates.selector).

// harnessAdmission is one Harness and its admission selector, for messages.
type harnessAdmission struct {
	Name     string
	Selector *metav1.LabelSelector
}

// String renders the Harness with what it admits.
func (h harnessAdmission) String() string {
	if h.Selector == nil {
		return h.Name + " (admits nothing: no allowedAgentTemplates)"
	}
	sel, err := metav1.LabelSelectorAsSelector(h.Selector)
	if err != nil {
		return fmt.Sprintf("%s (invalid selector: %v)", h.Name, err)
	}
	return fmt.Sprintf("%s (admits %s)", h.Name, sel.String())
}

// admits reports whether the Harness selects a template with these labels.
func (h harnessAdmission) admits(lbls map[string]string) bool {
	if h.Selector == nil {
		return false
	}
	sel, err := metav1.LabelSelectorAsSelector(h.Selector)
	return err == nil && sel.Matches(labels.Set(lbls))
}

// listHarnesses reads the Harnesses of a namespace.
func (s *Service) listHarnesses(ctx context.Context, dyn dynamic.Interface, ns string) ([]harnessAdmission, error) {
	list, err := dyn.Resource(s.harnessGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, wrapKube(err, "list harnesses in "+ns)
	}
	out := make([]harnessAdmission, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, harnessAdmissionOf(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func harnessAdmissionOf(h *unstructured.Unstructured) harnessAdmission {
	out := harnessAdmission{Name: h.GetName()}
	raw, found, _ := unstructured.NestedMap(h.Object, "spec", "allowedAgentTemplates", "selector")
	if !found {
		return out
	}
	sel := &metav1.LabelSelector{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, sel); err == nil {
		out.Selector = sel
	}
	return out
}

// requireHarnessAdmission fails when no Harness of the namespace admits a
// template with these labels, naming every Harness and what it admits.
func requireHarnessAdmission(harnesses []harnessAdmission, lbls map[string]string, ns string) error {
	var admitting, described []string
	for _, h := range harnesses {
		described = append(described, h.String())
		if h.admits(lbls) {
			admitting = append(admitting, h.Name)
		}
	}
	if len(admitting) > 0 {
		return nil
	}
	if len(harnesses) == 0 {
		return invalidf("no Harness exists in namespace %s: kagent main runs an agent only through a Harness whose allowedAgentTemplates selector admits the template (the connectivity chart renders them from kagent.harnesses[])", ns)
	}
	return invalidf("no Harness in namespace %s admits an AgentTemplate labelled %s; pass harness: <name> for one of: %s", ns, labels.Set(lbls).String(), strings.Join(described, "; "))
}
