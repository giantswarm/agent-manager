package skills

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// The marks the agent chart renders on an Agent's own skills Secret. Only a
// Secret carrying both, the annotation naming the Agent that references it,
// is ever written.
const (
	AgentSecretLabel      = "ui.giantswarm.io/agent-skills-git-auth"
	AgentSecretLabelValue = "agent"
	AgentSecretAnnotation = "ui.giantswarm.io/agent-skills-git-auth-agent"
)

// ScopedTokens mints installation tokens narrowed to repositories of the
// installation's account (App).
type ScopedTokens interface {
	Account(ctx context.Context) (string, error)
	ScopedTokenValidFor(ctx context.Context, repositories []string, d time.Duration) (string, time.Time, error)
}

// AgentSecretStatus is the state of one Agent's skills Secret, reported by
// get_info.
type AgentSecretStatus struct {
	Namespace string `json:"namespace"`
	Agent     string `json:"agent"`
	// SecretName is the Agent's own Secret.
	SecretName string `json:"secretName"`
	// Repositories are the owner/name pairs the token reads.
	Repositories []string `json:"repositories"`
	// Skipped are git sources of another owner than the installation's
	// account: the token cannot name them.
	Skipped []string `json:"skipped,omitempty"`
	// RefreshedAt is when the last refresh succeeded.
	RefreshedAt *time.Time `json:"refreshedAt,omitempty"`
	// ExpiresAt is when the written token expires.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// Error is the last refresh's failure; empty after a success.
	Error string `json:"error,omitempty"`
}

// AgentSecrets keeps every Agent's own skills Secret filled with an
// installation token that reads the contents of that Agent's git skill and
// plugin repositories and nothing else. It watches the Agents of its
// namespaces; an Agent's Secret is the one its git sources' credentialRef
// names, rendered by the agent chart without data together with a Role that
// lets agent-manager get and update that Secret by name. A Secret
// agent-manager may not read, or one not marked for the Agent, is left
// alone: a provisioned credential keeps working.
type AgentSecrets struct {
	tokens     ScopedTokens
	kube       kubernetes.Interface
	dyn        dynamic.Interface
	gvr        schema.GroupVersionResource
	namespaces []string
	log        *slog.Logger

	mu     sync.Mutex
	status map[string]AgentSecretStatus
	listed map[string]func(name string) (*unstructured.Unstructured, bool)
}

// NewAgentSecrets watches the Agents (gvr) of namespaces.
func NewAgentSecrets(tokens ScopedTokens, kube kubernetes.Interface, dyn dynamic.Interface, gvr schema.GroupVersionResource, namespaces []string, log *slog.Logger) *AgentSecrets {
	if log == nil {
		log = slog.Default()
	}
	return &AgentSecrets{tokens: tokens, kube: kube, dyn: dyn, gvr: gvr, namespaces: slices.Clone(namespaces), log: log,
		status: map[string]AgentSecretStatus{}, listed: map[string]func(string) (*unstructured.Unstructured, bool){}}
}

// Run watches the Agents and refreshes their Secrets until ctx ends: at once
// when an Agent appears or changes, every bootSecretInterval after a
// success, after bootSecretRetry (growing) after a failure.
func (a *AgentSecrets) Run(ctx context.Context) {
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.NewTypedItemExponentialFailureRateLimiter[string](bootSecretRetry, bootSecretInterval))
	defer queue.ShutDown()
	enqueue := func(obj any) {
		if key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err == nil {
			queue.Add(key)
		}
	}
	var synced []cache.InformerSynced
	for _, ns := range a.namespaces {
		factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(a.dyn, bootSecretInterval, ns, nil)
		informer := factory.ForResource(a.gvr).Informer()
		if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: enqueue, UpdateFunc: func(_, obj any) { enqueue(obj) }, DeleteFunc: enqueue}); err != nil {
			a.log.Error("watching Agents failed", "namespace", ns, "error", err)
			return
		}
		store := informer.GetStore()
		a.mu.Lock()
		a.listed[ns] = func(name string) (*unstructured.Unstructured, bool) {
			obj, ok, _ := store.GetByKey(ns + "/" + name)
			if !ok {
				return nil, false
			}
			u, ok := obj.(*unstructured.Unstructured)
			return u, ok
		}
		a.mu.Unlock()
		factory.Start(ctx.Done())
		synced = append(synced, informer.HasSynced)
	}
	if !cache.WaitForCacheSync(ctx.Done(), synced...) {
		return
	}
	go func() {
		<-ctx.Done()
		queue.ShutDown()
	}()
	for {
		key, shutdown := queue.Get()
		if shutdown {
			return
		}
		if err := a.refreshKey(ctx, key); err != nil && ctx.Err() == nil {
			a.log.Error("skills Secret refresh failed", "agent", key, "error", err)
			queue.AddRateLimited(key)
		} else {
			queue.Forget(key)
		}
		queue.Done(key)
	}
}

