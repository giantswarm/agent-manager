// Package toolchainmanager asks the toolchain-manager, through muster's MCP
// endpoint and as the caller, which versions of a toolchain the installation
// offers: its list_toolchains, which start_session holds a Session's
// toolchains to. A version the list does not carry, deprecated or never
// published, starts nothing.
package toolchainmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/agent-manager/internal/mustermcp"
)

// toolListToolchains is the toolchain-manager's tool, by its own name.
const toolListToolchains = "list_toolchains"

// statusGrantRequired is list_toolchains' answer while the caller has not
// granted the toolchain-manager's GitHub App in muster.
const statusGrantRequired = "grant_required"

// Client calls the toolchain-manager's tools through muster, named by the
// configured prefix (x_toolchain-manager_) joined with the tool's own name.
type Client struct {
	muster *mustermcp.Client
}

// New is the client of the toolchain-manager behind the muster MCP endpoint
// (http(s)://host[:port]/mcp) under the tool prefix. version names
// agent-manager in the MCP handshake.
func New(endpoint, prefix, version string) (*Client, error) {
	c, err := mustermcp.New(endpoint, prefix, version, "toolchain-manager")
	if err != nil {
		return nil, err
	}
	return &Client{muster: c}, nil
}

// The errors of a call through muster.
type (
	// AuthRequiredError is muster's sign-in challenge for the
	// toolchain-manager itself.
	AuthRequiredError = mustermcp.AuthRequiredError
	// ToolError is the toolchain-manager (or muster for it) refusing the call.
	ToolError = mustermcp.ToolError
)

// GrantRequiredError is the toolchain-manager's answer while the caller has
// not granted its GitHub App in muster: it reads the toolchains' sources as
// the person.
type GrantRequiredError struct {
	Apps    []string
	Message string
}

func (e *GrantRequiredError) Error() string { return e.Message }

// Offer is what the installation offers of one toolchain.
type Offer struct {
	// Versions are the offered versions, vX.Y.Z, newest first, of every
	// source that publishes the toolchain; none when it is not offered.
	Versions []string
	// Images are each version's images, <registry>/<name>:X.Y.Z, in the
	// list's order: one per source that publishes the version, so more than
	// one names a version two sources publish. A version the list gives no
	// image for has none.
	Images map[string][]string
}

// Offered asks which versions of toolchain name the caller, whose IdP token
// is bearer, may start a Session with.
func (c *Client) Offered(ctx context.Context, bearer, name string) (Offer, error) {
	tool := c.muster.Tool(toolListToolchains)
	res, err := c.muster.Call(ctx, bearer, tool, map[string]any{"name": name})
	if err != nil {
		return Offer{}, err
	}
	var out struct {
		Status     string   `json:"status"`
		Apps       []string `json:"apps"`
		Message    string   `json:"message"`
		Toolchains []struct {
			Name      string   `json:"name"`
			Versions  []string `json:"versions"`
			Published []struct {
				Version string `json:"version"`
				Image   string `json:"image"`
			} `json:"published"`
		} `json:"toolchains"`
	}
	raw := res.JSON()
	if len(raw) == 0 || json.Unmarshal(raw, &out) != nil {
		return Offer{}, fmt.Errorf("%s: %w: no JSON result", tool, mustermcp.ErrMalformed)
	}
	if out.Status == statusGrantRequired {
		return Offer{}, &GrantRequiredError{Apps: out.Apps, Message: out.Message}
	}
	if out.Status != "" {
		return Offer{}, fmt.Errorf("%s: %w: status %q: %s", tool, mustermcp.ErrMalformed, out.Status, out.Message)
	}
	offer := Offer{Images: map[string][]string{}}
	for _, t := range out.Toolchains {
		if t.Name != name {
			continue
		}
		for _, v := range t.Versions {
			if !slices.Contains(offer.Versions, v) {
				offer.Versions = append(offer.Versions, v)
			}
		}
		for _, p := range t.Published {
			if p.Image != "" && slices.Contains(t.Versions, p.Version) && !slices.Contains(offer.Images[p.Version], p.Image) {
				offer.Images[p.Version] = append(offer.Images[p.Version], p.Image)
			}
		}
	}
	slices.SortFunc(offer.Versions, func(a, b string) int { return compareVersions(b, a) })
	return offer, nil
}

// compareVersions orders two vX.Y.Z versions.
func compareVersions(a, b string) int {
	pa, pb := semver(a), semver(b)
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] - pb[i]
		}
	}
	return strings.Compare(a, b)
}

func semver(v string) [3]int {
	var out [3]int
	for i, p := range strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3) {
		_, _ = fmt.Sscanf(p, "%d", &out[i])
	}
	return out
}
