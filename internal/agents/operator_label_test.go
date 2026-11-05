package agents

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// OperatorObjectLabel copies agent-platform-operator's ObjectLabel, which
// lives in its internal controller package (not importable): it must equal
// the operator's value at the version go.mod pins, so a rename there fails
// here instead of letting a delete take the operator's chart source.
func TestOperatorObjectLabelIsTheOperators(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/giantswarm/agent-platform-operator").Output()
	require.NoError(t, err, "the pinned operator module must be downloaded (go mod download)")
	src, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "internal", "controller", "organization.go"))
	require.NoError(t, err)
	m := regexp.MustCompile(`\bObjectLabel\s*=\s*"([^"]+)"`).FindSubmatch(src)
	require.NotNil(t, m, "the operator no longer declares ObjectLabel in internal/controller/organization.go: find where it went")
	require.Equal(t, string(m[1]), OperatorObjectLabel)
}
