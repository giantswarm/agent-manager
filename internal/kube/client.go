// Package kube builds the Kubernetes clients agent-manager reads and writes
// with, and hides them behind an interface so the identity a call runs under
// is a deployment choice: one shared client authenticated as the pod's
// ServiceAccount (a server nothing but a trusted proxy can reach), or — with
// OAuth and downstream OAuth on — a client per caller that presents the
// caller's own IdP token, so the apiserver authenticates the real user and the
// user's RBAC decides. In that mode the ServiceAccount holds no permissions
// (the chart renders no RBAC for it) and nothing runs without a caller.
package kube

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/giantswarm/agent-manager/internal/identity"
)

// Identities a Provider reports through GET /info.
const (
	// IdentityServiceAccount: every call is the pod's ServiceAccount.
	IdentityServiceAccount = "serviceAccount"
	// IdentityCaller: every call presents the caller's IdP token.
	IdentityCaller = "caller"
)

// ErrNoCallerToken is returned by the caller provider when the request
// carries no IdP token: the server runs as the caller only, and there is no
// other credential to fall back to.
var ErrNoCallerToken = errors.New("no caller token on the request")

// Client is what the service needs from Kubernetes: the dynamic client for the
// custom resources (HelmRelease, OCIRepository, Agent, ModelConfig), the typed
// client for core objects (Deployments, Pods, Events) and discovery for the
// served API versions.
type Client interface {
	Dynamic() dynamic.Interface
	Typed() kubernetes.Interface
	Discovery() discovery.DiscoveryInterface
}

// Provider resolves the client a request runs with. The ServiceAccount
// provider always hands out the same shared client; the caller provider
// derives one per call from the IdP token on ctx.
type Provider interface {
	// Client returns the client for this request.
	Client(ctx context.Context) (Client, error)
	// Identity names how calls are authenticated, reported by GET /info:
	// IdentityServiceAccount or IdentityCaller.
	Identity() string
}

// Clients is the concrete client set built from a rest.Config.
type Clients struct {
	dynamic   dynamic.Interface
	typed     kubernetes.Interface
	discovery discovery.DiscoveryInterface
	restCfg   *rest.Config
}

// Dynamic implements Client.
func (c *Clients) Dynamic() dynamic.Interface { return c.dynamic }

// Typed implements Client.
func (c *Clients) Typed() kubernetes.Interface { return c.typed }

// Discovery implements Client.
func (c *Clients) Discovery() discovery.DiscoveryInterface { return c.discovery }

// FromInterfaces wraps existing clients (tests use the fakes).
func FromInterfaces(dyn dynamic.Interface, typed kubernetes.Interface, disc discovery.DiscoveryInterface) *Clients {
	return &Clients{dynamic: dyn, typed: typed, discovery: disc}
}

// Config selects how to reach the API server.
type Config struct {
	// Kubeconfig is an explicit kubeconfig path; empty uses the default
	// loading rules ($KUBECONFIG, ~/.kube/config).
	Kubeconfig string
	// Context overrides the kubeconfig's current context.
	Context string
	// InCluster forces in-cluster auth. When false and no kubeconfig is
	// found, in-cluster auth is still tried if the pod environment is present.
	InCluster bool
}

// New builds the clients.
func New(cfg Config) (*Clients, error) {
	restCfg, err := restConfig(cfg)
	if err != nil {
		return nil, err
	}
	restCfg.UserAgent = "agent-manager"
	return fromRESTConfig(restCfg)
}

func fromRESTConfig(restCfg *rest.Config) (*Clients, error) {
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("clientset: %w", err)
	}
	return &Clients{dynamic: dyn, typed: cs, discovery: disc, restCfg: restCfg}, nil
}

// ForToken returns clients that authenticate to the API server with token (an
// OIDC id_token the apiserver trusts) instead of the ServiceAccount. Server
// address, CA and TLS settings are kept; every credential of the base config
// is dropped so nothing but the caller's token is presented.
func (c *Clients) ForToken(token string) (*Clients, error) {
	if token == "" {
		return nil, ErrNoCallerToken
	}
	if c.restCfg == nil {
		return nil, fmt.Errorf("caller clients need a REST config (built with New)")
	}
	cfg := rest.AnonymousClientConfig(c.restCfg)
	cfg.BearerToken = token
	cfg.UserAgent = c.restCfg.UserAgent
	return fromRESTConfig(cfg)
}

