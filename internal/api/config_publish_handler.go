package api

import (
	"encoding/json"
	"net/http"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// publishConfigResponse is the peasant-owned body of GET /api/v1/config/publish.
//
// It is deliberately NOT a schema wire type. The effective push license is
// local server state (the loaded config's Push.License), not a contract shared
// with the village, so this surface can change without a schema-module tag.
// The web Share submit step reads it to name the license a push will apply,
// and to link the privacy notice, BEFORE the user commits to publishing.
type publishConfigResponse struct {
	// License is the effective default push license — the same value
	// `peasant village push` applies when no --license flag overrides it. Empty
	// means no license is attached (all rights reserved); the web surface maps
	// empty to the "publish without a license" phrasing.
	License string `json:"license"`
}

// handlePublishConfig serves the effective push license. It is always
// available: when no config is loaded it reports an empty license rather than
// failing, because the web page must still render the privacy-notice link. The
// answer is per-process server state, so it is marked no-store like the
// capabilities advertisement.
func (s *Server) handlePublishConfig(w http.ResponseWriter, _ *http.Request) {
	var license string
	if s.cfg.Config != nil {
		license = string(s.cfg.Config.Push.License)
	}

	data, err := json.Marshal(publishConfigResponse{License: license})
	if err != nil {
		http.Error(w, `{"error":"failed to marshal publish config response"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	w.Header().Set(defaults.HeaderCacheControl, defaults.CacheControlNoStore)
	w.Write(data)
}
