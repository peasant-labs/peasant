package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
)

const selectionVisibilityErrorCode = "selection_visibility"

type errorEnvelope struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func writeAPIError(w http.ResponseWriter, status int, message, code string) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Error: message, Code: code})
}

func writeDiscoveryError(w http.ResponseWriter, operation string, err error) {
	message := fmt.Sprintf("%s: %v; no discovery rows were returned, so retry after resolving the reported cause", operation, err)
	code := ""
	if sessionvisibility.IsError(err) {
		code = selectionVisibilityErrorCode
		message += "; run `peasant kickstart` to repair the persisted selection, then retry"
	}
	writeAPIError(w, http.StatusInternalServerError, message, code)
}
