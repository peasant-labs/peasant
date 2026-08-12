package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/annotations"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	schema "github.com/peasant-labs/schema"
)

// Package-local response bodies and fallback HTML.
const (
	responseHealthOK     = `{"status":"ok"}`
	responseShuttingDown = `{"status":"shutting_down"}`
	fallbackHTML         = `<html><body><h1>Peasant Web Dashboard</h1><p>Frontend not built yet. Run <code>make web</code> first.</p></body></html>`
)

// MockConfigResponse is the JSON response for GET /api/v1/config/mock.
// Canonical definition is in the github.com/peasant-labs/schema module; this
// alias maintains backward compatibility.
type MockConfigResponse = schema.MockConfigResponse

// ServerConfig holds the configuration for the web dashboard server.
type ServerConfig struct {
	Port         int
	Provider     DataProvider
	Hub          *Hub
	DevMode      bool
	DevProxyAddr string // e.g. "localhost:3000"
	WebAssets    fs.FS  // embedded SPA assets (nil in dev mode)
	MockConfig   *MockConfigResponse
	// Store is the SQLite analytics store, used to wire the annotation REST handlers.
	// Nil disables annotation endpoints (returns 503).
	Store *store.Store
	// Config is the loaded application config. Used by the sync handler for
	// redaction settings and push configuration. Nil disables sync endpoints.
	Config *config.Config
}

// Server is the HTTP server for the web dashboard.
type Server struct {
	cfg    ServerConfig
	server *http.Server
	hub    *Hub
	ln     net.Listener
}

// Addr returns the listener's address after ListenAndServe has bound.
// Returns nil if the server has not started listening.
func (s *Server) Addr() net.Addr {
	if s.ln != nil {
		return s.ln.Addr()
	}
	return nil
}

// NewServer creates a Server with the given configuration.
func NewServer(cfg ServerConfig) *Server {
	return &Server{
		cfg: cfg,
		hub: cfg.Hub,
	}
}

// Listen binds the server to the configured port and sets up routes.
// After Listen returns, Addr() returns the bound address.
// Call Serve to start accepting connections.
func (s *Server) Listen(ctx context.Context) error {
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("GET "+defaults.RouteHealth.String(), s.handleHealth)
	mux.HandleFunc("POST "+defaults.RouteShutdown.String(), s.handleShutdown(ctx))
	mux.HandleFunc(defaults.RouteWS.String(), s.handleWebSocket)
	mux.HandleFunc("GET "+defaults.RouteConfigMock.String(), s.handleMockConfig)
	mux.HandleFunc("GET "+defaults.RouteSessions.String(), s.handleSessions)
	mux.HandleFunc("GET /api/v1/web/discovery", s.handleWebDiscovery)
	mux.HandleFunc("GET "+defaults.RouteSessionTranscript.String(), s.handleSessionTranscript)
	mux.HandleFunc("GET /api/v1/annotations/review-sessions", s.handleReviewSessions)
	mux.HandleFunc("POST "+defaults.RouteLocalFeedback.String(), s.handleLocalFeedback)

	// Map / Review routes plus the home-picker summary
	mux.HandleFunc("GET "+defaults.RouteProjectsSummary.String(), s.handleProjectSummaries)
	mux.HandleFunc("GET "+defaults.RouteProjectResolve.String(), s.handleProjectResolve)
	mux.HandleFunc("GET "+defaults.RouteMapGraph.String(), s.handleMapGraph)
	mux.HandleFunc("GET "+defaults.RouteMapNode.String(), s.handleMapNode)
	mux.HandleFunc("GET "+defaults.RouteMapTasks.String(), s.handleMapTasks)
	mux.HandleFunc("GET "+defaults.RouteReview.String(), s.handleReviewChanges)
	mux.HandleFunc("GET "+defaults.RouteReviewChange.String(), s.handleReviewChange)
	mux.HandleFunc("GET "+defaults.RouteReviewDiff.String(), s.handleReviewDiff)
	mux.HandleFunc("GET "+defaults.RouteSearch.String(), s.handleSearch)

	// Annotation REST routes
	ah := &annotationHandler{
		annotationStore: s.cfg.Store,
		hub:             s.hub,
	}
	if s.cfg.Store != nil {
		ah.typeReader = annotations.NewTypeReader(s.cfg.Store)
	}
	mux.HandleFunc("GET "+defaults.RouteAnnotations.String(), ah.handleGetAnnotations)
	mux.HandleFunc("GET "+defaults.RouteAnnotationTypes.String(), ah.handleGetAnnotationTypes)
	mux.HandleFunc("POST "+defaults.RouteAnnotations.String(), ah.handleCreateAnnotation)
	mux.HandleFunc("POST "+defaults.RouteAnnotationsBatch.String(), ah.handleBatchCreateAnnotations)

	// Sync/push routes
	sh := &syncHandler{
		store:  s.cfg.Store,
		config: s.cfg.Config,
	}
	mux.HandleFunc("GET "+defaults.RouteSyncSessions.String(), sh.handleSyncSessions)
	mux.HandleFunc("GET "+defaults.RouteSyncAuth.String(), sh.handleSyncAuth)
	mux.HandleFunc("GET "+defaults.RouteSyncRedactions.String(), sh.handleSyncRedactions)
	mux.HandleFunc("POST "+defaults.RouteSyncPush.String(), sh.handleSyncPush)
	mux.HandleFunc("POST "+defaults.RouteSyncLogin.String(), sh.handleSyncLogin)
	mux.HandleFunc("POST "+defaults.RouteSyncIngest.String(), sh.handleSyncIngest)
	mux.HandleFunc("GET "+defaults.RouteSyncIngestStatus.String(), sh.handleSyncIngestStatus)

	// Static assets or dev proxy
	if s.cfg.DevMode && s.cfg.DevProxyAddr != "" {
		proxyURL, err := url.Parse("http://" + s.cfg.DevProxyAddr)
		if err != nil {
			return fmt.Errorf("invalid dev proxy address: %w", err)
		}
		proxy := httputil.NewSingleHostReverseProxy(proxyURL)
		mux.Handle("/", proxy)
	} else if s.cfg.WebAssets != nil {
		mux.Handle("/", &spaHandler{fs: http.FS(s.cfg.WebAssets)})
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(defaults.HeaderContentType, defaults.ContentHTML.String())
			fmt.Fprint(w, fallbackHTML)
		})
	}

	addr := fmt.Sprintf(":%d", s.cfg.Port)
	s.server = &http.Server{
		Addr:    addr,
		Handler: requestLogger(mux),
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	s.ln = ln

	return nil
}

