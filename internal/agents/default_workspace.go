package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/giantswarm/agent-manager/internal/workspaces"
)

// An agent's default workspace is the workspace its Sessions start in when
// start_session names none: neither a workspaceGrant nor noWorkspace. It is a
// name, <namespace>/<name>, kept as DefaultWorkspaceAnnotation in the chart's
// annotations value, which the chart puts on the Agent unchanged and kagent
// ignores. It mounts nothing and admits no one: whether the workspace exists
// and whether the person starting a Session may read it is the
// workspace-manager's answer at every start, as the person.

// DefaultWorkspaceAnnotation names the agent's default workspace, on the
// Agent object through the chart's annotations value. Set only by
// defaultWorkspace.
const DefaultWorkspaceAnnotation = "agent-platform.giantswarm.io/default-workspace"

// WorkspaceAuthorizer asks the workspace-manager, as the caller whose IdP
// token it is given, whether the caller may start a Session on a workspace
// (workspaces.Client).
type WorkspaceAuthorizer interface {
	Authorize(ctx context.Context, bearer, name string) (*workspaces.Authorization, error)
	SignInLink(ctx context.Context, bearer, server string) (string, error)
}

// ValidateDefaultWorkspace checks a default workspace's form and that it is
// in the agent's namespace, its tenant boundary. Empty is no default.
func ValidateDefaultWorkspace(workspace, agentNamespace string, target Target) error {
	if workspace == "" {
		return nil
	}
	if target.IsSet() {
		return invalidf("defaultWorkspace: Sessions start only on agents of the installation's own cluster, so an agent on a workload cluster has no default workspace")
	}
	ns, name, ok := strings.Cut(workspace, "/")
	if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
		return invalidf("defaultWorkspace %q: want <namespace>/<name>", workspace)
	}
	if msgs := validation.IsDNS1123Label(ns); len(msgs) > 0 {
		return invalidf("defaultWorkspace %q: the namespace is no namespace name: %s", workspace, strings.Join(msgs, "; "))
	}
	if msgs := validation.IsDNS1123Subdomain(name); len(msgs) > 0 {
		return invalidf("defaultWorkspace %q: the name is no workspace name: %s", workspace, strings.Join(msgs, "; "))
	}
	if ns != agentNamespace {
		return invalidf("defaultWorkspace %q: a workspace of namespace %s cannot be the default of an agent in %s, its tenant", workspace, ns, agentNamespace)
	}
	return nil
}

// refuseDefaultWorkspaceAnnotation refuses the annotation passed through
// annotations with a value other than current.
func refuseDefaultWorkspaceAnnotation(requested map[string]string, current string) error {
	if v, ok := requested[DefaultWorkspaceAnnotation]; ok && v != current {
		return invalidf("annotation %s is set by defaultWorkspace", DefaultWorkspaceAnnotation)
	}
	return nil
}

// defaultWorkspaceOf reads the default workspace from the chart's values.
func defaultWorkspaceOf(values map[string]any) string {
	annotations, _ := values["annotations"].(map[string]any)
	workspace, _ := annotations[DefaultWorkspaceAnnotation].(string)
	return workspace
}

// setDefaultWorkspace records workspace in the chart's annotations value;
// empty removes it.
func setDefaultWorkspace(values map[string]any, workspace string) {
	annotations, _ := values["annotations"].(map[string]any)
	if workspace == "" {
		if annotations != nil {
			delete(annotations, DefaultWorkspaceAnnotation)
			if len(annotations) == 0 {
				delete(values, "annotations")
			}
		}
		return
	}
	if annotations == nil {
		annotations = map[string]any{}
	}
	annotations[DefaultWorkspaceAnnotation] = workspace
	values["annotations"] = annotations
}

// mergeDefaultWorkspace carries the default workspace of before into after,
// or the one upd sets ("" removes it). annotations never change it.
func mergeDefaultWorkspace(before, after map[string]any, upd Update, agentNamespace string) error {
	current := defaultWorkspaceOf(before)
	if upd.Annotations != nil {
		if err := refuseDefaultWorkspaceAnnotation(*upd.Annotations, current); err != nil {
			return err
		}
	}
	next := current
	if upd.DefaultWorkspace != nil {
		next = strings.TrimSpace(*upd.DefaultWorkspace)
		if err := ValidateDefaultWorkspace(next, agentNamespace, upd.Target); err != nil {
			return err
		}
	}
	setDefaultWorkspace(after, next)
	return nil
}

