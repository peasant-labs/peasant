package defaults

import "time"

// DefaultPort is the default port for the web dashboard server.
const DefaultPort = 8690

// Server timeouts and intervals.
const (
	ServerShutdownGrace   = 5 * time.Second
	ServerShutdownTimeout = 5 * time.Second
	ServerBroadcastTick   = 5 * time.Second
	ServerFlushDelay      = 100 * time.Millisecond
	ServerClientTimeout   = 2 * time.Second
	ServerWriteTimeout    = 10 * time.Second
)

// Server buffer sizes and limits.
const (
	ServerWriteChSize = 64
	ServerReadLimit   = 4096
)

// Route is a typed API route path.
type Route string

func (r Route) String() string { return string(r) }

const (
	RouteHealth             Route = "/api/v1/health"
	RouteShutdown           Route = "/api/v1/shutdown"
	RouteWS                 Route = "/api/v1/ws"
	RouteConfigMock         Route = "/api/v1/config/mock"
	RouteConfigCapabilities Route = "/api/v1/config/capabilities"
	RouteSessions           Route = "/api/v1/sessions"
	// RouteSessionSummaries resolves links: it returns summaries for an explicit
	// set of session identifiers named in ?ids=, applying neither origin scope
	// nor selection scope. A sibling RESOURCE rather than a path under
	// /api/v1/sessions, because /api/v1/sessions/{id} is already the single
	// session operation and a by-id collection nested there would be ambiguous
	// against it — the same reason annotation types sit beside annotations.
	RouteSessionSummaries  Route = "/api/v1/session-summaries"
	RouteAnnotations       Route = "/api/v1/annotations"
	RouteAnnotationsBatch  Route = "/api/v1/annotations/batch"
	RouteAnnotationTypes   Route = "/api/v1/annotation-types"
	RouteSessionTranscript Route = "/api/v1/sessions/{id}/transcript"
	RouteLocalFeedback     Route = "/api/v1/local/feedback"
	RouteSyncSessions      Route = "/api/v1/sync/sessions"
	RouteSyncAuth          Route = "/api/v1/sync/auth"
	RouteSyncRedactions    Route = "/api/v1/sync/redactions"
	RouteSyncPush          Route = "/api/v1/sync/push"
	RouteSyncLogin         Route = "/api/v1/sync/login"
	RouteSyncIngest        Route = "/api/v1/sync/ingest"
	RouteSyncIngestStatus  Route = "/api/v1/sync/ingest/status"

	// Map / Review surfaces. Path params use Go 1.22 ServeMux
	// {wildcard} syntax; commit/path/file/branch arrive as query params.
	RouteMapGraph        Route = "/api/v1/map/{projectHash}"
	RouteMapNode         Route = "/api/v1/map/{projectHash}/node"
	RouteMapTasks        Route = "/api/v1/map/{projectHash}/tasks"
	RouteProjectsSummary Route = "/api/v1/projects/summary"
	RouteProjectResolve  Route = "/api/v1/projects/resolve"
	RouteReview          Route = "/api/v1/review/{projectHash}"
	RouteReviewChange    Route = "/api/v1/review/{projectHash}/change"
	RouteReviewDiff      Route = "/api/v1/review/{projectHash}/diff"

	// RouteSearch is global full-text transcript search (Cmd-K). Query in ?q=,
	// optional ?limit=; no path param (search spans all projects).
	RouteSearch Route = "/api/v1/search"
)

// DevProxy is the default dev-mode proxy address for the Next.js dev server.
const DevProxy = "localhost:3000"

// WSOriginPattern is a typed WebSocket origin pattern.
type WSOriginPattern string

// WSAllowedOrigins is the default set of allowed WebSocket origins.
var WSAllowedOrigins = []WSOriginPattern{"*"}

// Health check polling parameters for readiness probes.
const (
	HealthCheckAttempts = 50
	HealthCheckInterval = 100 * time.Millisecond
)
