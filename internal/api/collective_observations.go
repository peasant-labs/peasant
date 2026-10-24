package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/collectiveobservations"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

type collectiveObservationHandler struct {
	service *collectiveobservations.Service
}

func (h *collectiveObservationHandler) register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+defaults.RouteCollectiveObservationFetch.String(), h.fetch)
	mux.HandleFunc("GET "+defaults.RouteCollectiveObservations.String(), h.list)
	mux.HandleFunc("GET "+defaults.RouteLocalContributions.String(), h.history)
	mux.HandleFunc("PUT "+defaults.RouteCollectiveObservationTracking.String(), h.track)
	mux.HandleFunc("DELETE "+defaults.RouteCollectiveObservationTracking.String(), h.untrack)
}

func (h *collectiveObservationHandler) fetch(w http.ResponseWriter, r *http.Request) {
	if _, err := h.service.SelectedCredentials(r.Context()); err != nil {
		writeObservationError(w, err)
		return
	}
	if len(r.URL.Query()) != 0 {
		writeAPIError(w, 400, "Metadata fetch accepts no query parameters; retry the explicit POST with no body.", "")
		return
	}
	if !noObservationBody(w, r) {
		return
	}
	report, err := h.service.Fetch(r.Context())
	var failed *collectiveobservations.FetchFailure
	if err != nil && !errors.As(err, &failed) {
		writeObservationError(w, err)
		return
	}
	if validation := report.Validate(); validation != nil {
		writeObservationError(w, validation)
		return
	}
	writeObservationJSON(w, schema.CollectiveFetchHTTPStatus(report), report)
}
func writeObservationJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeObservationError(w http.ResponseWriter, err error) {
	var history *store.ContributionHistoryError
	if errors.As(err, &history) {
		if history.Status == http.StatusConflict && history.Conflict != nil {
			writeObservationJSON(w, http.StatusConflict, history.Conflict)
			return
		}
		writeAPIError(w, history.Status, err.Error(), "")
		return
	}
	var reference *collectiveobservations.ReferenceError
	var status *village.StatusError
	var network net.Error
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, auth.ErrSignedOut), errors.Is(err, auth.ErrCredentialsChanged):
		code = http.StatusUnauthorized
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = http.StatusGatewayTimeout
	case errors.As(err, &reference):
		code = http.StatusBadRequest
	case errors.As(err, &status):
		code = http.StatusBadGateway
		if status.StatusCode == http.StatusUnauthorized {
			code = http.StatusUnauthorized
		}
		if status.StatusCode == http.StatusForbidden || status.StatusCode == http.StatusNotFound {
			writeAPIError(w, http.StatusNotFound, "The collective is unavailable for the selected account; no preference changed. Verify the account and exact collective UUID, then retry.", "")
			return
		}
	case errors.As(err, &network), errors.Is(err, store.ErrObservationSuperseded):
		code = http.StatusBadGateway
	case collectiveobservations.IsSourceContractError(err):
		code = http.StatusBadGateway
	}
	writeAPIError(w, code, err.Error(), "")
}
func noObservationBody(w http.ResponseWriter, r *http.Request) bool {
	data, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(data) != 0 {
		writeAPIError(w, http.StatusBadRequest, "This metadata operation has no request body; nothing changed. Retry with an empty body.", "")
		return false
	}
	return true
}
func (h *collectiveObservationHandler) list(w http.ResponseWriter, r *http.Request) {
	if _, err := h.service.SelectedCredentials(r.Context()); err != nil {
		writeObservationError(w, err)
		return
	}
	if !noObservationBody(w, r) {
		return
	}
	if len(r.URL.Query()) != 0 {
		writeAPIError(w, http.StatusBadRequest, "Saved collective listing accepts no query parameters; use the dedicated metadata fetch operation to contact Village.", "")
		return
	}
	response, err := h.service.List(r.Context())
	if err != nil {
		writeObservationError(w, err)
		return
	}
	if err := response.Validate(); err != nil {
		writeObservationError(w, err)
		return
	}
	writeObservationJSON(w, http.StatusOK, response)
}
func (h *collectiveObservationHandler) history(w http.ResponseWriter, r *http.Request) {
	if _, err := h.service.SelectedCredentials(r.Context()); err != nil {
		writeObservationError(w, err)
		return
	}
	if !noObservationBody(w, r) {
		return
	}
	query := r.URL.Query()
	for key, values := range query {
		if len(values) != 1 || (key != "kind" && key != "collectiveId" && key != "page" && key != "limit" && key != "revision") {
			writeAPIError(w, http.StatusBadRequest, "Contribution query contains an unknown or repeated parameter; use one kind/filter/page/limit/revision value.", "")
			return
		}
	}
	q := schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceRemoteAttempts, Page: 1, Limit: 50}
	if values, exists := query["kind"]; exists {
		q.Kind = ""
		for _, known := range schema.AllLocalContributionEvidenceKinds {
			if string(known) == values[0] {
				q.Kind = known
			}
		}
		if !q.Kind.IsValid() {
			writeAPIError(w, http.StatusBadRequest, "Unknown contribution kind; select remote-attempts, receipts, or operation-diagnostics.", "")
			return
		}
	}
	var err error
	if values, exists := query["page"]; exists {
		q.Page, err = strconv.Atoi(values[0])
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "Contribution page is not an integer; use page >= 1.", "")
			return
		}
	}
	if values, exists := query["limit"]; exists {
		q.Limit, err = strconv.Atoi(values[0])
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "Contribution limit is not an integer; use 1..200.", "")
			return
		}
	}
	if values, exists := query["collectiveId"]; exists {
		id := schema.VillageUUID(values[0])
		if !village.IsCollectiveID(id) {
			writeAPIError(w, http.StatusBadRequest, "Contribution collectiveId is not a canonical UUID; resolve an exact collective reference first.", "")
			return
		}
		q.CollectiveID = &id
	}
	if values, exists := query["revision"]; exists {
		revision := schema.ContributionTraversalRevision(values[0])
		q.Revision = &revision
	}
	response, err := h.service.History(r.Context(), q)
	if err != nil {
		writeObservationError(w, err)
		return
	}
	writeObservationJSON(w, http.StatusOK, response)
}
func (h *collectiveObservationHandler) trackingID(w http.ResponseWriter, r *http.Request) (schema.VillageUUID, bool) {
	if _, err := h.service.SelectedCredentials(r.Context()); err != nil {
		writeObservationError(w, err)
		return "", false
	}
	if len(r.URL.Query()) != 0 {
		writeAPIError(w, http.StatusBadRequest, "Tracking accepts a UUID path and no query parameters; nothing changed.", "")
		return "", false
	}
	if !noObservationBody(w, r) {
		return "", false
	}
	id := schema.VillageUUID(r.PathValue("collectiveId"))
	if !village.IsCollectiveID(id) {
		writeAPIError(w, http.StatusBadRequest, "Tracking path is not a canonical collective UUID; nothing changed. Resolve an exact name or URL before retrying.", "")
		return "", false
	}
	return id, true
}
func (h *collectiveObservationHandler) track(w http.ResponseWriter, r *http.Request) {
	id, ok := h.trackingID(w, r)
	if !ok {
		return
	}
	response, err := h.service.Track(r.Context(), id)
	if err != nil {
		writeObservationError(w, err)
		return
	}
	writeObservationJSON(w, http.StatusOK, response)
}
func (h *collectiveObservationHandler) untrack(w http.ResponseWriter, r *http.Request) {
	id, ok := h.trackingID(w, r)
	if !ok {
		return
	}
	response, err := h.service.Untrack(r.Context(), id)
	if err != nil {
		writeObservationError(w, err)
		return
	}
	writeObservationJSON(w, http.StatusOK, response)
}
