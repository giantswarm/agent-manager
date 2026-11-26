// Package muster calls muster's session bindings as the caller: the binding
// of a workspace Session's sandbox actor to the person who started it, which
// muster checks before it answers the egress gateway a person's provider
// token for that actor. Every call carries the caller's IdP token as
// `Authorization: Bearer` and nothing else that identifies anyone; muster
// files the binding under that token's subject and refuses a binding of
// another person's actor. agent-manager holds no token: the binding names
// the actor, the Session and the provider instances, never a credential.
package muster

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ActorTrustDomain is the SPIFFE trust domain Substrate mints actor
// identities in: spiffe://substrate-actor.local/actor/<atespace>/<name>.
const ActorTrustDomain = "substrate-actor.local"

// ActorID is the SPIFFE id of the Substrate actor name in atespace, the
// identity the egress gateway verifies and muster finds the binding by.
func ActorID(atespace, name string) string {
	return "spiffe://" + ActorTrustDomain + "/actor/" + atespace + "/" + name
}

// Binding is one Session's sandbox actor bound to the caller.
type Binding struct {
	// Actor is the sandbox actor's SPIFFE id (ActorID).
	Actor string `json:"actor"`
	// Session is the Session as muster logs it: <namespace>/<id>, its
	// Agent's namespace and the Session's id.
	Session string `json:"session"`
	// Providers are the provider instances whose grants muster may release
	// to the actor: the first path segment of the Session's
	// ate-secret://muster.giantswarm.io/<provider>/<git|api> references,
	// and the reserved provider muster when kagent admitted the Session to
	// muster's actor bearer.
	Providers []string `json:"providers"`
	// Toolset is the agent's toolset at the Session's start, the selectors
	// muster resolves every request of the actor within when it calls muster
	// with its own JWT (a header narrows it, never widens it). nil binds none:
	// implicit full access.
	Toolset []string `json:"toolset,omitempty"`
	// Agent is the agent the Session runs, <namespace>/<name>: muster names
	// it on the approval page and binds approvals to it. muster takes it
	// only from a sender it reviewed (SenderHeader).
	Agent string `json:"agent,omitempty"`
	// Approval is the agent's approval settings and grant marks; muster
	// takes them only from a sender it reviewed (SenderHeader).
	Approval *Approval `json:"approval,omitempty"`
}

// Approval is an agent's approval settings as a binding carries them.
type Approval struct {
	// Entries are the tool list's entries that carry a setting.
	Entries []ApprovalEntry `json:"entries,omitempty"`
	// Default is the list's default setting, "" for none.
	Default string `json:"default,omitempty"`
	// Grants mark the entries whose Allow holds over a connector's Ask.
	Grants []Grant `json:"grants,omitempty"`
}

// ApprovalEntry is one entry's setting.
type ApprovalEntry struct {
	Entry   string `json:"entry"`
	Setting string `json:"setting"`
}

// Grant marks an entry's Allow over one connector's Ask: the entry (or
// "default"), the connector's muster server key, its MCPServer UID and the
// server's own names of the tools the grant covered when it was given;
// muster lifts only those.
type Grant struct {
	Entry  string   `json:"entry"`
	Server string   `json:"server"`
	UID    string   `json:"uid"`
	Tools  []string `json:"tools"`
}

// SettingsHeader is muster's answer to a bind on what it did with the
// bind's agent and approval settings.
const SettingsHeader = "X-Muster-Approval-Settings"

// What muster answers in SettingsHeader. A muster without per-agent tool
// approval sends no header (BindResult.Settings "").
const (
	// SettingsBound: muster took the agent and its approval settings.
	SettingsBound = "bound"
	// SettingsIgnored: they came without a sender muster accepts and were
	// dropped.
	SettingsIgnored = "ignored"
	// SettingsNone: the bind carried none.
	SettingsNone = "none"
)

// DropResult is muster's answer to a grant drop: how many live bindings the
// agent has and how many marks it removed.
type DropResult struct {
	Bindings int `json:"bindings"`
	Dropped  int `json:"dropped"`
}

// BindResult is muster's answer to a bind.
type BindResult struct {
	// Settings is SettingsHeader's value, "" without one.
	Settings string
}

// SenderHeader carries agent-manager's own projected ServiceAccount token
// (audience muster) beside the person's: muster reviews it (TokenReview,
// SubjectAccessReview for bind on toolapprovals) before it takes approval
// settings from a bind, and requires it to drop grant marks.
const SenderHeader = "X-Muster-Approval-Sender"

// GrantsPath is the path, below the bindings endpoint, that drops grant
// marks from an agent's running Sessions.
const GrantsPath = "/grants"

// Provider is the reserved provider instance a binding lists when kagent
// admitted the Session to muster's actor bearer: the sandbox calls muster
// itself as the person, and the binding's Toolset bounds those calls.
const Provider = "muster"

