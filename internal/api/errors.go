package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/peasant-labs/peasant/internal/defaults"
)

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
	// Discovery no longer applies the saved selection, so this writer never
	// rewrites a failure as a selection-repair problem. The error itself carries
	// the actionable cause (an unusable stored origin names re-ingest).
	message := fmt.Sprintf("%s: %v; no discovery rows were returned, so retry after resolving the reported cause", operation, err)
	writeAPIError(w, http.StatusInternalServerError, message, "")
}