func (a *AgentSecrets) refreshKey(ctx context.Context, key string) error {
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return nil
	}
	a.mu.Lock()
	get := a.listed[ns]
	a.mu.Unlock()
	if get == nil {
		return nil
	}
	agent, ok := get(name)
	if !ok {
		a.mu.Lock()
		delete(a.status, key)
		a.mu.Unlock()
		return nil
	}
	return a.Refresh(ctx, agent)
}

// Refresh writes the Agent's Secret: a token valid for at least
// bootTokenValidity that reads the Agent's repositories, when it differs from
// the one written. An Agent without a Secret of its own is forgotten.
func (a *AgentSecrets) Refresh(ctx context.Context, agent *unstructured.Unstructured) error {
	key := agent.GetNamespace() + "/" + agent.GetName()
	st, err := a.refresh(ctx, agent)
	a.mu.Lock()
	defer a.mu.Unlock()
	if st == nil {
		delete(a.status, key)
		return err
	}
	if err != nil {
		st.Error = err.Error()
		if prev, ok := a.status[key]; ok {
			st.RefreshedAt, st.ExpiresAt = prev.RefreshedAt, prev.ExpiresAt
		}
	}
	a.status[key] = *st
	return err
}

// gitSource is one git skill or plugin source of an Agent.
type gitSource struct {
	url        string
	secretName string
	secretKey  string
}

func gitSources(agent *unstructured.Unstructured) []gitSource {
	var out []gitSource
	for _, list := range []string{"skills", "plugins"} {
		items, _, _ := unstructured.NestedSlice(agent.Object, "spec", "template", list)
		for _, item := range items {
			entry, _ := item.(map[string]any)
			url, _, _ := unstructured.NestedString(entry, "source", "git", "url")
			if url == "" {
				continue
			}
			name, _, _ := unstructured.NestedString(entry, "source", "git", "credentialRef", "name")
			key, _, _ := unstructured.NestedString(entry, "source", "git", "credentialRef", "key")
			out = append(out, gitSource{url: url, secretName: name, secretKey: key})
		}
	}
	return out
}

// errNotOwn marks a Secret that is not the Agent's own minted one.
var errNotOwn = errors.New("not the Agent's own skills Secret")

// ownSecret finds the Secret among the Agent's credentialRefs that is marked
// for this Agent. A Secret marked for another Agent is an error: two Agents
// sharing one minted Secret would flap between their repository sets.
func (a *AgentSecrets) ownSecret(ctx context.Context, agent *unstructured.Unstructured, sources []gitSource) (string, error) {
	var names []string
	for _, src := range sources {
		if src.secretName != "" && !slices.Contains(names, src.secretName) {
			names = append(names, src.secretName)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		secret, err := a.kube.CoreV1().Secrets(agent.GetNamespace()).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsForbidden(err) || apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read Secret %s/%s: %w", agent.GetNamespace(), name, err)
		}
		if secret.Labels[AgentSecretLabel] != AgentSecretLabelValue {
			continue
		}
		if owner := secret.Annotations[AgentSecretAnnotation]; owner != agent.GetName() {
			return name, fmt.Errorf("the Secret %s/%s is the skills Secret of Agent %q, not of %q: every Agent gets its own", agent.GetNamespace(), name, owner, agent.GetName())
		}
		return name, nil
	}
	return "", errNotOwn
}