// Sentinel errors of a refused call; every error names muster.
var (
	// ErrForbidden: the actor is bound to another person.
	ErrForbidden = errors.New("muster refused the binding: the actor is bound to another person")
	// ErrInvalid: muster does not know a provider, or the binding is malformed.
	ErrInvalid = errors.New("muster refused the binding as invalid")
	// ErrUnauthenticated: muster did not accept the caller's token.
	ErrUnauthenticated = errors.New("muster did not accept the caller's token")
	// ErrUnknownField: muster refused a field of the request it does not
	// know: it predates per-agent tool approval (it then also answers
	// ErrInvalid).
	ErrUnknownField = errors.New("muster does not know a field of the request")
	// ErrConflict: muster refused to replace a binding another sender (or
	// agent-manager with its settings) wrote.
	ErrConflict = errors.New("muster refused to replace the binding")
)

const (
	callTimeout   = 10 * time.Second
	maxErrorBytes = 4 << 10
	// maxBindBytes is muster's bind body limit (64 KiB): a toolset of
	// up to 8 KiB, at most 128 approval entries and 256 grant marks.
	maxBindBytes = 64 << 10
)

// Client is muster's session bindings endpoint.
type Client struct {
	endpoint *url.URL
	http     *http.Client
	// senderToken reads agent-manager's projected ServiceAccount token per
	// call (it rotates); nil sends none.
	senderToken func() (string, error)
}

// WithSenderTokenFile sends the token in path (re-read on every call) as
// SenderHeader on binds and grant drops. The token says who agent-manager
// is to muster for about an hour: sending it over plain http is warned
// about, and the client never follows a redirect (New).
func (c *Client) WithSenderTokenFile(path string) *Client {
	if path == "" {
		return c
	}
	if c.endpoint.Scheme != "https" && !ClusterLocal(c.endpoint.Hostname()) {
		slog.Warn("muster session bindings are called over plain http outside the cluster: the approval sender token travels unencrypted; use an https URL", "url", c.endpoint.Redacted())
	}
	c.senderToken = func() (string, error) {
		raw, err := os.ReadFile(path) // #nosec G304 -- the operator's configured projected token path
		if err != nil {
			return "", fmt.Errorf("read the approval sender token: %w", err)
		}
		return strings.TrimSpace(string(raw)), nil
	}
	return c
}

// SendsApproval reports whether binds carry the sender token, so muster
// takes approval settings from them.
func (c *Client) SendsApproval() bool { return c.senderToken != nil }

// New is the client of the session bindings endpoint, its full URL
// (http(s)://host[:port]/session-bindings). A nil httpClient is a default
// one with a per-call timeout.
func New(endpoint string, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("muster session bindings URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("muster session bindings URL %q: want http(s)://host[:port]/path without a query", endpoint)
	}
	hc := &http.Client{Timeout: callTimeout}
	if httpClient != nil {
		copied := *httpClient
		hc = &copied
	}
	// A redirect would carry the person's token and the sender token to
	// wherever it points: none is followed.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{endpoint: u, http: hc}, nil
}

// ClusterLocal reports whether host names an in-cluster Service: a single
// label or <service>.<namespace> (resolved through the cluster's search
// path), or a name ending in .svc or .svc.cluster.local. Plain http to it
// stays on the cluster network.
func ClusterLocal(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || net.ParseIP(host) != nil {
		return false
	}
	if strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local") {
		return true
	}
	return strings.Count(host, ".") <= 1
}

