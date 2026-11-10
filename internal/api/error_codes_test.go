package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	contract "github.com/giantswarm/agent-manager/api"
	"github.com/giantswarm/agent-manager/internal/organizations"
)

type contractOperation struct {
	OperationID string                    `json:"operationId"`
	Responses   map[string]map[string]any `json:"responses"`
}

type contractDocument struct {
	Paths map[string]struct {
		Get    *contractOperation `json:"get"`
		Post   *contractOperation `json:"post"`
		Patch  *contractOperation `json:"patch"`
		Delete *contractOperation `json:"delete"`
	} `json:"paths"`
	Components struct {
		Schemas struct {
			Error struct {
				Properties struct {
					Error struct {
						Properties struct {
							Code struct {
								Enum []string `json:"enum"`
							} `json:"code"`
						} `json:"properties"`
					} `json:"error"`
				} `json:"properties"`
			} `json:"Error"`
		} `json:"schemas"`
	} `json:"components"`
}

func readContract(t *testing.T) contractDocument {
	t.Helper()
	var document contractDocument
	require.NoError(t, yaml.Unmarshal(contract.OpenAPI, &document))
	return document
}

// contractErrorStatuses is every operation's listed 4xx and 5xx statuses by
// operationId.
func contractErrorStatuses(t *testing.T, document contractDocument) map[string][]int {
	t.Helper()
	out := map[string][]int{}
	for _, item := range document.Paths {
		for _, operation := range []*contractOperation{item.Get, item.Post, item.Patch, item.Delete} {
			if operation == nil {
				continue
			}
			require.NotEmpty(t, operation.OperationID)
			statuses := []int{}
			for key := range operation.Responses {
				status, err := strconv.Atoi(key)
				require.NoError(t, err, "%s lists response %q", operation.OperationID, key)
				if status >= http.StatusBadRequest {
					statuses = append(statuses, status)
				}
			}
			out[operation.OperationID] = statuses
		}
	}
	return out
}

// Every code the server answers is one the contract's Error.code lists, and
// the contract lists no other, so a client generated from it can switch on
// each.
func TestTheContractListsEveryErrorCode(t *testing.T) {
	listed := readContract(t).Components.Schemas.Error.Properties.Error.Properties.Code.Enum
	require.NotEmpty(t, listed)

	_, backendError := statusFor(assert.AnError)
	answered := []string{backendError}
	for _, c := range errorCodes {
		answered = append(answered, c.code)
	}
	assert.ElementsMatch(t, answered, listed)
}

// No operation lists an error status the server never answers.
func TestTheContractListsOnlyErrorStatusesTheServerAnswers(t *testing.T) {
	backendStatus, _ := statusFor(assert.AnError)
	answered := []int{backendStatus}
	for _, c := range errorCodes {
		answered = append(answered, c.status)
	}
	for operation, statuses := range contractErrorStatuses(t, readContract(t)) {
		for _, status := range statuses {
			assert.Contains(t, answered, status, "%s lists %d", operation, status)
		}
	}
}

// Each credential route lists the status it answers without the
// organization tools and when its backend fails.
func TestTheCredentialRoutesListTheirFailureStatuses(t *testing.T) {
	listed := contractErrorStatuses(t, readContract(t))
	operation := Prefix + "/credential-operations/" + strings.Repeat("a", 48)
	routes := []struct {
		operationID, method, path string
	}{
		{"getCredentialReferences", http.MethodGet, credentialsPath + "tenant-credential-claude"},
		{"getCredentialOperation", http.MethodGet, operation},
		{"saveCredentialOperation", http.MethodPost, operation},
		{"cancelCredentialOperation", http.MethodDelete, operation},
	}
	failing := func(t *testing.T) *routeFixture {
		t.Helper()
		storeDown := func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("store down")
		}
		maps := kubefake.NewClientset()
		maps.PrependReactor("get", "configmaps", storeDown)
		f := newOrgFixtureWith(t, func(cfg *organizations.Config) {
			cfg.CredentialOperations = organizations.NewCredentialOperations(maps.CoreV1().ConfigMaps("agent-manager"))
		})
		f.dyn.PrependReactor("list", "modelconfigs", storeDown)
		return newRoute(t, f)
	}
	outcomes := []struct {
		name   string
		mux    func(*testing.T) *http.ServeMux
		status int
	}{
		{"without the organization tools", func(t *testing.T) *http.ServeMux {
			svc, _ := newService(t)
			mux := http.NewServeMux()
			NewREST(svc, nil).Register(mux)
			return mux
		}, http.StatusNotImplemented},
		{"when the backend fails", func(t *testing.T) *http.ServeMux { return failing(t).mux }, http.StatusBadGateway},
	}
	for _, outcome := range outcomes {
		for _, route := range routes {
			t.Run(outcome.name+"/"+route.operationID, func(t *testing.T) {
				f := newOrgFixture(t)
				request := httptest.NewRequestWithContext(f.ctx, route.method, route.path, strings.NewReader(`{"values":[]}`))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("If-Match", `"1"`)
				recorder := httptest.NewRecorder()
				outcome.mux(t).ServeHTTP(recorder, request)

				require.Equal(t, outcome.status, recorder.Code, recorder.Body.String())
				assert.True(t, slices.Contains(listed[route.operationID], recorder.Code), "%s answers %d, the contract lists %v", route.operationID, recorder.Code, listed[route.operationID])
			})
		}
	}
}
