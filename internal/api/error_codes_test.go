package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	contract "github.com/giantswarm/agent-manager/api"
)

// Every code the server answers is one the contract's Error.code lists, so a
// client generated from it can switch on each.
func TestTheContractListsEveryErrorCode(t *testing.T) {
	var spec struct {
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
	require.NoError(t, yaml.Unmarshal(contract.OpenAPI, &spec))
	listed := spec.Components.Schemas.Error.Properties.Error.Properties.Code.Enum
	require.NotEmpty(t, listed)

	_, backendError := statusFor(assert.AnError)
	answered := []string{backendError}
	for _, c := range errorCodes {
		answered = append(answered, c.code)
	}
	assert.ElementsMatch(t, answered, listed)
}