// WithCAFile trusts the PEM certificates in path (with the system pool) for
// an https endpoint, e.g. muster's own CA. An empty path changes nothing.
func (c *Client) WithCAFile(path string) (*Client, error) {
	if path == "" {
		return c, nil
	}
	pem, err := os.ReadFile(path) // #nosec G304 -- the operator's configured CA path
	if err != nil {
		return nil, fmt.Errorf("muster session bindings CA: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("muster session bindings CA %s holds no PEM certificate", path)
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("muster session bindings CA: unexpected default transport")
	}
	t := transport.Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	c.http.Transport = t
	return c, nil
}

// approvalBytes is the size of the bind's agent and approval part.
func approvalBytes(b Binding) int {
	if b.Approval == nil && b.Agent == "" {
		return 0
	}
	raw, _ := json.Marshal(struct {
		Agent    string    `json:"agent,omitempty"`
		Approval *Approval `json:"approval,omitempty"`
	}{b.Agent, b.Approval})
	return len(raw)
}

// Bind binds b.Actor to the caller whose IdP token is token (PUT, 204).
// Binding the same actor again for the same person is accepted. The result
// says what muster did with the agent and its approval settings.
func (c *Client) Bind(ctx context.Context, token string, b Binding) (BindResult, error) {
	body, err := json.Marshal(b)
	if err != nil {
		return BindResult{}, fmt.Errorf("muster: encode the binding: %w", err)
	}
	if len(body) > maxBindBytes {
		return BindResult{}, fmt.Errorf("%w: session binding encodes to %d bytes, more than the %d allowed (%d of them the agent's tool approval settings and grants); reduce the toolset, use a preset, or set fewer entries and grants", ErrInvalid, len(body), maxBindBytes, approvalBytes(b))
	}
	header, err := c.call(ctx, http.MethodPut, c.endpoint.String(), token, body, false)
	if err != nil {
		return BindResult{}, err
	}
	return BindResult{Settings: header.Get(SettingsHeader)}, nil
}

// DropGrants removes, from every live binding of agent (<namespace>/<name>),
// each grant mark not in keep (PATCH <endpoint>/grants), changing nothing
// else. It needs the sender token: muster refuses it without. token is the
// person's bearer where there is one; "" sends the sender token alone.
func (c *Client) DropGrants(ctx context.Context, token, agent string, keep []Grant) (DropResult, error) {
	if keep == nil {
		keep = []Grant{}
	}
	body, err := json.Marshal(map[string]any{"agent": agent, "keep": keep})
	if err != nil {
		return DropResult{}, fmt.Errorf("muster: encode the grants: %w", err)
	}
	u := *c.endpoint
	u.Path = strings.TrimSuffix(u.Path, "/") + GrantsPath
	var out DropResult
	_, answer, err := c.do(ctx, http.MethodPatch, u.String(), token, body, true)
	if err != nil {
		return DropResult{}, err
	}
	// An answer without the counts (an older muster) reads as zeroes.
	_ = json.Unmarshal(answer, &out)
	return out, nil
}

// Unbind removes the caller's binding of actor (DELETE, 204); an actor
// without a binding is no error.
func (c *Client) Unbind(ctx context.Context, token, actor string) error {
	u := *c.endpoint
	u.RawQuery = url.Values{"actor": {actor}}.Encode()
	_, err := c.call(ctx, http.MethodDelete, u.String(), token, nil, false)
	return err
}

// call sends one request and returns muster's response headers on success.
func (c *Client) call(ctx context.Context, method, target, token string, body []byte, needSender bool) (http.Header, error) {
	header, _, err := c.do(ctx, method, target, token, body, needSender)
	return header, err
}

// do sends one request and returns muster's response headers and body (at
// most maxErrorBytes) on success.
func (c *Client) do(ctx context.Context, method, target, token string, body []byte, needSender bool) (http.Header, []byte, error) {
	// A grant drop may go with the sender token alone (muster takes it
	// without a person's bearer: a startup sweep has none); every other
	// call is the person's.
	if token == "" && (!needSender || c.senderToken == nil) {
		return nil, nil, fmt.Errorf("%w: the request carries no identity token", ErrUnauthenticated)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("muster: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// The sender token goes on every call: muster refuses to delete a
	// binding agent-manager wrote with approval settings without it.
	if c.senderToken != nil {
		sender, err := c.senderToken()
		if err != nil {
			return nil, nil, fmt.Errorf("muster: %w", err)
		}
		req.Header.Set(SenderHeader, sender)
	} else if needSender {
		return nil, nil, fmt.Errorf("%w: agent-manager has no approval sender token (--muster-approval-sender-token-file), so muster takes no grant changes from it", ErrInvalid)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The URL names no secret; the error may carry it and nothing else.
		return nil, nil, fmt.Errorf("muster session bindings (%s): %w", c.endpoint.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		answer, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
		return resp.Header, answer, nil
	}
	detail := readDetail(resp.Body)
	var sentinel error
	switch resp.StatusCode {
	case http.StatusForbidden:
		sentinel = ErrForbidden
	case http.StatusConflict:
		sentinel = ErrConflict
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		sentinel = ErrInvalid
		if strings.Contains(detail, "unknown field") {
			return nil, nil, fmt.Errorf("%w: %w (%s)%s", ErrInvalid, ErrUnknownField, resp.Status, detail)
		}
	case http.StatusUnauthorized:
		sentinel = ErrUnauthenticated
	default:
		return nil, nil, fmt.Errorf("muster session bindings answered %s%s", resp.Status, detail)
	}
	return nil, nil, fmt.Errorf("%w (%s)%s", sentinel, resp.Status, detail)
}

// readDetail is muster's own words on a refusal, bounded, as ": <text>".
func readDetail(body io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(body, maxErrorBytes))
	text := strings.TrimSpace(string(raw))
	var doc struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &doc) == nil {
		text = strings.TrimSpace(strings.Join([]string{doc.Error, doc.Message}, " "))
	}
	if text == "" {
		return ""
	}
	return ": " + text
}