func (a *AgentSecrets) refresh(ctx context.Context, agent *unstructured.Unstructured) (*AgentSecretStatus, error) {
	sources := gitSources(agent)
	name, err := a.ownSecret(ctx, agent, sources)
	if errors.Is(err, errNotOwn) {
		return nil, nil
	}
	st := &AgentSecretStatus{Namespace: agent.GetNamespace(), Agent: agent.GetName(), SecretName: name, Repositories: []string{}}
	if err != nil {
		return st, err
	}
	account, err := a.tokens.Account(ctx)
	if err != nil {
		return st, err
	}
	dataKey := ""
	var repos []string
	for _, src := range sources {
		if src.secretName != name {
			continue
		}
		if dataKey == "" {
			dataKey = src.secretKey
		} else if src.secretKey != dataKey {
			return st, fmt.Errorf("the git sources of Agent %q name Secret %s with the keys %q and %q; one key holds the token", agent.GetName(), name, dataKey, src.secretKey)
		}
		owner, repo, err := parseRepoURL(src.url)
		if err != nil || !strings.EqualFold(owner, account) {
			if !slices.Contains(st.Skipped, src.url) {
				st.Skipped = append(st.Skipped, src.url)
			}
			continue
		}
		if !slices.Contains(repos, repo) {
			repos = append(repos, repo)
		}
	}
	sort.Strings(repos)
	for _, repo := range repos {
		st.Repositories = append(st.Repositories, account+"/"+repo)
	}
	if dataKey == "" {
		dataKey = BootSecretKey
	}
	secrets := a.kube.CoreV1().Secrets(agent.GetNamespace())
	secret, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return st, fmt.Errorf("read Secret %s/%s: %w", agent.GetNamespace(), name, err)
	}
	if len(repos) == 0 {
		// An empty list would grant the whole installation: no token at all.
		if _, ok := secret.Data[dataKey]; ok {
			delete(secret.Data, dataKey)
			if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{FieldManager: "agent-manager"}); err != nil {
				return st, fmt.Errorf("clear Secret %s/%s: %w", agent.GetNamespace(), name, err)
			}
		}
		return st, fmt.Errorf("no git source of Agent %q is a repository of %s, the account the skills GitHub App is installed on", agent.GetName(), account)
	}
	token, expires, err := a.tokens.ScopedTokenValidFor(ctx, repos, bootTokenValidity)
	if err != nil {
		return st, err
	}
	value := []byte(base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token)))
	if !bytes.Equal(secret.Data[dataKey], value) {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[dataKey] = value
		if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{FieldManager: "agent-manager"}); err != nil {
			return st, fmt.Errorf("write Secret %s/%s: %w", agent.GetNamespace(), name, err)
		}
		a.log.Info("skills Secret refreshed", "agent", agent.GetName(), "namespace", agent.GetNamespace(), "secret", name, "repositories", st.Repositories, "expiresAt", expires)
	}
	now := time.Now()
	st.RefreshedAt, st.ExpiresAt = &now, &expires
	return st, nil
}

// Status reports every Agent's Secret, ordered by namespace and Agent.
func (a *AgentSecrets) Status() []AgentSecretStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AgentSecretStatus, 0, len(a.status))
	for _, st := range a.status {
		st.Repositories = slices.Clone(st.Repositories)
		st.Skipped = slices.Clone(st.Skipped)
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Agent < out[j].Agent
	})
	return out
}

// StatusOf reports one Agent's Secret; false when the Agent has none.
func (a *AgentSecrets) StatusOf(namespace, agent string) (AgentSecretStatus, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.status[namespace+"/"+agent]
	st.Repositories = slices.Clone(st.Repositories)
	st.Skipped = slices.Clone(st.Skipped)
	return st, ok
}
