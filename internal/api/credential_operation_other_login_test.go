package api

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/providers"
	"github.com/giantswarm/mcp-oauth/providers/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/giantswarm/agent-manager/internal/idtoken"
	"github.com/giantswarm/agent-manager/internal/organizations"
)

const ownerIssuer = "https://identity.example.com"

func TestCredentialOperationsAcrossAuthenticationPaths(t *testing.T) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))}, nil)
	require.NoError(t, err)
	token, err := jwt.Signed(signer).Claims(jwt.Claims{Issuer: ownerIssuer, Subject: "a", Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour))}).Serialize()
	require.NoError(t, err)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		for _, prepareOAuth := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/oauth=%t", method, prepareOAuth), func(t *testing.T) {
				fixture, _ := otherLoginFixture(t)
				oauthUser := owner()
				oauthUser.Issuer = "https://manager.example.com"
				oauthUser.TokenSource = providers.TokenSourceOAuth
				forwardedUser := owner()
				forwardedUser.TokenSource = providers.TokenSourceTrustedIssuer
				preparer, reader := oauthUser, forwardedUser
				if !prepareOAuth {
					preparer, reader = reader, preparer
				}
				fixture.ctx = idtoken.ContextWith(handler.ContextWithUserInfo(fixture.ctx, preparer), token)
				result := prepareModelOperation(t, fixture, nil)
				confirmation := confirmOperation(t, fixture, result.CredentialOperation)
				verified, ok := handler.UserInfoFromContext(fixture.ctx)
				require.True(t, ok)
				require.Equal(t, preparer.Issuer, verified.Issuer)
				body := ""
				if method == http.MethodPost {
					body = valuesBody(t, result, "fixture-credential")
				}
				status, answer, _ := operationHTTP(t, fixture, method, result.CredentialOperation, confirmation.Version, body, reader)
				require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, status, answer)
			})
		}
	}
}

// owner is the fixture's caller with a verified email address: the person
// who prepares the operations below.
func owner() *providers.UserInfo {
	return &providers.UserInfo{Issuer: ownerIssuer, ClientID: "platform", ID: "a", Email: "alice@example.com", EmailVerified: true}
}

// otherLogin is the owner signed in through another connector: same issuer
// and verified email address, written with other case, another subject.
func otherLogin() *providers.UserInfo {
	return &providers.UserInfo{Issuer: ownerIssuer, ClientID: "platform", ID: "a-other-connector", Email: "Alice@Example.com", EmailVerified: true}
}

func otherLoginFixture(t *testing.T) (*orgFixture, typedcore.ConfigMapInterface) {
	t.Helper()
	maps := operationMaps(t)
	fixture := newOrgFixtureWith(t, func(cfg *organizations.Config) {
		cfg.CredentialOperations = organizations.NewCredentialOperations(maps)
	})
	fixture.ctx = handler.ContextWithUserInfo(fixture.ctx, owner())
	return fixture, maps
}

// editStoredOperation changes a field of the stored operation record, as an
// older agent-manager or the passing of time left it.
func editStoredOperation(t *testing.T, maps typedcore.ConfigMapInterface, id string, edit func(map[string]any)) {
	t.Helper()
	item, err := maps.Get(t.Context(), "credential-"+id, metav1.GetOptions{})
	require.NoError(t, err)
	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(item.Data["operation"]), &record))
	edit(record)
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	item.Data["operation"] = string(raw)
	_, err = maps.Update(t.Context(), item, metav1.UpdateOptions{})
	require.NoError(t, err)
}

// requireNothingOfTheOperation fails when an answer names anything the
// operation holds.
func requireNothingOfTheOperation(t *testing.T, answer string) {
	t.Helper()
	for _, content := range []string{orgNS, "payments", "org-acme", "claude", "Anthropic", "tenant-credential", "apiKey", "portal.example.com"} {
		require.NotContains(t, answer, content)
	}
}

