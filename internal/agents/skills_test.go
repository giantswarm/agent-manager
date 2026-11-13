package agents

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/agent-manager/internal/skillmanager"
)

func TestSkillManagerGrantRequiredNamesItsApp(t *testing.T) {
	refusal := fmt.Errorf("resolve: %w", &skillmanager.GrantRequiredError{App: "skill-manager-app"})
	for name, err := range map[string]error{
		"source":    sourceError("skills[0]", refusal),
		"catalogue": skillManagerError(refusal),
	} {
		t.Run(name, func(t *testing.T) {
			grant, ok := errors.AsType[*GrantRequiredError](err)
			require.True(t, ok, "%v", err)
			require.Equal(t, "skill-manager-app", grant.App)
			require.Contains(t, grant.Reason, "core_auth_login app=skill-manager-app")
			require.NotErrorIs(t, err, ErrUnauthenticated)
		})
	}
}
