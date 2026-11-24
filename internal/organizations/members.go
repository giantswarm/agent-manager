package organizations

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kube"
)

// The organization roles, by their short names: what list_organization_members
// reads.
var memberRoles = map[string]string{roleAdminShort: RoleAdmin, "editor": RoleEditor, "user": RoleUser}

// roleAdminShort is the admin role's short name. The admins are not written
// by the member tools: they are spec.admins of the Organization, which
// agent-platform-operator binds to RoleAdmin in the organization namespace.
const roleAdminShort = "admin"

// adminsBinding is the RoleBinding to RoleAdmin that agent-platform-operator
// derives from the Organization's spec.admins.
const adminsBinding = "organization-admins"

// adminsFrom says where the admins come from and how they change.
const adminsFrom = "the Organization's spec.admins, which agent-platform-operator binds to " + RoleAdmin + "; update_organization (adminGroups, adminUsers) changes them"

// memberBinding is the RoleBinding add_organization_member writes a role's
// members into.
func memberBinding(role string) string { return "organization-" + role + "s" }

// Member is one subject bound to an organization role.
type Member struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// Role is admin, editor or user.
	Role string `json:"role"`
	// Binding is the RoleBinding that grants it.
	Binding string `json:"binding"`
	// From says where an admin comes from: the Organization, not a binding
	// the member tools write.
	From string `json:"from,omitempty"`
}

// Members is list_organization_members's answer.
type Members struct {
	Namespace string   `json:"namespace"`
	Members   []Member `json:"members"`
}

// organization resolves an organization namespace argument: it must be one
// agent-platform-operator keeps.
func (s *Service) organization(ctx context.Context, ns string) (Namespace, error) {
	if s.cfg.Namespaces == nil {
		return Namespace{}, fmt.Errorf("%w: organization management is disabled", agents.ErrUnsupported)
	}
	if ns == "" {
		return Namespace{}, invalidf("namespace (the organization namespace) is required; list_organizations lists yours, get_organization names an organization's")
	}
	org, ok, err := s.cfg.Namespaces.Get(ctx, ns)
	if err != nil {
		return Namespace{}, fmt.Errorf("%w: %v", agents.ErrUnsupported, err)
	}
	if !ok {
		return Namespace{}, invalidf("namespace %s is not an organization namespace; list_organizations lists yours", ns)
	}
	return org, nil
}

