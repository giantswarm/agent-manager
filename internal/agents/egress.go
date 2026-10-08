package agents

import (
	"regexp"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The Agent CRD's bounds on spec.egress.
const (
	MaxEgress       = 64
	MaxEgressLength = 270
)

// egressOrigin is the CRD's pattern for one spec.egress entry: an HTTP(S)
// origin whose host may start with a "*." wildcard that needs two labels
// under it, optionally with a port.
var egressOrigin = regexp.MustCompile(`^https?://(\*\.([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+|([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)*)[a-z]([-a-z0-9]{0,61}[a-z0-9])?(:([1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5]))?$`)

// ValidateEgress checks what the Agent CRD refuses on spec.egress: at most
// MaxEgress origins, each an HTTP(S) origin of at most MaxEgressLength
// characters, no entry twice.
func ValidateEgress(list []string) error {
	if len(list) > MaxEgress {
		return invalidf("egress has %d entries; the Agent accepts at most %d", len(list), MaxEgress)
	}
	for i, origin := range list {
		if len(origin) > MaxEgressLength {
			return invalidf("egress[%d] is %d characters long; an origin has at most %d", i, len(origin), MaxEgressLength)
		}
		if !egressOrigin.MatchString(origin) {
			return invalidf("egress[%d] %q is not an HTTP(S) origin: give <scheme>://<host>[:<port>], such as https://github.com:443 or https://*.githubusercontent.com (a wildcard replaces the leftmost label and needs two labels under it)", i, origin)
		}
		if slices.Contains(list[:i], origin) {
			return invalidf("egress[%d] %q is listed twice", i, origin)
		}
	}
	return nil
}

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
