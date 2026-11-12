package api

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/organizations"
)

// A caller who may not list the RoleBindings of an organization namespace
// is told what reading the members needs: the list right on RoleBindings
// there, which every member role holds (from agent-platform chart 4.179.0
// on), not agent-platform:admin alone. Changing the members still needs
// agent-platform:admin.
func TestMemberReadRefusalNamesEveryMemberRole(t *testing.T) {
	f := newOrgFixture(t)
	f.dyn.PrependReactor("list", "rolebindings", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("RBAC: access denied"))
	})

	text, isErr := f.call(t, ToolListOrganizationMembers, map[string]any{"namespace": orgNS})
	require.True(t, isErr, text)
	assert.Contains(t, text, "forbidden:")
	assert.Contains(t, text, "list on RoleBindings")
	for _, role := range []string{organizations.RoleUser, organizations.RoleEditor, organizations.RoleAdmin} {
		assert.Contains(t, text, role, "every member role reads the members")
	}
	assert.NotContains(t, text, "this needs "+organizations.RoleAdmin, "reading the members does not need admin")

	for tool, arguments := range map[string]map[string]any{
		ToolAddOrganizationMember:    {"namespace": orgNS, "role": "user", "group": "payments-team"},
		ToolRemoveOrganizationMember: {"namespace": orgNS, "group": "payments-team"},
	} {
		text, isErr = f.call(t, tool, arguments)
		require.True(t, isErr, text)
		assert.Contains(t, text, "this needs "+organizations.RoleAdmin, "%s changes the members: admin", tool)
	}
}
