package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

// maxKickstartBody bounds a kickstart request body. The largest is the tree
// answer, one session id per selected session.
const maxKickstartBody = 8 << 20

// kickstartHandler answers the kickstart HTTP routes over the session store.
// Every route passes the server's origin guard before it reaches a handler.
type kickstartHandler struct {
	sessions   *kickstartSessions
	configPath string
	// commit saves a session and starts the import. Listen wires it to the
	// server's live configuration and its ingest.
	commit *kickstartCommitter
}

func (h *kickstartHandler) available(w http.ResponseWriter) bool {
	if h.sessions != nil && h.configPath != "" {
		return true
	}
	writeKickstartCode(w, schema.LocalKickstartErrorUnavailable,
		"Setup could not start because this server was started without a configuration file in internal/api.kickstartHandler. Nothing was changed. Start the dashboard with `peasant web start`, then retry.")
	return false
}

// handleStatus answers GET /api/v1/kickstart/status: the first-run redirect
// hook. configured is true once the configuration file exists. activeSessionId
// is the live full-scope session, when there is one.
func (h *kickstartHandler) handleStatus(w http.ResponseWriter, _ *http.Request) {
	if !h.available(w) {
		return
	}
	configured := true
	if _, err := os.Stat(h.configPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("kickstart: read the configuration file", "path", h.configPath, "error", err)
			writeKickstartCode(w, schema.LocalKickstartErrorStatusUnreadable,
				fmt.Sprintf("Setup status could not be read because the configuration file %s could not be inspected in internal/api.kickstartHandler.handleStatus: %v. Nothing was changed. Fix the file's permissions, then retry.", h.configPath, err))
			return
		}
		configured = false
	}
	status := schema.LocalKickstartStatus{Configured: configured, ActiveSessionID: h.sessions.activeFullSessionID()}
	writeKickstartContract(w, &status)
}

// handleOpen answers POST /api/v1/kickstart/sessions: open or join the session
// for a scope. The same scope returns the same session; the other scope's
// session is not touched.
func (h *kickstartHandler) handleOpen(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}
	var request schema.LocalKickstartOpenRequest
	if err := decodeKickstartBody(w, r, &request); err != nil {
		writeKickstartError(w, err)
		return
	}
	if err := request.Validate(); err != nil {
		writeKickstartError(w, newKickstartError(schema.LocalKickstartErrorScopeInvalid,
			"The setup session could not be opened: %v. No session was opened. Send {\"scope\": \"full\"} or {\"scope\": \"selection\"}.", err))
		return
	}
	scope, err := onboarding.NewModelScope(request.Scope.String())
	if err != nil {
		writeKickstartError(w, newKickstartError(schema.LocalKickstartErrorScopeInvalid, "%v", err))
		return
	}
	view, err := h.sessions.open(r.Context(), scope)
	if err != nil {
		writeKickstartError(w, err)
		return
	}
	writeKickstartContract(w, &view)
}

// handleGet answers GET /api/v1/kickstart/sessions/{id}: the reconnect snapshot.
func (h *kickstartHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}
	view, err := h.sessions.get(r.PathValue("id"))
	if err != nil {
		writeKickstartError(w, err)
		return
	}
	writeKickstartContract(w, &view)
}

// handlePatch answers PATCH /api/v1/kickstart/sessions/{id}: one request's
// answers. A write at an older revision is applied and reported in the body; it
// is not a separate status.
func (h *kickstartHandler) handlePatch(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}
	var request schema.LocalKickstartAnswers
	if err := decodeKickstartBody(w, r, &request); err != nil {
		writeKickstartError(w, err)
		return
	}
	if err := request.Validate(); err != nil {
		writeKickstartError(w, newKickstartError(schema.LocalKickstartErrorAnswersInvalid,
			"The answer was refused in internal/api.kickstartHandler.handlePatch: %v. Nothing was changed.", err))
		return
	}
	result, err := h.sessions.patch(r.PathValue("id"), request)
	if err != nil {
		writeKickstartError(w, err)
		return
	}
	writeKickstartContract(w, &result)
}

