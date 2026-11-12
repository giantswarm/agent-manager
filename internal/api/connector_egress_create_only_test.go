package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/organizations"
)

// createOnlySubject is a set tool that takes create, with an object of it
// that exists and the arguments of a new one of the same name.
type createOnlySubject struct {
	tool, name, secret, file string
	gvr                      schema.GroupVersionResource
	existing                 func() *unstructured.Unstructured
	arguments                map[string]any
	// spec is the field a write of arguments changes, as the live object has it.
	spec func(t *testing.T, fixture *orgFixture) any
}

func createOnlySubjects() []createOnlySubject {
	return []createOnlySubject{
		{ // #nosec G101 -- Secret names, no credentials
			tool: ToolSetOrganizationConnector, name: "gh", secret: "tenant-credential-gh", file: paymentsPath + "/remotemcpserver-gh.yaml", gvr: serverGVR,
			existing:  func() *unstructured.Unstructured { return connector("gh", "tenant-credential-gh", "token") },
			arguments: map[string]any{"namespace": orgNS, "name": "gh", "url": "https://new.example.com/mcp", "credentialHeader": "Authorization"},
			spec: func(t *testing.T, fixture *orgFixture) any {
				value, _, err := unstructured.NestedString(fixture.get(t, serverGVR, orgNS, "gh").Object, "spec", "url")
				require.NoError(t, err)
				return value
			},
		},
		{ // #nosec G101 -- Secret names, no credentials
			tool: ToolSetOrganizationEgress, name: "github", secret: "tenant-credential-github", file: paymentsPath + "/egresspolicy-github.yaml", gvr: egressGVR,
			existing: func() *unstructured.Unstructured {
				return egressPolicy("github", map[string]any{"origin": "https://api.github.com"})
			},
			arguments: map[string]any{"namespace": orgNS, "name": "github", "destinations": []any{map[string]any{"origin": "https://new.example.com", "credentialHeader": "Authorization"}}},
			spec: func(t *testing.T, fixture *orgFixture) any {
				value, _, err := unstructured.NestedSlice(fixture.get(t, egressGVR, orgNS, "github").Object, "spec", "destinations")
				require.NoError(t, err)
				return value[0].(map[string]any)["origin"]
			},
		},
	}
}

func (s createOnlySubject) call(t *testing.T, fixture *orgFixture, extra map[string]any) (string, bool) {
	t.Helper()
	arguments := map[string]any{"create": true}
	for key, value := range s.arguments {
		arguments[key] = value
	}
	for key, value := range extra {
		arguments[key] = value
	}
	return fixture.call(t, s.tool, arguments)
}

func requireAlreadyExists(t *testing.T, text string, failed bool) {
	t.Helper()
	require.True(t, failed, text)
	require.True(t, strings.HasPrefix(text, "already_exists:"), text)
}

// With create, a connector or egress policy that exists live is refused with
// already_exists and left as it is; without it, the same call changes it.
func TestSetConnectorAndEgressCreateRefusesAnExistingObject(t *testing.T) {
	for _, subject := range createOnlySubjects() {
		t.Run(subject.tool, func(t *testing.T) {
			fixture := newOrgFixture(t, subject.existing())
			seedSecret(t, fixture, subject.secret, "token")
			before := subject.spec(t, fixture)

			text, failed := subject.call(t, fixture, nil)
			requireAlreadyExists(t, text, failed)
			require.Equal(t, before, subject.spec(t, fixture))
			require.Zero(t, consumerWrites(fixture))

			text, failed = subject.call(t, fixture, map[string]any{"create": false})
			require.False(t, failed, text)
			require.NotEqual(t, before, subject.spec(t, fixture))
		})
	}
}

// With create, a new connector or egress policy is created.
func TestSetConnectorAndEgressCreateCreatesANewObject(t *testing.T) {
	for _, subject := range createOnlySubjects() {
		t.Run(subject.tool, func(t *testing.T) {
			fixture := newOrgFixture(t)
			seedSecret(t, fixture, subject.secret, "token")
			text, failed := subject.call(t, fixture, nil)
			require.False(t, failed, text)
			require.Equal(t, true, decode(t, text)["created"])
		})
	}
}

// An object made by someone else between the read and the write is refused
// by the create itself, with the same code.
func TestSetConnectorAndEgressCreateRefusesAnObjectMadeAfterTheRead(t *testing.T) {
	for _, subject := range createOnlySubjects() {
		t.Run(subject.tool, func(t *testing.T) {
			fixture := newOrgFixture(t)
			seedSecret(t, fixture, subject.secret, "token")
			fixture.dyn.PrependReactor("create", subject.gvr.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
				if len(action.(k8stesting.CreateActionImpl).CreateOptions.DryRun) > 0 {
					return false, nil, nil
				}
				return true, nil, apierrors.NewAlreadyExists(subject.gvr.GroupResource(), subject.name)
			})
			text, failed := subject.call(t, fixture, nil)
			requireAlreadyExists(t, text, failed)
		})
	}
}

// In commit mode, with create, an object that git applies, or whose file is
// in git and not applied yet, is refused with already_exists and no pull
// request; with a credential to enter or without, and in a dry run.
func TestSetConnectorAndEgressCreateInCommitModeRefusesAnExistingObject(t *testing.T) {
	for _, subject := range createOnlySubjects() {
		for _, scenario := range []struct {
			name                 string
			live, secret, dryRun bool
		}{
			{"applied from git", true, true, false},
			{"applied from git, credential missing", true, false, false},
			{"in git, not applied yet", false, true, false},
			{"in git, not applied yet, credential missing", false, false, false},
			{"in git, not applied yet, credential missing, dry run", false, false, true},
		} {
			t.Run(subject.tool+"/"+scenario.name, func(t *testing.T) {
				var objects []runtime.Object
				if scenario.live {
					live := subject.existing()
					live.SetLabels(fromGitLabels("payments"))
					objects = append(objects, live)
				}
				fixture, forge := commitFixture(t, map[string][]byte{subject.file: gitManifest(t, subject.existing())}, objects...)
				if scenario.secret {
					seedSecret(t, fixture, subject.secret, "token")
				}
				text, failed := subject.call(t, fixture, map[string]any{"mode": "commit", "dryRun": scenario.dryRun})
				requireAlreadyExists(t, text, failed)
				require.Empty(t, forge.PullRequests())
			})
		}
	}
}

// A new connector or egress policy that waits for its key is refused at the
// save when an object of that name exists by then; no Secret is written.
func TestSetConnectorAndEgressCreateSaveRefusesAnObjectMadeSincePreparation(t *testing.T) {
	for _, subject := range createOnlySubjects() {
		t.Run(subject.tool, func(t *testing.T) {
			fixture := newOrgFixture(t)
			text, failed := subject.call(t, fixture, nil)
			require.False(t, failed, text)
			var result organizations.ResourceResult
			require.NoError(t, json.Unmarshal([]byte(text), &result))
			require.NotEmpty(t, result.CredentialOperation, text)
			confirmation := confirmOperation(t, fixture, result.CredentialOperation)
			_, err := fixture.dyn.Resource(subject.gvr).Namespace(orgNS).Create(t.Context(), subject.existing(), metav1.CreateOptions{})
			require.NoError(t, err)
			status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "first-value"), nil)
			require.Equal(t, http.StatusConflict, status, answer)
			require.Equal(t, "already_exists", errorCode(t, answer))
			require.NotContains(t, answer, "first-value")
			requireNoSecret(t, fixture, subject.secret)
		})
	}
}
