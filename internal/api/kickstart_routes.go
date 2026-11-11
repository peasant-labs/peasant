package api

import (
	"net/http"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// registerKickstartRoutes mounts the kickstart routes on mux. The server wraps
// the mux in its origin guard, so every route that is not a read refuses a
// foreign origin before a handler runs.
func registerKickstartRoutes(mux *http.ServeMux, h *kickstartHandler) {
	mux.HandleFunc("GET "+defaults.RouteKickstartStatus.String(), h.handleStatus)
	mux.HandleFunc("POST "+defaults.RouteKickstartSessions.String(), h.handleOpen)
	mux.HandleFunc("GET "+defaults.RouteKickstartSession.String(), h.handleGet)
	mux.HandleFunc("PATCH "+defaults.RouteKickstartSession.String(), h.handlePatch)
	mux.HandleFunc("GET "+defaults.RouteKickstartTree.String(), h.handleTree)
	mux.HandleFunc("GET "+defaults.RouteKickstartPreview.String(), h.handlePreview)
	mux.HandleFunc("POST "+defaults.RouteKickstartScan.String(), h.handleScan)
}
