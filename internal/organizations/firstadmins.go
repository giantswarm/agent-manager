package organizations

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/agent-platform-operator/pkg/adminmirror"
	authenticationv1 "k8s.io/api/authentication/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
	"github.com/giantswarm/agent-manager/internal/kube"
)

// mirrorRule is adminmirror's rule in words: whose rights in the parent
// agent-platform-operator copies into an organization namespace.
var mirrorRule = fmt.Sprintf("agent-platform-operator binds in an organization namespace, as %s, the Users and Groups of the parent's RoleBindings to the ClusterRole %s and to the ClusterRole %s labelled %s: %s, without ServiceAccounts and system: names",
	adminmirror.OrganizationsAdminClusterRole, adminmirror.OrganizationsAdminClusterRole, adminmirror.ClusterAdminClusterRole, adminmirror.RBACOperatorLabel, adminmirror.RBACOperatorValue)

// firstAdminsAccess is what binding the first admins asks in ns: create the
// RoleBinding and bind RoleAdmin.
func firstAdminsAccess(ns string) []access {
	return []access{
		{namespace: ns, verb: "create", group: rbacGVR.Group, resource: rbacGVR.Resource},
		{namespace: ns, verb: "bind", group: rbacGVR.Group, resource: "clusterroles", name: RoleAdmin},
	}
}

// requireRightReachesOrganization refuses, before the organization is
// written, a caller whose right in parent will not reach the organization
// namespace, so the first admins could not be bound and the organization
// would end Pending. An access review cannot say where a right comes from,
// so it asks as the caller, in this order:
//
//   - may the caller bind the first admins cluster-wide (a cluster-admin, the
//     installation's customer admin groups)? Then the right reaches every
//     namespace.
//   - may the caller list the RoleBindings of parent? Then one of them must be
//     one the operator mirrors now (adminmirror.Mirrors: not one being
//     deleted) with the caller among its mirrored subjects
//     (adminmirror.MirroredSubjects), as the API server authenticates the
//     caller (a SelfSubjectReview).
//   - otherwise the source cannot be read, and the create goes on as before:
//     the access review in the organization namespace after Ready stays the
//     safety net, and answers Pending.
func (s *Service) requireRightReachesOrganization(ctx context.Context, c kube.Client, parent string) error {
	clusterWide := true
	for _, a := range firstAdminsAccess("") {
		allowed, err := reviewAccess(ctx, c, a)
		if err != nil {
			return err
		}
		clusterWide = clusterWide && allowed
	}
	if clusterWide {
		return nil
	}

	list, err := c.Dynamic().Resource(rbacGVR).Namespace(parent).List(ctx, metav1.ListOptions{})
	if apierrors.IsForbidden(err) {
		s.log.Info("create_organization cannot list the parent's RoleBindings; the check after Ready decides", identity.LogAttr(ctx), "parent", parent)
		return nil
	}
	if err != nil {
		return kubeErr(err, "list RoleBindings in "+parent, wayOrganizationsAdmin(parent))
	}
	review, err := c.Typed().AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if apierrors.IsForbidden(err) || apierrors.IsNotFound(err) {
		s.log.Info("create_organization cannot learn who the API server authenticates; the check after Ready decides", identity.LogAttr(ctx), "parent", parent)
		return nil
	}
	if err != nil {
		return kubeErr(err, "SelfSubjectReview", "")
	}
	user := review.Status.UserInfo

	var naming []string
	for i := range list.Items {
		var rb rbacv1.RoleBinding
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &rb); err != nil {
			return fmt.Errorf("read RoleBinding %s/%s: %w", parent, list.Items[i].GetName(), err)
		}
		if adminmirror.Mirrors(&rb) && slices.ContainsFunc(adminmirror.MirroredSubjects(rb.Subjects), namesUser(user, parent)) {
			return nil
		}
		if slices.ContainsFunc(rb.Subjects, namesUser(user, parent)) {
			naming = append(naming, notMirrored(&rb))
		}
	}
	return fmt.Errorf("%w: %s", agents.ErrForbidden, refuseUnmirrored(parent, user.Username, naming))
}

// namesUser reports whether a subject of a RoleBinding in ns names user, as the
// RBAC authorizer matches it.
func namesUser(user authenticationv1.UserInfo, ns string) func(rbacv1.Subject) bool {
	return func(sub rbacv1.Subject) bool {
		switch sub.Kind {
		case rbacv1.UserKind:
			return sub.Name == user.Username
		case rbacv1.GroupKind:
			return slices.Contains(user.Groups, sub.Name)
		case rbacv1.ServiceAccountKind:
			saNS := sub.Namespace
			if saNS == "" {
				saNS = ns
			}
			return user.Username == "system:serviceaccount:"+saNS+":"+sub.Name
		}
		return false
	}
}

// notMirrored names a binding that names the caller and says why the
// operator does not carry the caller into the organization namespace.
func notMirrored(rb *rbacv1.RoleBinding) string {
	ref := fmt.Sprintf("%s %s", rb.RoleRef.Kind, rb.RoleRef.Name)
	switch {
	case adminmirror.IsMirroredAdmin(rb) && !adminmirror.Mirrors(rb):
		return fmt.Sprintf("%s (%s; it is being deleted, so the operator will not mirror it)", rb.Name, ref)
	case adminmirror.IsMirroredAdmin(rb):
		return fmt.Sprintf("%s (%s; it names you as a ServiceAccount or a system: name, which the mirror leaves out)", rb.Name, ref)
	case rb.RoleRef.Kind == "ClusterRole" && rb.RoleRef.Name == adminmirror.ClusterAdminClusterRole:
		return fmt.Sprintf("%s (%s, without the label %s: %s)", rb.Name, ref, adminmirror.RBACOperatorLabel, adminmirror.RBACOperatorValue)
	}
	return fmt.Sprintf("%s (%s)", rb.Name, ref)
}

// refuseUnmirrored is the refusal of a caller whose right in parent will not
// reach the organization namespace: the bindings that name the caller, the
// rule and the ways out. A ServiceAccount or another system: name is never
// mirrored, so only a right cluster-wide reaches the namespace for it.
func refuseUnmirrored(parent, username string, naming []string) string {
	from := fmt.Sprintf("No RoleBinding in %s that agent-platform-operator mirrors names you", parent)
	if len(naming) > 0 {
		from = fmt.Sprintf("The RoleBindings in %s that name you are not mirrored: %s", parent, strings.Join(naming, ", "))
	}
	head := fmt.Sprintf("your right to create an organization in %s will not reach its namespace, so its first admins could not be bound; nothing was written. %s. %s (a right cluster-wide reaches it too).", parent, from, mirrorRule)
	if strings.HasPrefix(username, "system:") {
		return fmt.Sprintf("%s You (User %q) are a system: name, which is never mirrored: a cluster-admin creates the organization, or binds the right to you cluster-wide", head, username)
	}
	who := "you"
	if username != "" {
		who = fmt.Sprintf("you (User %q)", username)
	}
	return fmt.Sprintf("%s Either a RoleBinding in %s binds %s to %s, written by an admin of the Giant Swarm Organization that owns %s, or a cluster-admin creates the organization",
		head, parent, adminmirror.OrganizationsAdminClusterRole, who, parent)
}