func restConfig(cfg Config) (*rest.Config, error) {
	if cfg.InCluster {
		c, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("in-cluster config: %w", err)
		}
		return c, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if cfg.Kubeconfig != "" {
		rules.ExplicitPath = cfg.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if cfg.Context != "" {
		overrides.CurrentContext = cfg.Context
	}
	c, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err == nil {
		return c, nil
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		if ic, icErr := rest.InClusterConfig(); icErr == nil {
			return ic, nil
		}
	}
	return nil, fmt.Errorf("kubeconfig: %w", err)
}

// ServiceAccountProvider hands out one shared client: every call is
// authenticated as agent-manager's own ServiceAccount, and the proxy in front
// of the server (the agentgateway JWT policy, muster) is the trust boundary.
type ServiceAccountProvider struct {
	client Client
}

// NewServiceAccountProvider wraps c.
func NewServiceAccountProvider(c Client) *ServiceAccountProvider {
	return &ServiceAccountProvider{client: c}
}

// Client implements Provider.
func (p *ServiceAccountProvider) Client(context.Context) (Client, error) { return p.client, nil }

// Identity implements Provider.
func (p *ServiceAccountProvider) Identity() string { return IdentityServiceAccount }

// CallerProvider hands out clients that present the caller's IdP token (put on
// the context by the OAuth layer with downstream OAuth on). There is no
// fallback: a request without a token gets ErrNoCallerToken, and a token that
// expires while its request is still running keeps being presented — the
// apiserver answers 401, the request fails attributed to the caller, and the
// operation stays bounded by the caller's own credential. Clients are cached
// per token until the token's exp.
type CallerProvider struct {
	base *Clients
	log  *slog.Logger

	mu      sync.Mutex
	byToken map[[32]byte]*callerEntry
}

// callerEntry caches the clients built for one caller token.
type callerEntry struct {
	clients *Clients
	expires time.Time
	// expiredLogged: the expiry is logged once per token, not on every call
	// a request makes after its token ran out.
	expiredLogged bool
}

// maxCallerClients bounds the per-token cache; entries expire with their token
// anyway, this only guards against a flood of distinct tokens.
const maxCallerClients = 256

// NewCallerProvider builds per-caller clients from base (its server address,
// CA and TLS settings; never its credentials).
func NewCallerProvider(base *Clients, log *slog.Logger) *CallerProvider {
	if log == nil {
		log = slog.Default()
	}
	return &CallerProvider{base: base, log: log, byToken: map[[32]byte]*callerEntry{}}
}

// Client implements Provider.
func (p *CallerProvider) Client(ctx context.Context) (Client, error) {
	token, ok := identity.TokenFromContext(ctx)
	if !ok {
		return nil, ErrNoCallerToken
	}
	key := sha256.Sum256([]byte(token))
	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()
	e, cached := p.byToken[key]
	if !cached {
		if len(p.byToken) >= maxCallerClients {
			p.evictLocked(now)
		}
		clients, err := p.base.ForToken(token)
		if err != nil {
			return nil, fmt.Errorf("caller clients: %w", err)
		}
		e = &callerEntry{clients: clients, expires: identity.TokenExpiry(token)}
		p.byToken[key] = e
	}
	if !e.expires.IsZero() && now.After(e.expires) && !e.expiredLogged {
		e.expiredLogged = true
		p.log.Warn("caller token expired; the Kubernetes API will reject the remaining calls of this request (the ServiceAccount holds no permissions)",
			identity.LogAttr(ctx), "expired", e.expires.UTC().Format(time.RFC3339))
	}
	return e.clients, nil
}

// Identity implements Provider.
func (p *CallerProvider) Identity() string { return IdentityCaller }

// evictLocked drops expired entries, and when nothing expired the whole cache
// (a token flood is not worth an LRU).
func (p *CallerProvider) evictLocked(now time.Time) {
	for k, e := range p.byToken {
		if !e.expires.IsZero() && now.After(e.expires) {
			delete(p.byToken, k)
		}
	}
	if len(p.byToken) >= maxCallerClients {
		p.byToken = map[[32]byte]*callerEntry{}
	}
}

// DiscoverVersion returns the API version of group that serves resource,
// preferring the server's preferred version. Used for kagent.dev (agents,
// modelconfigs) and the Flux groups (helmreleases, ocirepositories), whose
// versions differ between installations. Discovery is what every
// authenticated principal may read, so the ServiceAccount answers it even
// when it holds no other permission.
func DiscoverVersion(dc discovery.DiscoveryInterface, group, resource string) (string, error) {
	groups, err := dc.ServerGroups()
	if err != nil {
		return "", fmt.Errorf("discover API groups: %w", err)
	}
	for _, g := range groups.Groups {
		if g.Name != group {
			continue
		}
		candidates := []string{g.PreferredVersion.Version}
		for _, v := range g.Versions {
			if v.Version != g.PreferredVersion.Version {
				candidates = append(candidates, v.Version)
			}
		}
		for _, v := range candidates {
			res, err := dc.ServerResourcesForGroupVersion(group + "/" + v)
			if err != nil {
				continue
			}
			for _, r := range res.APIResources {
				if r.Name == resource {
					return v, nil
				}
			}
		}
		return "", fmt.Errorf("group %s has no %s resource", group, resource)
	}
	return "", fmt.Errorf("API group %s not found", group)
}
