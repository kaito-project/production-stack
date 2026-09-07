/*
Copyright 2026 The KAITO Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package modelsapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// NamespaceHeader carries the workload namespace a request was admitted
	// through. It is stamped by the per-namespace Gateway route rendered by
	// charts/modelharness with OVERWRITE_IF_EXISTS_OR_ADD, so a client-supplied
	// value is always discarded before the request reaches this server. Host is
	// deliberately NOT used for this: it is caller-controlled and forgeable.
	NamespaceHeader = "X-Kaito-Gateway-Namespace"

	// ListPath is the exact path of the model listing endpoint; RetrievePath is
	// the prefix of the single-model endpoint. The trailing slash matters: a
	// bare "/v1/models" prefix would also swallow paths like "/v1/modelsfoo",
	// which must stay unmatched so the Gateway catch-all can answer them.
	ListPath     = "/v1/models"
	RetrievePath = "/v1/models/"

	// errorSourceHeader names the at-fault component on error responses,
	// matching the convention the Gateway EnvoyFilters already stamp.
	errorSourceHeader = "x-kaito-error-source"
	errorSourceValue  = "gateway"
)

// Handler serves the model discovery endpoints. It is safe for concurrent use.
type Handler struct {
	lister *Lister
	log    logr.Logger
}

// NewHandler builds a Handler over lister.
func NewHandler(lister *Lister, log logr.Logger) *Handler {
	return &Handler{lister: lister, log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == ListPath:
		h.serve(w, r, "")
	case strings.HasPrefix(r.URL.Path, RetrievePath):
		h.serve(w, r, strings.TrimPrefix(r.URL.Path, RetrievePath))
	default:
		writeError(w, http.StatusNotFound, "invalid_request_error", "unknown_url",
			"Unknown request URL.")
	}
}

// serve handles both endpoints; id is empty for the listing endpoint.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed",
			"Only GET is supported on this endpoint.")
		return
	}

	namespace := r.Header.Get(NamespaceHeader)
	if errMsg := validateNamespace(namespace); errMsg != "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_gateway_namespace", errMsg)
		return
	}

	managed, err := h.lister.NamespaceManaged(r.Context(), namespace)
	if err != nil {
		h.log.Error(err, "resolve workload namespace", "namespace", namespace)
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "models_api_unavailable",
			"The model registry is temporarily unavailable. Please retry.")
		return
	}
	if !managed {
		writeError(w, http.StatusNotFound, "invalid_request_error", "namespace_not_found",
			"The namespace does not exist or is not managed by production-stack.")
		return
	}

	if id == "" && strings.HasPrefix(r.URL.Path, RetrievePath) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_model_id",
			"A model id is required.")
		return
	}

	if id == "" {
		h.list(w, r, namespace)
		return
	}
	h.retrieve(w, r, namespace, id)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, namespace string) {
	models, err := h.lister.List(r.Context(), namespace)
	if err != nil {
		h.log.Error(err, "list models", "namespace", namespace)
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "models_api_unavailable",
			"The model registry is temporarily unavailable. Please retry.")
		return
	}
	writeJSON(w, http.StatusOK, ModelList{Object: objectList, Data: models})
}

func (h *Handler) retrieve(w http.ResponseWriter, r *http.Request, namespace, id string) {
	model, found, err := h.lister.Get(r.Context(), namespace, id)
	if err != nil {
		h.log.Error(err, "get model", "namespace", namespace, "model", id)
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "models_api_unavailable",
			"The model registry is temporarily unavailable. Please retry.")
		return
	}
	if !found {
		// Same body the Gateway catch-all returns for an unroutable model, so
		// "model does not exist" has one representation across the data plane.
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			"The model does not exist.")
		return
	}
	writeJSON(w, http.StatusOK, model)
}

// validateNamespace returns a client-facing message when the injected namespace
// is missing or not a legal namespace name, and the empty string when it is
// usable. A malformed value means the request did not arrive through a
// correctly configured Gateway route.
func validateNamespace(namespace string) string {
	if namespace == "" {
		return "The request did not arrive through a configured production-stack gateway."
	}
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		return "The gateway supplied an invalid workload namespace."
	}
	return ""
}

// errorBody is the OpenAI-compatible error envelope.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

func writeError(w http.ResponseWriter, status int, errType, code, message string) {
	w.Header().Set(errorSourceHeader, errorSourceValue)
	var param *string
	if code == "model_not_found" {
		p := "model"
		param = &p
	}
	writeJSON(w, status, errorBody{Error: errorDetail{
		Message: message,
		Type:    errType,
		Param:   param,
		Code:    code,
	}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