// Serve starts accepting connections and blocks until shutdown.
// Listen must be called first.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		return fmt.Errorf("server not listening; call Listen first")
	}

	// Start hub in background
	if s.hub != nil {
		go s.hub.Run(ctx)
	}

	port := s.ln.Addr().(*net.TCPAddr).Port
	log.Printf("Peasant web dashboard listening on http://localhost:%d", port)

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.server.Serve(s.ln)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), defaults.ServerShutdownGrace)
		defer cancel()
		return s.server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// ListenAndServe binds and serves in one call. Blocks until shutdown.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if err := s.Listen(ctx); err != nil {
		return err
	}
	return s.Serve(ctx)
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	fmt.Fprint(w, responseHealthOK)
}

func (s *Server) handleMockConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	if s.cfg.MockConfig == nil {
		http.Error(w, `{"error":"mock config not available"}`, http.StatusServiceUnavailable)
		return
	}

	data, err := json.Marshal(s.cfg.MockConfig)
	if err != nil {
		http.Error(w, `{"error":"failed to marshal config"}`, http.StatusInternalServerError)
		return
	}
	w.Write(data)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	if s.cfg.Provider == nil {
		http.Error(w, `{"error":"data provider not available"}`, http.StatusServiceUnavailable)
		return
	}

	summaries, err := s.cfg.Provider.SessionSummaries(r.Context())
	if err != nil {
		writeDiscoveryError(w, "failed to fetch sessions", err)
		return
	}

	// Wrap in the same envelope shape as the WebSocket ChannelSessions payload
	// so REST and WS consumers share a single response contract.
	type sessionsEnvelope struct {
		Sessions any `json:"sessions"`
	}
	data, err := json.Marshal(sessionsEnvelope{Sessions: summaries})
	if err != nil {
		http.Error(w, `{"error":"failed to marshal sessions"}`, http.StatusInternalServerError)
		return
	}
	w.Write(data)
}