// ListMembers lists the subjects bound to the organization's roles in ns, as
// the caller: groups first.
func (s *Service) ListMembers(ctx context.Context, ns string) (*Members, error) {
	if _, err := s.organization(ctx, ns); err != nil {
		return nil, err
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	bindings, err := s.roleBindings(ctx, c, ns, wayMemberRead(ns))
	if err != nil {
		return nil, err
	}
	out := &Members{Namespace: ns, Members: []Member{}}
	for _, b := range bindings {
		for _, sub := range bindingSubjects(&b.obj) {
			m := Member{Kind: sub.Kind, Name: sub.Name, Namespace: sub.Namespace, Role: b.role, Binding: b.obj.GetName()}
			if b.role == roleAdminShort && b.obj.GetName() == adminsBinding {
				m.From = adminsFrom
			}
			out.Members = append(out.Members, m)
		}
	}
	slices.SortStableFunc(out.Members, func(a, b Member) int {
		if a.Kind != b.Kind {
			return kindOrder(a.Kind) - kindOrder(b.Kind)
		}
		return strings.Compare(a.Name+a.Role, b.Name+b.Role)
	})
	return out, nil
}

func kindOrder(kind string) int {
	switch kind {
	case "Group":
		return 0
	case "User":
		return 1
	}
	return 2
}

type roleBindingOf struct {
	obj  unstructured.Unstructured
	role string
}

// wayMemberRead is the way to read the members of ns: the list right on
// RoleBindings there. Every member role holds it from agent-platform chart
// 4.179.0 on (giantswarm/agent-platform#1369); before, agent-platform:admin
// alone did.
func wayMemberRead(ns string) string {
	return fmt.Sprintf("reading the members needs the right to list RoleBindings in organization namespace %s. Every member role (%s, %s or %s) has it from agent-platform chart 4.179.0 on; before that chart, %s alone. An admin of the organization adds you as a user or an editor (add_organization_member), or an organizations-admin (%s) makes you an admin (update_organization)", ns, RoleUser, RoleEditor, RoleAdmin, RoleAdmin, RoleOrganizationsAdmin)
}

// roleBindings are the RoleBindings of ns to the organization's roles; way
// is the way out of a refused read.
func (s *Service) roleBindings(ctx context.Context, c kube.Client, ns, way string) ([]roleBindingOf, error) {
	list, err := c.Dynamic().Resource(rbacGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, kubeErr(err, "list RoleBindings in "+ns, way)
	}
	var out []roleBindingOf
	for _, item := range list.Items {
		kind, _, _ := unstructured.NestedString(item.Object, "roleRef", "kind")
		name, _, _ := unstructured.NestedString(item.Object, "roleRef", "name")
		if kind != "ClusterRole" {
			continue
		}
		for short, role := range memberRoles {
			if name == role {
				out = append(out, roleBindingOf{obj: item, role: short})
			}
		}
	}
	slices.SortFunc(out, func(a, b roleBindingOf) int { return strings.Compare(a.obj.GetName(), b.obj.GetName()) })
	return out, nil
}

func bindingSubjects(obj *unstructured.Unstructured) []Subject {
	raw, _, _ := unstructured.NestedSlice(obj.Object, "subjects")
	out := make([]Subject, 0, len(raw))
	for _, v := range raw {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		sub := Subject{}
		sub.Kind, _ = m["kind"].(string)
		sub.Name, _ = m["name"].(string)
		sub.Namespace, _ = m["namespace"].(string)
		out = append(out, sub)
	}
	return out
}

// MemberRequest is add_organization_member's and remove_organization_member's
// arguments: exactly one of Group and User.
type MemberRequest struct {
	Namespace string
	// Role is editor or user; on a remove empty means both. admin is
	// refused: the admins are the Organization's spec.admins.
	Role   string
	Group  string
	User   string
	DryRun bool
	// Mode is apply (default) or commit.
	Mode string
}

func (r MemberRequest) subject() (Subject, error) {
	switch {
	case r.Group != "" && r.User != "":
		return Subject{}, invalidf("name a group or a user, not both")
	case r.Group != "":
		return Subject{Kind: "Group", Name: r.Group}, nil
	case r.User != "":
		return Subject{Kind: "User", Name: r.User}, nil
	}
	return Subject{}, invalidf("group (an identity provider group, preferred: membership then changes in the identity provider) or user is required")
}

// roleOf is the ClusterRole of a role the member tools write: editor or
// user. admin is refused with the way to change the admins.
func roleOf(short string) (string, error) {
	if short == roleAdminShort {
		return "", invalidf("the member tools do not give or remove the admin role: the admins are %s", adminsFrom)
	}
	role, ok := memberRoles[short]
	if !ok {
		return "", invalidf("role %q: editor or user", short)
	}
	return role, nil
}

// MemberResult is the answer of a member change.
type MemberResult struct {
	Namespace string `json:"namespace"`
	Member    Member `json:"member"`
	// Changed: false when the subject already held (or did not hold) the
	// role.
	Changed bool `json:"changed"`
	// Bindings are the RoleBindings the change wrote or deleted.
	Bindings    []string `json:"bindings,omitempty"`
	DryRun      bool     `json:"dryRun,omitempty"`
	RequestedBy string   `json:"requestedBy,omitempty"`
	// Commit is the pull request of a change in mode commit.
	Commit *CommitResult `json:"commit,omitempty"`
	// Note says what a remove left: an admin role, which comes from the
	// Organization.
	Note string `json:"note,omitempty"`
}

// AddMember binds a group or a user to an organization role as the caller,
// dry run first: into the role's RoleBinding organization-<role>s.
func (s *Service) AddMember(ctx context.Context, req MemberRequest) (*MemberResult, error) {
	sub, err := req.subject()
	if err != nil {
		return nil, err
	}
	role, err := roleOf(req.Role)
	if err != nil {
		return nil, err
	}
	org, err := s.organization(ctx, req.Namespace)
	if err != nil {
		return nil, err
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ns := req.Namespace
	name := memberBinding(req.Role)
	res := &MemberResult{Namespace: ns, Member: Member{Kind: sub.Kind, Name: sub.Name, Role: req.Role, Binding: name}, DryRun: req.DryRun, RequestedBy: identity.Caller(ctx)}
	// The caller reads first: one who may not gets forbidden, not the
	// organization's state.
	bindings, err := s.roleBindings(ctx, c, ns, wayOrganizationAdmin(ns))
	if err != nil {
		return nil, err
	}
	cw, err := s.contentWriter(ctx, org, req.Mode)
	if err != nil {
		return nil, err
	}
	for _, b := range bindings {
		if b.role == req.Role && slices.ContainsFunc(bindingSubjects(&b.obj), sub.matches) {
			res.Member.Binding = b.obj.GetName()
			return res, nil
		}
	}
	way := wayOrganizationAdmin(ns)
	what := fmt.Sprintf("RoleBinding %s/%s", ns, name)
	existing, err := c.Dynamic().Resource(rbacGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	var w write
	switch {
	case apierrors.IsNotFound(err):
		w = write{gvr: rbacGVR, kind: kindRoleBinding, op: opCreate, obj: roleBinding(ns, name, role, []Subject{sub}), ns: ns, name: name, what: what, reason: way}
	case err != nil:
		return nil, kubeErr(err, "get "+what, way)
	default:
		obj, err := cw.source(ctx, existing, kindRoleBinding)
		if err != nil {
			return nil, err
		}
		if ref, _, _ := unstructured.NestedString(obj.Object, "roleRef", "name"); ref != role {
			return nil, fmt.Errorf("%w: %s binds %s, not %s: agent-manager writes the %s members into it; rename or remove that binding first", agents.ErrConflict, what, ref, role, req.Role)
		}
		obj = obj.DeepCopy()
		subs, _, _ := unstructured.NestedSlice(obj.Object, "subjects")
		subs = append(subs, subjectObject(sub))
		if err := unstructured.SetNestedSlice(obj.Object, subs, "subjects"); err != nil {
			return nil, err
		}
		w = write{gvr: rbacGVR, kind: kindRoleBinding, op: opUpdate, obj: obj.Object, ns: ns, name: name, what: what, reason: way}
	}
	if res.Commit, err = cw.land(ctx, s, []write{w}, req.DryRun, "member-add-"+req.Role+"-"+slug(sub.Kind+"-"+sub.Name), "chore(organizations): add "+req.Role+" "+sub.Name); err != nil {
		return nil, err
	}
	res.Changed, res.Bindings = true, []string{name}
	if !req.DryRun && res.Commit == nil {
		s.log.Info("organization member added", identity.LogAttr(ctx), "namespace", ns, "role", req.Role, "kind", sub.Kind)
	}
	return res, nil
}

func (s Subject) matches(o Subject) bool {
	return s.Kind == o.Kind && s.Name == o.Name && s.Namespace == o.Namespace
}

// RemoveMember unbinds a group or a user from an organization role (editor
// and user when Role is empty) as the caller, dry run first: from every
// RoleBinding of the role that holds it, deleting a binding it empties. An
// admin stays: the admins are the Organization's spec.admins.
func (s *Service) RemoveMember(ctx context.Context, req MemberRequest) (*MemberResult, error) {
	sub, err := req.subject()
	if err != nil {
		return nil, err
	}
	if req.Role != "" {
		if _, err := roleOf(req.Role); err != nil {
			return nil, err
		}
	}
	org, err := s.organization(ctx, req.Namespace)
	if err != nil {
		return nil, err
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	ns := req.Namespace
	res := &MemberResult{Namespace: ns, Member: Member{Kind: sub.Kind, Name: sub.Name, Role: req.Role}, DryRun: req.DryRun, RequestedBy: identity.Caller(ctx)}
	// The caller reads first: one who may not gets forbidden, not the
	// organization's state.
	bindings, err := s.roleBindings(ctx, c, ns, wayOrganizationAdmin(ns))
	if err != nil {
		return nil, err
	}
	cw, err := s.contentWriter(ctx, org, req.Mode)
	if err != nil {
		return nil, err
	}
	way := wayOrganizationAdmin(ns)
	var writes []write
	for _, b := range bindings {
		if req.Role != "" && b.role != req.Role {
			continue
		}
		if !slices.ContainsFunc(bindingSubjects(&b.obj), sub.matches) {
			continue
		}
		if b.role == roleAdminShort {
			// The member tools write no admin binding: the operator's
			// organization-admins follows spec.admins, and another one is
			// not the member tools' to change.
			if b.obj.GetName() == adminsBinding {
				res.Note = fmt.Sprintf("%s %s stays an admin: the admins are %s", sub.Kind, sub.Name, adminsFrom)
			} else {
				res.Note = fmt.Sprintf("%s %s stays an admin by the RoleBinding %s, which the member tools do not change; the organization's admins are %s", sub.Kind, sub.Name, b.obj.GetName(), adminsFrom)
			}
			continue
		}
		src, err := cw.source(ctx, &b.obj, kindRoleBinding)
		if err != nil {
			return nil, err
		}
		subs, _, _ := unstructured.NestedSlice(src.Object, "subjects")
		kept := make([]any, 0, len(subs))
		for _, v := range subs {
			m, _ := v.(map[string]any)
			if m != nil && sub.matches(subjectFrom(m)) {
				continue
			}
			kept = append(kept, v)
		}
		if len(kept) == len(subs) {
			continue
		}
		name := b.obj.GetName()
		what := fmt.Sprintf("RoleBinding %s/%s", ns, name)
		reason := way
		if len(kept) == 0 {
			writes = append(writes, write{gvr: rbacGVR, kind: kindRoleBinding, op: opDelete, ns: ns, name: name, what: what, reason: reason})
		} else {
			obj := src.DeepCopy()
			if err := unstructured.SetNestedSlice(obj.Object, kept, "subjects"); err != nil {
				return nil, err
			}
			writes = append(writes, write{gvr: rbacGVR, kind: kindRoleBinding, op: opUpdate, obj: obj.Object, ns: ns, name: name, what: what, reason: reason})
		}
		res.Bindings = append(res.Bindings, name)
		if res.Member.Role == "" {
			res.Member.Role = b.role
		}
	}
	if len(writes) == 0 {
		return res, nil
	}
	if res.Commit, err = cw.land(ctx, s, writes, req.DryRun, "member-remove-"+slug(sub.Kind+"-"+sub.Name), "chore(organizations): remove member "+sub.Name); err != nil {
		return nil, err
	}
	res.Changed = true
	if !req.DryRun && res.Commit == nil {
		s.log.Info("organization member removed", identity.LogAttr(ctx), "namespace", ns, "role", req.Role, "kind", sub.Kind)
	}
	return res, nil
}

func subjectFrom(m map[string]any) Subject {
	s := Subject{}
	s.Kind, _ = m["kind"].(string)
	s.Name, _ = m["name"].(string)
	s.Namespace, _ = m["namespace"].(string)
	return s
}

// slug makes s fit a branch name: lower case letters, digits, dots and
// dashes, at most 40 characters.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if len(out) > 40 {
		out = strings.Trim(out[:40], "-.")
	}
	return out
}
