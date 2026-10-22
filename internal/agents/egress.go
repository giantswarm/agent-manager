package agents

import (
	"context"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// egressPolicyRefsFromValues reads agent.egressPolicyRefs from a release's
// values.
func egressPolicyRefsFromValues(values map[string]any) []string {
	agentBlock, _ := values["agent"].(map[string]any)
	raw, _ := agentBlock["egressPolicyRefs"].([]any)
	return stringsOf(raw)
}

// egressPolicyRefsFromObject reads the names of spec.egressPolicyRefs from an
// Agent object.
func egressPolicyRefsFromObject(obj *unstructured.Unstructured) []string {
	raw, _, _ := unstructured.NestedSlice(obj.Object, "spec", "egressPolicyRefs")
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if ref, ok := v.(map[string]any); ok {
			if name, ok := ref["name"].(string); ok {
				out = append(out, name)
			}
		}
	}
	return out
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

// ---- egress policies ----------------------------------------------------------

func (s *Service) egressPolicyGVR() schema.GroupVersionResource {
	return s.kagentGVR("egresspolicies")
}

// ListEgressPolicies lists the kagent EgressPolicies of a namespace as the
// caller, on the target cluster when one is named: the names an agent's
// egressPolicyRefs may pick, with each policy's origins.
func (s *Service) ListEgressPolicies(ctx context.Context, loc Location) ([]EgressPolicy, error) {
	st, err := s.site(ctx, loc)
	if err != nil {
		return nil, err
	}
	return s.listEgressPolicies(ctx, st)
}

func (s *Service) listEgressPolicies(ctx context.Context, st *site) ([]EgressPolicy, error) {
	ns := st.ns()
	list, err := st.agent.Resource(s.egressPolicyGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, wrapKube(err, "list egresspolicies in "+ns)
	}
	out := make([]EgressPolicy, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, egressPolicyOf(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// egressPolicyOf reads an EgressPolicy's destinations: each origin with the
// name of the header its credential is sent in. The Secret the credential
// comes from is never read or reported.
func egressPolicyOf(obj *unstructured.Unstructured) EgressPolicy {
	p := EgressPolicy{Name: obj.GetName(), Namespace: obj.GetNamespace(), Destinations: []EgressDestination{}}
	raw, _, _ := unstructured.NestedSlice(obj.Object, "spec", "destinations")
	for _, v := range raw {
		d, ok := v.(map[string]any)
		if !ok {
			continue
		}
		dest := EgressDestination{}
		dest.Origin, _ = d["origin"].(string)
		if auth, ok := d["authentication"].(map[string]any); ok {
			dest.CredentialHeader, _ = auth["header"].(string)
		}
		p.Destinations = append(p.Destinations, dest)
	}
	p.ManagedBy = obj.GetLabels()[ManagedByLabel]
	return p
}

// requireEgressPolicies fails, naming each entry, when a name has no
// EgressPolicy in the site's namespace as the caller reads it. The CRD's own
// limits (count, uniqueness, empty names) are the dry-run's answer.
func (s *Service) requireEgressPolicies(ctx context.Context, st *site, names []string) error {
	if len(names) == 0 {
		return nil
	}
	policies, err := s.listEgressPolicies(ctx, st)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(policies))
	valid := make([]string, 0, len(policies))
	for _, p := range policies {
		known[p.Name] = true
		valid = append(valid, p.Name)
	}
	var missing []string
	for _, name := range names {
		if name != "" && !known[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	ns := st.ns()
	quoted := make([]string, len(missing))
	for i, m := range missing {
		quoted[i] = `"` + m + `"`
	}
	what := "egressPolicyRefs: EgressPolicy " + strings.Join(quoted, ", ")
	if len(valid) == 0 {
		return invalidf("%s does not exist in namespace %s (no EgressPolicies there; a tenant admin writes them; list_egress_policies)", what, ns)
	}
	return invalidf("%s does not exist in namespace %s; valid: %s", what, ns, strings.Join(valid, ", "))
}

// refuseCodingEgressPolicies refuses egressPolicyRefs on an agent of a coding
// Harness: such an agent reaches its Harness's platform-set origins only.
func refuseCodingEgressPolicies(harness string, values map[string]any) error {
	refs := egressPolicyRefsFromValues(values)
	if len(refs) == 0 {
		return nil
	}
	return invalidf("egressPolicyRefs %s: an agent on coding Harness %s reaches its Harness's origins only, which the platform sets; it picks no EgressPolicy", strings.Join(refs, ", "), harness)
}
