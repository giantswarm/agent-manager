// Package kube builds the Kubernetes clients agent-manager writes with, and
// hides them behind an interface so the identity a write runs under can change
// without touching the service: today one client authenticated as the pod's
// ServiceAccount, later a per-call client carrying the caller's own token
// (writes-as-caller, the follow-up tracked in the README).
package kube

import (
	"context"
	"fmt"
	"os"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

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
// provider always hands out the same shared client; a caller-identity provider
// derives one per call from the bearer token on ctx, so the apiserver
// authenticates the real user and Kubernetes RBAC decides.
type Provider interface {
	// Client returns the client for this request.
	Client(ctx context.Context) (Client, error)
	// Identity names how writes are authenticated, reported by GET /info:
	// "serviceAccount" today.
	Identity() string
}

// Clients is the concrete client set built from a rest.Config.
type Clients struct {
	dynamic   dynamic.Interface
	typed     kubernetes.Interface
	discovery discovery.DiscoveryInterface
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
	return &Clients{dynamic: dyn, typed: cs, discovery: disc}, nil
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

// ServiceAccountProvider hands out one shared client: every write is
// authenticated as agent-manager's own ServiceAccount, and the agentgateway
// JWT policy in front of the route is the trust boundary.
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
func (p *ServiceAccountProvider) Identity() string { return "serviceAccount" }

// DiscoverVersion returns the API version of group that serves resource,
// preferring the server's preferred version. Used for kagent.dev (agents,
// modelconfigs) and the Flux groups (helmreleases, ocirepositories), whose
// versions differ between installations.
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