// The four callers of a credential operation: the owner reads it, the same
// person with another login gets an answer that names the cause and nothing
// of the operation, another person and an unverified email address get
// not_found, as before.
func TestCredentialOperationCallers(t *testing.T) {
	callers := []struct {
		name   string
		person *providers.UserInfo
		status int
		code   string
	}{
		{"the owner", nil, http.StatusOK, ""},
		{"the same person with another login", otherLogin(), http.StatusForbidden, "credential_operation_other_login"},
		{"another person", &providers.UserInfo{Issuer: ownerIssuer, ClientID: "platform", ID: "b", Email: "bob@example.com", EmailVerified: true}, http.StatusNotFound, "not_found"},
		{"an unverified email address", &providers.UserInfo{Issuer: ownerIssuer, ClientID: "platform", ID: "a-other-connector", Email: "alice@example.com"}, http.StatusNotFound, "not_found"},
		{"another issuer", &providers.UserInfo{Issuer: "https://other.example.com", ClientID: "platform", ID: "a-other-connector", Email: "alice@example.com", EmailVerified: true}, http.StatusNotFound, "not_found"},
		{"a delegated token", &providers.UserInfo{Issuer: ownerIssuer, ClientID: "platform", ID: "a-other-connector", Email: "alice@example.com", EmailVerified: true, ActorChain: []oidc.ActorClaim{{Subject: "agent"}}}, http.StatusNotFound, "not_found"},
		{"a flattened actor", &providers.UserInfo{Issuer: ownerIssuer, ClientID: "platform", ID: "a", Email: "alice@example.com", EmailVerified: true, ActorSubject: "agent"}, http.StatusNotFound, "not_found"},
	}
	for _, caller := range callers {
		t.Run(caller.name, func(t *testing.T) {
			fixture, _ := otherLoginFixture(t)
			result := prepareModelOperation(t, fixture, nil)
			confirmation := confirmOperation(t, fixture, result.CredentialOperation)

			status, answer, _ := operationHTTP(t, fixture, http.MethodGet, result.CredentialOperation, "", "", caller.person)
			require.Equal(t, caller.status, status, answer)
			if caller.status == http.StatusOK {
				return
			}
			require.Equal(t, caller.code, errorCode(t, answer))
			requireNothingOfTheOperation(t, answer)

			// The save and the cancellation answer the same, and write nothing.
			before := len(fixture.dyn.Actions())
			status, answer, _ = operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "sensitive-fixture"), caller.person)
			require.Equal(t, caller.status, status, answer)
			require.Equal(t, caller.code, errorCode(t, answer))
			requireNothingOfTheOperation(t, answer)
			require.NotContains(t, answer, "sensitive-fixture")
			status, answer, _ = operationHTTP(t, fixture, http.MethodDelete, result.CredentialOperation, confirmation.Version, "", caller.person)
			require.Equal(t, caller.status, status, answer)
			require.Equal(t, caller.code, errorCode(t, answer))
			require.Len(t, fixture.dyn.Actions(), before, "a refused caller writes nothing")

			// The owner still reads the pending operation.
			require.Equal(t, "pending", confirmOperation(t, fixture, result.CredentialOperation).State)
		})
	}
}

// An expired or cancelled operation answers as before: the owner reads its
// state, every other caller, the owner's other login too, gets not_found. So
// does an operation prepared before the email digest was recorded.
func TestCredentialOperationOtherLoginKeepsTheEarlierAnswers(t *testing.T) {
	scenarios := []struct {
		name        string
		change      func(*testing.T, *orgFixture, typedcore.ConfigMapInterface, string, string)
		ownerStatus int
	}{
		{"expired", func(t *testing.T, _ *orgFixture, maps typedcore.ConfigMapInterface, id, _ string) {
			editStoredOperation(t, maps, id, func(record map[string]any) {
				record["expiresAt"] = time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
			})
		}, http.StatusConflict},
		{"cancelled", func(t *testing.T, fixture *orgFixture, _ typedcore.ConfigMapInterface, id, version string) {
			status, answer, _ := operationHTTP(t, fixture, http.MethodDelete, id, version, "", nil)
			require.Equal(t, http.StatusNoContent, status, answer)
		}, http.StatusOK},
		{"prepared without an email digest", func(t *testing.T, _ *orgFixture, maps typedcore.ConfigMapInterface, id, _ string) {
			editStoredOperation(t, maps, id, func(record map[string]any) {
				require.NotEmpty(t, record["emailDigest"], "a verified owner's operation records the digest")
				delete(record, "emailDigest")
			})
		}, http.StatusOK},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			fixture, maps := otherLoginFixture(t)
			result := prepareModelOperation(t, fixture, nil)
			confirmation := confirmOperation(t, fixture, result.CredentialOperation)
			scenario.change(t, fixture, maps, result.CredentialOperation, confirmation.Version)

			status, answer, _ := operationHTTP(t, fixture, http.MethodGet, result.CredentialOperation, "", "", otherLogin())
			require.Equal(t, http.StatusNotFound, status, answer)
			require.Equal(t, "not_found", errorCode(t, answer))
			status, answer, _ = operationHTTP(t, fixture, http.MethodGet, result.CredentialOperation, "", "", nil)
			require.Equal(t, scenario.ownerStatus, status, answer)
		})
	}
}

// An owner whose email address was not verified at preparation records no
// digest: another login with a verified address then gets not_found.
func TestCredentialOperationOfAnUnverifiedOwnerAnswersNotFound(t *testing.T) {
	fixture, _ := otherLoginFixture(t)
	unverified := owner()
	unverified.EmailVerified = false
	fixture.ctx = handler.ContextWithUserInfo(fixture.ctx, unverified)
	result := prepareModelOperation(t, fixture, nil)

	status, answer, _ := operationHTTP(t, fixture, http.MethodGet, result.CredentialOperation, "", "", otherLogin())
	require.Equal(t, http.StatusNotFound, status, answer)
	require.Equal(t, "not_found", errorCode(t, answer))
}

// A completed operation answers the owner's other login as a pending one
// does: the answer does not tell the state.
func TestCredentialOperationOtherLoginOfACompletedOperation(t *testing.T) {
	fixture, _ := otherLoginFixture(t)
	result := prepareModelOperation(t, fixture, nil)
	confirmation := confirmOperation(t, fixture, result.CredentialOperation)
	status, answer, _ := operationHTTP(t, fixture, http.MethodPost, result.CredentialOperation, confirmation.Version, valuesBody(t, result, "sensitive-fixture"), nil)
	require.Equal(t, http.StatusOK, status, answer)
	require.Equal(t, "complete", confirmOperation(t, fixture, result.CredentialOperation).State)

	status, answer, _ = operationHTTP(t, fixture, http.MethodGet, result.CredentialOperation, "", "", otherLogin())
	require.Equal(t, http.StatusForbidden, status, answer)
	require.Equal(t, "credential_operation_other_login", errorCode(t, answer))
	requireNothingOfTheOperation(t, answer)
}