func (s *Server) handleSessionTranscript(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if sessionID == "" {
		http.Error(w, `{"error":"missing session ID"}`, http.StatusBadRequest)
		return
	}

	if s.cfg.Store == nil {
		http.Error(w, `{"error":"store not available"}`, http.StatusServiceUnavailable)
		return
	}

	sid, sidErr := ingest.NewSessionID(sessionID)
	if sidErr != nil {
		http.Error(w, `{"error":"invalid session ID"}`, http.StatusBadRequest)
		return
	}

	hostSlug, parentID, err := s.cfg.Store.LookupSessionLocation(r.Context(), sid)
	if err != nil || hostSlug == "" {
		http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
		return
	}

	// Construct transcript path: {dataDir}/peasant-sync/{hostSlug}/{sessionId}/{sessionId}--transcript.{ext}
	// Subagent sessions: {dataDir}/peasant-sync/{hostSlug}/{parentId}/subagents/{sessionId}/{sessionId}--transcript.{ext}
	dataDir := string(defaults.Data.DataDirPath)
	var sessionDir string
	if parentID != "" {
		sessionDir = filepath.Join(dataDir, "peasant-sync", hostSlug, parentID, "subagents", sessionID)
	} else {
		sessionDir = filepath.Join(dataDir, "peasant-sync", hostSlug, sessionID)
	}

	// Try JSONL first, then JSON
	var transcriptPath string
	for _, ext := range []string{string(ingest.SourceFormatJSONL), string(ingest.SourceFormatJSON)} {
		candidate := filepath.Join(sessionDir, sessionID+defaults.TranscriptPrefix+ext)
		if _, statErr := os.Stat(candidate); statErr == nil {
			transcriptPath = candidate
			break
		}
	}

	if transcriptPath == "" {
		http.Error(w, `{"error":"transcript file not found"}`, http.StatusNotFound)
		return
	}

	// Serve as download with original filename
	filename := filepath.Base(transcriptPath)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	http.ServeFile(w, r, transcriptPath)
}

func (s *Server) handleReviewSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())

	if s.cfg.Store == nil {
		http.Error(w, `{"error":"store not available"}`, http.StatusServiceUnavailable)
		return
	}

	pool := s.cfg.Store.Pool()
	conn, err := pool.Take(r.Context())
	if err != nil {
		http.Error(w, `{"error":"db connection failed"}`, http.StatusInternalServerError)
		return
	}
	defer pool.Put(conn)

	// Query sessions that have agent (llm-judge) friction annotations,
	// with counts of agent and human episodes.
	type reviewRow struct {
		ID            string `json:"id"`
		TurnCount     int    `json:"turnCount"`
		Project       string `json:"project"`
		AgentEpisodes int    `json:"agentEpisodes"`
		HumanEpisodes int    `json:"humanEpisodes"`
		Status        string `json:"status"`
	}

	query := `SELECT
		COALESCE(ats.session_id, ate.session_id) as sid,
		COALESCE(m.turn_count, 0),
		COALESCE(p.canonical_cwd, p.project_hash, ''),
		SUM(CASE WHEN ak.name = 'agent' THEN 1 ELSE 0 END) as agent_count,
		SUM(CASE WHEN ak.name = 'human' THEN 1 ELSE 0 END) as human_count
	FROM annotations a
	JOIN annotation_types t ON t.id = a.annotation_type_id
	JOIN annotators ann ON ann.id = a.annotator_id
	JOIN annotator_kinds ak ON ak.id = ann.kind_id
	LEFT JOIN annotation_target_sessions ats ON ats.annotation_id = a.id
	LEFT JOIN annotation_target_entries ate ON ate.annotation_id = a.id
	LEFT JOIN sessions s ON s.session_id = COALESCE(ats.session_id, ate.session_id)
	LEFT JOIN session_metrics m ON m.session_id = s.session_id
	LEFT JOIN projects p ON s.project_hash = p.project_hash
	WHERE t.type_id = 'research.friction_episode'
	AND a.superseded_by IS NULL
	GROUP BY sid
	HAVING agent_count > 0
	ORDER BY sid`

	var rows []reviewRow
	stmt, _, stmtErr := conn.PrepareTransient(query)
	if stmtErr != nil {
		http.Error(w, fmt.Sprintf(`{"error":"query failed: %s"}`, stmtErr), http.StatusInternalServerError)
		return
	}
	defer stmt.Finalize()

	for {
		hasRow, stepErr := stmt.Step()
		if stepErr != nil {
			break
		}
		if !hasRow {
			break
		}
		project := stmt.ColumnText(2)
		if idx := strings.LastIndex(project, "/"); idx >= 0 {
			project = project[idx+1:]
		}
		status := "pending"
		if stmt.ColumnInt(4) > 0 {
			status = "done"
		}
		rows = append(rows, reviewRow{
			ID:            stmt.ColumnText(0),
			TurnCount:     stmt.ColumnInt(1),
			Project:       project,
			AgentEpisodes: stmt.ColumnInt(3),
			HumanEpisodes: stmt.ColumnInt(4),
			Status:        status,
		})
	}

	data, _ := json.Marshal(map[string]any{"sessions": rows})
	w.Write(data)
}