// handleTree answers GET /api/v1/kickstart/sessions/{id}/tree: the whole forest.
func (h *kickstartHandler) handleTree(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}
	tree, err := h.sessions.tree(r.PathValue("id"))
	if err != nil {
		writeKickstartError(w, err)
		return
	}
	writeKickstartContract(w, &tree)
}

// handlePreview answers GET /api/v1/kickstart/sessions/{id}/tree/{nodeId}/preview.
func (h *kickstartHandler) handlePreview(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}
	preview, err := h.sessions.preview(r.Context(), r.PathValue("id"), r.PathValue("nodeId"))
	if err != nil {
		writeKickstartError(w, err)
		return
	}
	writeKickstartContract(w, &preview)
}

// handleScan answers POST /api/v1/kickstart/sessions/{id}/scan: restart the
// discovery scan. The body is the scan state at accept; progress is pushed.
func (h *kickstartHandler) handleScan(w http.ResponseWriter, r *http.Request) {
	if !h.available(w) {
		return
	}
	scan, err := h.sessions.rescan(r.PathValue("id"))
	if err != nil {
		writeKickstartError(w, err)
		return
	}
	writeKickstartContract(w, &scan)
}

// decodeKickstartBody reads exactly one JSON value into a contract request type,
// which refuses a field the contract does not declare.
func decodeKickstartBody(w http.ResponseWriter, r *http.Request, into any) error {
	if r.Body == nil {
		return newKickstartError(schema.LocalKickstartErrorBodyMissing,
			"The request has no body in internal/api.decodeKickstartBody. Nothing was changed. Send one JSON object.")
	}
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, maxKickstartBody), into); err != nil {
		if errors.Is(err, io.EOF) {
			return newKickstartError(schema.LocalKickstartErrorBodyMissing,
				"The request body is empty in internal/api.decodeKickstartBody. Nothing was changed. Send one JSON object.")
		}
		return newKickstartError(schema.LocalKickstartErrorBodyInvalid,
			"The request body is not accepted in internal/api.decodeKickstartBody: %v. Nothing was changed. Send one JSON object of the declared shape.", err)
	}
	return nil
}

// writeKickstartError answers a refusal with its code, and the status the code
// answers with. An error that is not a refusal is a defect no narrower code
// names, and says so.
func writeKickstartError(w http.ResponseWriter, err error) {
	var refusal *kickstartError
	if errors.As(err, &refusal) {
		writeKickstartCode(w, refusal.code, refusal.message)
		return
	}
	slog.Error("kickstart: a request failed", "error", err)
	writeKickstartCode(w, schema.LocalKickstartErrorFailed,
		fmt.Sprintf("The setup request failed in internal/api.kickstartHandler: %v. Nothing was changed. Retry; if it repeats, report it.", err))
}

// writeKickstartCode answers every non-2xx kickstart request that is not the
// shared 403 or the commit 409: the contract's error body, with the status the
// code answers with. A message or a code the contract refuses is itself a
// defect, answered as one rather than sent.
func writeKickstartCode(w http.ResponseWriter, code schema.LocalKickstartErrorCode, message string) {
	response := schema.LocalKickstartErrorResponse{Error: message, Code: code}
	if err := response.Validate(); err != nil {
		slog.Error("kickstart: an error answer breaks the Local API contract and was replaced", "code", code, "error", err)
		response = schema.LocalKickstartErrorResponse{
			Error: fmt.Sprintf("The setup request failed in internal/api.writeKickstartCode: its error answer breaks the Local API contract (%v). Nothing was changed. Retry; if it repeats, report it.", err),
			Code:  schema.LocalKickstartErrorFailed,
		}
	}
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	w.WriteHeader(response.Code.HTTPStatus())
	_ = json.NewEncoder(w).Encode(response)
}

// writeKickstartContract answers a successful kickstart request with a contract
// value, after checking it the way a client does.
func writeKickstartContract(w http.ResponseWriter, value contractValue) {
	writeContractStatus(w, http.StatusOK, value, "Retry; if it repeats, report it.")
}
