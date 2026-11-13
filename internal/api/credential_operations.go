package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/giantswarm/agent-manager/internal/organizations"
)

type credentialOperationSave struct {
	Values []organizations.CredentialValue `json:"values"`
}

func (h *REST) getCredentialOperation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.orgs == nil {
		h.writeError(w, errOrganizationsUnsupported)
		return
	}
	operation, err := h.orgs.ReadCredentialOperation(r.Context(), r.PathValue("operation"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	w.Header().Set("ETag", `"`+operation.Version+`"`)
	writeJSON(w, http.StatusOK, operation)
}

func (h *REST) saveCredentialOperation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.orgs == nil {
		h.writeError(w, errOrganizationsUnsupported)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.writeError(w, errCredentialBody)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		h.writeError(w, errCredentialBody)
		return
	}
	var input credentialOperationSave
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
		h.writeError(w, errCredentialBody)
		return
	}
	version, err := operationVersion(r)
	if err != nil {
		h.writeError(w, err)
		return
	}
	result, err := h.orgs.SaveCredentialOperation(r.Context(), r.PathValue("operation"), version, input.Values)
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *REST) cancelCredentialOperation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.orgs == nil {
		h.writeError(w, errOrganizationsUnsupported)
		return
	}
	version, err := operationVersion(r)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if err := h.orgs.CancelCredentialOperation(r.Context(), r.PathValue("operation"), version); err != nil {
		h.writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// errIfMatchRequired answers a save or cancellation without If-Match: 428
// Precondition Required (RFC 6585 §3).
var errIfMatchRequired = errors.New("If-Match with the confirmation's ETag is required")

func operationVersion(request *http.Request) (string, error) {
	value := request.Header.Get("If-Match")
	if value == "" {
		return "", errIfMatchRequired
	}
	if len(value) < 3 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", errCredentialBody
	}
	return value[1 : len(value)-1], nil
}