// defaultWorkspaceOfAgent is the default workspace of the agent ref as its
// Agent object carries it; "" for an agent without one or without an object.
func (s *Service) defaultWorkspaceOfAgent(ctx context.Context, namespace, name string) (string, error) {
	st, err := s.site(ctx, In(namespace))
	if err != nil {
		return "", err
	}
	obj, err := s.getAgentObject(ctx, st.agent, namespace, name)
	if err != nil || obj == nil {
		return "", err
	}
	return obj.GetAnnotations()[DefaultWorkspaceAnnotation], nil
}

// authorizeDefaultWorkspace asks the workspace-manager, as the caller, for a
// grant of the agent's default workspace, through muster. Every answer but a
// grant refuses the start and names the workspace: the repositories the
// caller may not read (forbidden), the providers to connect and their link
// (auth_required), a workspace that is gone (not_found) or cannot be started
// on yet (conflict).
func (s *Service) authorizeDefaultWorkspace(ctx context.Context, token, workspace string) (string, error) {
	if s.cfg.Workspaces == nil || s.cfg.Bindings == nil {
		return "", fmt.Errorf("%w: the agent's default workspace %s needs the workspace-manager through muster and muster's session bindings, which this installation does not configure: pass noWorkspace to start without it", ErrUnsupported, workspace)
	}
	_, name, _ := strings.Cut(workspace, "/")
	a, err := s.cfg.Workspaces.Authorize(ctx, token, name)
	var challenge *workspaces.AuthRequiredError
	var toolErr *workspaces.ToolError
	switch {
	case errors.As(err, &challenge):
		return "", authRequiredf("the agent's default workspace %s: sign in to muster server %s first%s (this is the workspace-manager's sign-in, not agent-manager's GitHub one)", workspace, challenge.Server, linkSuffix(challenge.LoginURL))
	case errors.As(err, &toolErr) && toolErr.NotFound():
		return "", notFoundf("the agent's default workspace %s: %s; pass another workspaceGrant or noWorkspace", workspace, toolErr.Message)
	case err != nil:
		return "", fmt.Errorf("the agent's default workspace %s could not be authorized: %w", workspace, err)
	}
	switch a.Outcome {
	case workspaces.Granted:
		return a.Grant, nil
	case workspaces.Refused:
		return "", forbiddenf("the agent's default workspace %s: you cannot read %s; pass another workspaceGrant or noWorkspace", workspace, strings.Join(a.Unreadable, ", "))
	case workspaces.ConnectRequired:
		return "", authRequiredf("the agent's default workspace %s: connect %s in muster first (the workspace's provider sign-in, not agent-manager's GitHub one)", workspace, s.connectList(ctx, token, a.Connect))
	default:
		return "", conflictf("the agent's default workspace %s cannot be started on: %s; pass another workspaceGrant or noWorkspace", workspace, a.Reason)
	}
}

// connectList names each provider to connect, the muster server of its App
// and, when muster answers one, its core_auth_login link.
func (s *Service) connectList(ctx context.Context, token string, connect []workspaces.Connect) string {
	parts := make([]string, 0, len(connect))
	for _, c := range connect {
		server := c.Server()
		if server == "" {
			parts = append(parts, fmt.Sprintf("provider %s (%s)", c.Provider, c.SignIn))
			continue
		}
		link, err := s.cfg.Workspaces.SignInLink(ctx, token, server)
		if err != nil {
			s.log.WarnContext(ctx, "core_auth_login failed", "server", server, "error", err)
		}
		parts = append(parts, fmt.Sprintf("provider %s at muster server %s%s", c.Provider, server, linkSuffix(link)))
	}
	return strings.Join(parts, "; ")
}

func linkSuffix(link string) string {
	if link == "" {
		return ""
	}
	return ": " + link
}
