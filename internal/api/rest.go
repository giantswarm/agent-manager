// Package api exposes the service twice from one process: REST/JSON under
// /api/v1 for the portal backend, and MCP tools for muster. Both map onto the
// same service methods so behavior cannot drift between them.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	spec "github.com/giantswarm/agent-manager/api"
	"github.com/giantswarm/agent-manager/internal/agents"
)

// Prefix is the REST base path.
const Prefix = "/api/v1"

const maxBodyBytes = 1 << 20

// REST serves the JSON API.
type REST struct {
	svc *agents.Service
	log *slog.Logger
}

// NewREST builds the REST handler set.
func NewREST(svc *agents.Service, log *slog.Logger) *REST {
	if log == nil {
		log = slog.Default()
	}
	return &REST{svc: svc, log: log}
}

// Register mounts the routes on mux.
func (h *REST) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+Prefix+"/info", h.getInfo)
	mux.HandleFunc("GET "+Prefix+"/openapi.yaml", h.getOpenAPI)
	mux.HandleFunc("GET "+Prefix+"/agents", h.listAgents)
	mux.HandleFunc("POST "+Prefix+"/agents", h.createAgent)
	mux.HandleFunc("POST "+Prefix+"/agents/validate", h.validateAgent)
	mux.HandleFunc("GET "+Prefix+"/agents/{namespace}/{name}", h.getAgent)
	mux.HandleFunc("PATCH "+Prefix+"/agents/{namespace}/{name}", h.updateAgent)
	mux.HandleFunc("DELETE "+Prefix+"/agents/{namespace}/{name}", h.deleteAgent)
	mux.HandleFunc("GET "+Prefix+"/agents/{namespace}/{name}/status", h.getAgentStatus)
	mux.HandleFunc("GET "+Prefix+"/modelconfigs", h.listModelConfigs)
	mux.HandleFunc("GET "+Prefix+"/skills", h.listSkills)
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// validateRequest is the body of POST /agents/validate: a create spec, or an
// update when `update` is true (the spec's non-empty fields become the change).
type validateRequest struct {
	agents.Spec
	Update bool `json:"update,omitempty"`
	Force  bool `json:"force,omitempty"`
}

func (h *REST) getInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.svc.Info(r.Context()))
}

func (h *REST) getOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(spec.OpenAPI)
}

func (h *REST) listAgents(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.List(r.Context(), r.URL.Query().Get("namespace"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": list})
}

func (h *REST) getAgent(w http.ResponseWriter, r *http.Request) {
	a, err := h.svc.Get(r.Context(), r.PathValue("namespace"), r.PathValue("name"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *REST) createAgent(w http.ResponseWriter, r *http.Request) {
	var s agents.Spec
	if !h.decode(w, r, &s) {
		return
	}
	res, err := h.svc.Create(r.Context(), s)
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (h *REST) validateAgent(w http.ResponseWriter, r *http.Request) {
	var req validateRequest
	if !h.decode(w, r, &req) {
		return
	}
	var (
		res *agents.ValidateResult
		err error
	)
	if req.Update {
		upd := SpecToUpdate(req.Spec)
		upd.Force = req.Force
		res, err = h.svc.ValidateUpdate(r.Context(), upd)
	} else {
		res, err = h.svc.ValidateCreate(r.Context(), req.Spec)
	}
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *REST) updateAgent(w http.ResponseWriter, r *http.Request) {
	var upd agents.Update
	if !h.decode(w, r, &upd) {
		return
	}
	upd.Namespace = r.PathValue("namespace")
	upd.Name = r.PathValue("name")
	if strings.EqualFold(r.URL.Query().Get("force"), "true") {
		upd.Force = true
	}
	res, err := h.svc.Update(r.Context(), upd)
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *REST) deleteAgent(w http.ResponseWriter, r *http.Request) {
	force := strings.EqualFold(r.URL.Query().Get("force"), "true")
	res, err := h.svc.Delete(r.Context(), r.PathValue("namespace"), r.PathValue("name"), force)
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *REST) getAgentStatus(w http.ResponseWriter, r *http.Request) {
	st, err := h.svc.Status(r.Context(), r.PathValue("namespace"), r.PathValue("name"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *REST) listModelConfigs(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListModelConfigs(r.Context(), r.URL.Query().Get("namespace"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"modelConfigs": list})
}

func (h *REST) listSkills(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res, err := h.svc.ListSkills(r.Context(), q.Get("repository"), q.Get("ref"), strings.EqualFold(q.Get("refresh"), "true"))
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// SpecToUpdate turns a full spec into a partial update: every non-empty field
// becomes a change, empty ones stay untouched. Used by validate (update mode)
// and by the MCP update tool, whose arguments are flat.
func SpecToUpdate(s agents.Spec) agents.Update {
	upd := agents.Update{Namespace: s.Namespace, Name: s.Name}
	str := func(v string) *string {
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return &v
	}
	upd.DisplayName = str(s.DisplayName)
	upd.Description = str(s.Description)
	upd.SystemMessage = str(s.SystemMessage)
	upd.ModelConfig = str(s.ModelConfig)
	upd.IconURL = str(s.IconURL)
	upd.Runtime = str(s.Runtime)
	if s.Skills != nil {
		upd.Skills = s.Skills
	}
	if s.ToolNames != nil {
		names := s.ToolNames
		upd.ToolNames = &names
	}
	if s.Labels != nil {
		labels := s.Labels
		upd.Labels = &labels
	}
	if s.Annotations != nil {
		annotations := s.Annotations
		upd.Annotations = &annotations
	}
	return upd
}

func (h *REST) decode(w http.ResponseWriter, r *http.Request, into any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		h.writeError(w, fmt.Errorf("%w: read body: %v", agents.ErrInvalid, err))
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		h.writeError(w, fmt.Errorf("%w: a JSON body is required", agents.ErrInvalid))
		return false
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		h.writeError(w, fmt.Errorf("%w: invalid JSON: %v", agents.ErrInvalid, err))
		return false
	}
	return true
}

func (h *REST) writeError(w http.ResponseWriter, err error) {
	status, code := statusFor(err)
	if status >= http.StatusInternalServerError {
		h.log.Error("request failed", "status", status, "error", err)
	}
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: err.Error()}})
}

// statusFor maps domain errors to HTTP status and a stable code clients can
// switch on.
func statusFor(err error) (int, string) {
	switch {
	case errors.Is(err, agents.ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, agents.ErrInvalid):
		return http.StatusBadRequest, "invalid_request"
	case errors.Is(err, agents.ErrConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(err, agents.ErrForbidden):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, agents.ErrUnsupported):
		return http.StatusNotImplemented, "unsupported"
	default:
		return http.StatusBadGateway, "backend_error"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