func (s *Server) handleShutdown(_ context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Only allow shutdown from localhost
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		isLocal := false
		for _, addr := range defaults.LocalhostAddrs {
			if host == addr {
				isLocal = true
				break
			}
		}
		if !isLocal {
			http.Error(w, "forbidden: shutdown only from localhost", http.StatusForbidden)
			return
		}

		w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
		fmt.Fprint(w, responseShuttingDown)
		time.Sleep(defaults.ServerFlushDelay)

		// Trigger shutdown asynchronously
		go func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), defaults.ServerShutdownTimeout)
			defer cancel()
			if s.server != nil {
				if err := s.server.Shutdown(shutdownCtx); err != nil {
					log.Printf("server shutdown error: %v", err)
				}
			}
		}()
	}
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.hub == nil {
		http.Error(w, "websocket hub not initialized", http.StatusInternalServerError)
		return
	}
	s.hub.HandleUpgrade(w, r)
}

// spaHandler serves embedded SPA assets with fallback to index.html for client-side routing.
type spaHandler struct {
	fs http.FileSystem
}

type spaRoutePrefix string

const (
	spaProjectsRoute spaRoutePrefix = "/projects/"
	spaMapRoute      spaRoutePrefix = "/map/"
	spaReviewRoute   spaRoutePrefix = "/review/"
)

