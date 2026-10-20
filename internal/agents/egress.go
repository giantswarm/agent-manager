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

// egressOrigin is the CRD's pattern for one spec.egress entry: an http, https
// or tls origin whose host may start with a "*." wildcard that needs two
// labels under it, optionally with a port (the CRD requires it for tls).
var egressOrigin = regexp.MustCompile(`^(?P<scheme>https?|tls)://(?P<host>(\*\.([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+|([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)*)[a-z]([-a-z0-9]{0,61}[a-z0-9])?)(:(?P<port>[1-9][0-9]{0,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5]))?$`)

// defaultPorts are the ports an origin without one reaches.
var defaultPorts = map[string]string{"http": "80", "https": "443"}

// ValidateEgress checks what the Agent CRD and kagent's compiler refuse on
// spec.egress: at most MaxEgress origins, each an HTTP(S) origin or a
// tls://<host>:<port> destination of at most MaxEgressLength characters, no
// entry twice, and no host pattern and port reached over two protocols.
func ValidateEgress(list []string) error {
	if len(list) > MaxEgress {
		return invalidf("egress has %d entries; the Agent accepts at most %d", len(list), MaxEgress)
	}
	protocols := make(map[string]string, len(list))
	for i, origin := range list {
		if len(origin) > MaxEgressLength {
			return invalidf("egress[%d] is %d characters long; an origin has at most %d", i, len(origin), MaxEgressLength)
		}
		m := egressOrigin.FindStringSubmatch(origin)
		if m == nil {
			return invalidf("egress[%d] %q is not an egress origin: give http(s)://<host>[:<port>], such as https://github.com:443 or https://*.githubusercontent.com, or tls://<host>:<port>, such as tls://db.example.com:5432 (a wildcard replaces the leftmost label and needs two labels under it)", i, origin)
		}
		scheme, host, port := m[egressOrigin.SubexpIndex("scheme")], m[egressOrigin.SubexpIndex("host")], m[egressOrigin.SubexpIndex("port")]
		if port == "" && scheme == "tls" {
			return invalidf("egress[%d] %q has no port: a tls:// origin requires one, such as tls://db.example.com:5432", i, origin)
		}
		if slices.Contains(list[:i], origin) {
			return invalidf("egress[%d] %q is listed twice", i, origin)
		}
		if port == "" {
			port = defaultPorts[scheme]
		}
		key := host + ":" + port
		if other, ok := protocols[key]; ok && other != scheme {
			return invalidf("egress[%d] %q reaches %s over %s, which another entry reaches over %s: a host pattern and port take one protocol", i, origin, key, scheme, other)
		}
		protocols[key] = scheme
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
