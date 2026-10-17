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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	// ate-secret://muster.giantswarm.io/<provider>/<git|api> references.
	Providers []string `json:"providers"`
}

// Sentinel errors of a refused call; every error names muster.
var (
	// ErrForbidden: the actor is bound to another person.
	ErrForbidden = errors.New("muster refused the binding: the actor is bound to another person")
	// ErrInvalid: muster does not know a provider, or the binding is malformed.
	ErrInvalid = errors.New("muster refused the binding as invalid")
	// ErrUnauthenticated: muster did not accept the caller's token.
	ErrUnauthenticated = errors.New("muster did not accept the caller's token")
)

const (
	callTimeout   = 10 * time.Second
	maxErrorBytes = 4 << 10
)

// Client is muster's session bindings endpoint.
type Client struct {
	endpoint *url.URL
	http     *http.Client
}

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
	if httpClient == nil {
		httpClient = &http.Client{Timeout: callTimeout}
	}
	return &Client{endpoint: u, http: httpClient}, nil
}

// Bind binds b.Actor to the caller whose IdP token is token (PUT, 204).
// Binding the same actor again for the same person is accepted.
func (c *Client) Bind(ctx context.Context, token string, b Binding) error {
	body, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("muster: encode the binding: %w", err)
	}
	return c.call(ctx, http.MethodPut, c.endpoint.String(), token, body)
}

// Unbind removes the caller's binding of actor (DELETE, 204); an actor
// without a binding is no error.
func (c *Client) Unbind(ctx context.Context, token, actor string) error {
	u := *c.endpoint
	u.RawQuery = url.Values{"actor": {actor}}.Encode()
	return c.call(ctx, http.MethodDelete, u.String(), token, nil)
}

func (c *Client) call(ctx context.Context, method, target, token string, body []byte) error {
	if token == "" {
		return fmt.Errorf("%w: the request carries no identity token", ErrUnauthenticated)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("muster: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The URL names no secret; the error may carry it and nothing else.
		return fmt.Errorf("muster session bindings (%s): %w", c.endpoint.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBytes))
		return nil
	}
	detail := readDetail(resp.Body)
	var sentinel error
	switch resp.StatusCode {
	case http.StatusForbidden:
		sentinel = ErrForbidden
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		sentinel = ErrInvalid
	case http.StatusUnauthorized:
		sentinel = ErrUnauthenticated
	default:
		return fmt.Errorf("muster session bindings answered %s%s", resp.Status, detail)
	}
	return fmt.Errorf("%w (%s)%s", sentinel, resp.Status, detail)
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