var spaDynamicRoutePrefixes = [...]spaRoutePrefix{
	spaProjectsRoute,
	spaMapRoute,
	spaReviewRoute,
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Normalize: strip trailing slash for filesystem lookup (http.FS rejects
	// trailing slashes). Keep the original for http.FileServer delegation.
	cleanPath := strings.TrimRight(path, "/")
	if cleanPath == "" {
		cleanPath = "/"
	}

	// Try to open the requested path as a static asset.
	f, err := h.fs.Open(cleanPath)
	if err == nil {
		stat, statErr := f.Stat()
		f.Close()
		if statErr == nil && !stat.IsDir() {
			// Exact static file match — delegate to http.FileServer.
			http.FileServer(h.fs).ServeHTTP(w, r)
			return
		}
		if statErr == nil && stat.IsDir() {
			// It's a directory — check if it has its own index.html.
			indexPath := cleanPath + defaults.SPAFallbackFile
			if idx, idxErr := h.fs.Open(indexPath); idxErr == nil {
				idx.Close()
				// Serve the directory's index.html directly to avoid
				// http.FileServer's /index.html → ./ redirect.
				h.serveFile(w, r, indexPath)
				return
			}
		}
	}

	// No exact match. Apply fallback logic based on file extension:
	//
	// - .txt (RSC flight data): walk up to find nearest parent's index.txt.
	//   Next.js requests /projects/foo/index.txt for client-side navigation
	//   to a catch-all route, but only /projects/index.txt was statically
	//   generated. It also appends .txt directly to dotted dynamic segments,
	//   such as timestamp-based Strike session IDs. Serving the catch-all's
	//   .txt enables client-side navigation for both forms.
	//
	// - No extension or .html: SPA fallback to nearest index.html.
	//
	// - Everything else (.js, .css, .json, etc.): 404.
	ext := pathExt(cleanPath)
	if ext == ".txt" && isSPADynamicRoute(cleanPath) {
		fallback := h.nearestFile(strings.TrimSuffix(path, ".txt"), "/index.txt")
		h.serveStaticFile(w, r, fallback)
		return
	}
	if ext == ".txt" && strings.HasSuffix(cleanPath, "/index.txt") {
		fallback := h.nearestFile(path, "/index.txt")
		h.serveStaticFile(w, r, fallback)
		return
	}
	// Dynamic route segments are user/project/session identities, not assets.
	// A period in one of those identities must not turn the route into a file
	// request; static assets live outside these route prefixes.
	if isSPADynamicRoute(cleanPath) {
		fallback := h.nearestFile(path, defaults.SPAFallbackFile)
		h.serveFile(w, r, fallback)
		return
	}
	if ext != "" && ext != ".html" {
		http.NotFound(w, r)
		return
	}
	fallback := h.nearestFile(path, defaults.SPAFallbackFile)
	h.serveFile(w, r, fallback)
}

func isSPADynamicRoute(path string) bool {
	for _, prefix := range spaDynamicRoutePrefixes {
		if strings.HasPrefix(path, string(prefix)) {
			return true
		}
	}
	return false
}

// webDashboardNotBundledHTML is served by the SPA fallback when the embedded
// web/out contains no index.html — i.e. a build that embedded only the tracked
// `web/out/.gitkeep` placeholder (`go install …@tag`, `nix build`, or a bare
// `go build` that never ran `make web`/`make web-stub`). The release binaries
// embed the real dashboard and never reach this branch.
//
// Keep this text in sync with the Makefile `web-stub` target's placeholder —
// the two live at a Go↔shell boundary and cannot share a single literal.
const webDashboardNotBundledHTML = "<!doctype html><meta charset=\"utf-8\"><title>peasant</title>" +
	"<p>The peasant web dashboard is not bundled in this build. " +
	"Run a full `make build` (or use the release binaries) for the dashboard.</p>\n"

// serveFile opens the given path from the embedded FS and writes it as HTML.
// Unlike http.FileServer, it does not issue a 301 redirect for /index.html.
func (h *spaHandler) serveFile(w http.ResponseWriter, _ *http.Request, filePath string) {
	f, err := h.fs.Open(filePath)
	if err != nil {
		// No index.html in the embedded FS — the dashboard isn't bundled (only
		// the `.gitkeep` placeholder was embedded). Serve a built-in notice with
		// 200 instead of a bare 404, so the SPA root is actionable. Real-asset
		// 404s are unaffected: the caller only routes no-extension/.html SPA
		// paths here (other extensions 404 before serveFile).
		w.Header().Set(defaults.HeaderContentType, defaults.ContentHTML.String())
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, webDashboardNotBundledHTML)
		return
	}
	defer f.Close()

	w.Header().Set(defaults.HeaderContentType, defaults.ContentHTML.String())
	io.Copy(w, f)
}

// serveStaticFile opens the given path and serves it with auto-detected content type.
// Used for non-HTML fallbacks (e.g. RSC .txt flight data).
func (h *spaHandler) serveStaticFile(w http.ResponseWriter, _ *http.Request, filePath string) {
	f, err := h.fs.Open(filePath)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	defer f.Close()

	ct := "application/octet-stream"
	switch pathExt(filePath) {
	case ".txt":
		ct = "text/plain; charset=utf-8"
	case ".json":
		ct = defaults.ContentJSON.String()
	}
	w.Header().Set(defaults.HeaderContentType, ct)
	io.Copy(w, f)
}

// nearestFile walks up from path to find the nearest directory containing
// the given filename suffix. For example, with suffix="/index.html" and
// path="/projects/foo/bar", it checks /projects/foo/bar/index.html, then
// /projects/foo/index.html, /projects/index.html, and /index.html.
func (h *spaHandler) nearestFile(reqPath string, suffix string) string {
	p := strings.TrimRight(reqPath, "/")
	for p != "" {
		candidate := p + suffix
		if f, err := h.fs.Open(candidate); err == nil {
			f.Close()
			return candidate
		}
		slash := strings.LastIndex(p, "/")
		if slash < 0 {
			break
		}
		p = p[:slash]
	}
	return suffix
}

// pathExt returns the file extension of the last path segment (e.g. ".js",
// ".txt"). Returns "" for paths without an extension (directory-like routes).
func pathExt(p string) string {
	return path.Ext(p)
}

// requestLogger wraps an http.Handler with slog-based request logging.
// Logs method, path, status, and duration for every request.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"dur", time.Since(start).Round(time.Millisecond),
		)
	})
}

// statusRecorder captures the HTTP status code written by the handler.
// It implements http.Hijacker so WebSocket upgrades work through the wrapper.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := r.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not implement http.Hijacker")
}
