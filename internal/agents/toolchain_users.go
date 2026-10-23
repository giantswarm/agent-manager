package agents

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/agent-manager/internal/kagent"
	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/oci"
)

// ToolchainUsage is who uses a toolchain, or one of its versions, as the
// caller sees it: the toolchain-manager's deprecate_toolchain names them
// before a toolchain leaves the offered list.
type ToolchainUsage struct {
	// Agents are the agents whose default toolchains name it.
	Agents []ToolchainAgent `json:"agents"`
	// Sessions are the Sessions that hold its digest; a suspended one
	// resumes only on it.
	Sessions []ToolchainSession `json:"sessions"`
	// Unreadable are the namespaces, agents or images that could not be
	// read as the caller, with why.
	Unreadable []string `json:"unreadable,omitempty"`
}

// ToolchainAgent is an agent whose default toolchains name a toolchain.
type ToolchainAgent struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Toolchain is its default toolchain entry, name[@version].
	Toolchain string `json:"toolchain"`
}

// ToolchainSession is a Session that mounts a toolchain.
type ToolchainSession struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace"`
	Agent     string `json:"agent"`
	// Image is the digest-pinned image the Session mounts.
	Image string `json:"image"`
	State string `json:"state,omitempty"`
}

// ToolchainUsers names the agents whose default toolchains name toolchain
// name, ImplicitDefaultToolchain an agent's without them, and the Sessions
// that mount it, in every managed namespace the caller reads. With version (vX.Y.Z or its tag X.Y.Z) only those naming that
// version and those holding its digest: an agent whose default names no
// version takes the newest offered one at each start, and does not name it.
func (s *Service) ToolchainUsers(ctx context.Context, name, version string) (*ToolchainUsage, error) {
	if !toolchainNamePattern.MatchString(name) {
		return nil, invalidf("name %q: lower-case letters, digits and dashes (at most 58)", name)
	}
	tag := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if version != "" && !toolchainVersionPattern.MatchString(tag) {
		return nil, invalidf("version %q is no image tag", version)
	}
	if !s.SessionsAvailable() {
		return nil, errSessionsUnsupported
	}
	token, err := sessionToken(ctx)
	if err != nil {
		return nil, err
	}
	out := &ToolchainUsage{Agents: []ToolchainAgent{}, Sessions: []ToolchainSession{}}
	holds := func(image *apiv1alpha1.SessionToolImage) bool { return image.GetName() == name }
	if tag != "" {
		digests, err := s.toolchainDigests(ctx, token, name, tag)
		switch {
		case err != nil && len(digests) == 0:
			// No digest, no Session can be matched to the version.
			out.Unreadable = append(out.Unreadable, fmt.Sprintf("toolchain %s %s: %v; Sessions holding it are not named", name, version, err))
		case err != nil:
			out.Unreadable = append(out.Unreadable, fmt.Sprintf("toolchain %s %s: %v; Sessions holding the main source's image are named", name, version, err))
		}
		holds = func(image *apiv1alpha1.SessionToolImage) bool {
			return image.GetName() == name && slices.ContainsFunc(digests, func(d string) bool { return strings.HasSuffix(image.GetOci(), "@"+d) })
		}
	}
	for _, ns := range s.Managed() {
		st, err := s.site(ctx, In(ns))
		if err != nil {
			out.Unreadable = append(out.Unreadable, fmt.Sprintf("namespace %s: %v", ns, err))
			continue
		}
		objects, err := st.agent.Resource(s.agentGVR()).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			out.Unreadable = append(out.Unreadable, fmt.Sprintf("namespace %s: %v", ns, wrapKube(err, "list agents in "+ns)))
			continue
		}
		for _, obj := range objects.Items {
			agent := kagent.Ref{Namespace: ns, Name: obj.GetName()}
			defaults := splitList(obj.GetAnnotations()[DefaultToolchainsAnnotation])
			if len(defaults) == 0 && s.cfg.Toolchains != nil {
				defaults = []string{ImplicitDefaultToolchain}
			}
			for _, entry := range defaults {
				n, v, _ := strings.Cut(entry, "@")
				if n == name && (tag == "" || strings.TrimPrefix(v, "v") == tag) {
					out.Agents = append(out.Agents, ToolchainAgent{Namespace: ns, Name: agent.Name, Toolchain: entry})
				}
			}
			sessions, err := s.sessionsOfAgent(ctx, token, agent)
			if err != nil {
				out.Unreadable = append(out.Unreadable, fmt.Sprintf("sessions of agent %s: %v", agent.Tenant(), err))
				continue
			}
			for _, session := range sessions {
				if session.GetState() == apiv1alpha1.RuntimeState_RUNTIME_STATE_DELETED {
					continue
				}
				if i := slices.IndexFunc(session.GetToolImages(), holds); i >= 0 {
					out.Sessions = append(out.Sessions, ToolchainSession{
						ID: session.GetId(), Namespace: ns, Agent: agent.Name,
						Image: session.GetToolImages()[i].GetOci(), State: strings.TrimPrefix(session.GetState().String(), "RUNTIME_STATE_"),
					})
				}
			}
		}
	}
	return out, nil
}

// sessionsOfAgent is every person's Sessions of the agent when the
// controller grants the caller that, else the caller's own.
func (s *Service) sessionsOfAgent(ctx context.Context, token string, agent kagent.Ref) ([]*apiv1alpha1.Session, error) {
	sessions, err := s.cfg.Sessions.ListSessions(ctx, token, agent, true)
	if err == nil {
		return sessions, nil
	}
	if !errors.Is(kagentError(err, ""), ErrForbidden) {
		return nil, kagentError(err, fmt.Sprintf("sessions of agent %s", agent.Tenant()))
	}
	sessions, err = s.cfg.Sessions.ListSessions(ctx, token, agent, false)
	if err != nil {
		return nil, kagentError(err, fmt.Sprintf("sessions of agent %s", agent.Tenant()))
	}
	return sessions, nil
}

// toolchainDigests are the digests toolchain name's image tag resolves to:
// in the repository of every source the toolchain-manager lists the version
// of for the caller, whose IdP token is token, and the main source's. A list
// that cannot be read leaves the main source's, with the error.
func (s *Service) toolchainDigests(ctx context.Context, token, name, tag string) ([]string, error) {
	src := s.cfg.Toolchains
	if src == nil {
		return nil, errToolchainsUnsupported
	}
	repos := []oci.Reference{src.repositoryOf(name)}
	var listErr error
	if catalog := s.cfg.ToolchainCatalog; catalog != nil {
		offer, err := catalog.Offered(ctx, token, name)
		if err != nil {
			listErr = fmt.Errorf("the toolchain-manager's list could not be read: %w", err)
		}
		for _, image := range offer.Images["v"+tag] {
			if ref, err := oci.ParseImageReference(image); err == nil && !slices.Contains(repos, ref.Reference) {
				repos = append(repos, ref.Reference)
			}
		}
	}
	var digests []string
	var missing []string
	for _, repo := range repos {
		digest, err := src.Registry.ManifestDigest(ctx, repo, tag)
		switch {
		case errors.Is(err, oci.ErrNotFound):
			missing = append(missing, fmt.Sprintf("%s/%s:%s", repo.Host, repo.Repository, tag))
		case err != nil:
			return digests, err
		default:
			digests = append(digests, digest)
		}
	}
	if len(digests) == 0 {
		return nil, errors.Join(listErr, fmt.Errorf("%s is not published", strings.Join(missing, ", ")))
	}
	return digests, listErr
}
