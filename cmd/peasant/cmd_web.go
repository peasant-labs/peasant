package main

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	peasant "github.com/peasant-labs/peasant"
	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/browser"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/mock"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/spf13/cobra"
)

// BuildWebCommand constructs the fully wired web cobra command tree.
func BuildWebCommand() *cobra.Command {
	webCmd := &cobra.Command{
		Use:   "web",
		Short: "Launch the web dashboard",
		// No RunE — bare `peasant web` prints help via Cobra default
	}

	var (
		webPort         int
		webDev          bool
		webFg           bool
		webNoBrowser    bool
		webVerbose      bool
		webExperimental bool
		mockDataStore   string
	)

	webStartCmd := &cobra.Command{
		Use:   "start",
		Short: "Start the web dashboard server",
		RunE: func(cmd *cobra.Command, args []string) error {
			if webVerbose {
				configureVerboseLogging()
			}
			if webFg || webDev {
				cfgPath := resolveConfigPath(cmd)
				return runWebForeground(cmd, cfgPath, webPort, webDev, mockDataStore, webExperimental)
			}
			cfgPath := resolveConfigPath(cmd)
			return runWebBackground(cfgPath, webPort, webNoBrowser, webVerbose, mockDataStore, webExperimental)
		},
	}
	webStartCmd.Flags().IntVar(&webPort, "port", defaults.DefaultPort, "Port to listen on")
	webStartCmd.Flags().BoolVar(&webDev, "dev", false, "Dev mode: proxy to Next.js dev server (implies --foreground)")
	webStartCmd.Flags().BoolVar(&webFg, "foreground", false, "Run in foreground (no background fork)")
	webStartCmd.Flags().BoolVar(&webNoBrowser, "no-browser", false, "Don't auto-open browser")
	webStartCmd.Flags().BoolVar(&webVerbose, "verbose", false, "Enable verbose logging (debug level)")
	webStartCmd.Flags().BoolVar(&webExperimental, "experimental", false, "Advertise experimental web navigation (currently: code map discoverability); direct routes stay available either way")
	webStartCmd.Flags().StringVar(&mockDataStore, "mock-data-store", "", "Use mock data store (comma-separated: web,tui,api or dashboard,sessions,trends,metrics,qualitySessions)")

	webStopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the web dashboard server",
		RunE: func(cmd *cobra.Command, args []string) error {
			return stopWeb(webPort)
		},
	}
	webStopCmd.Flags().IntVar(&webPort, "port", defaults.DefaultPort, "Port the server is listening on")

	webCmd.AddCommand(webStartCmd, webStopCmd)

	return webCmd
}

func toMockConfigResponse(cfg *config.MockConfig) *api.MockConfigResponse {
	if cfg == nil {
		return nil
	}
	convertSections := func(sections []defaults.MockSection) []string {
		result := make([]string, len(sections))
		for i, s := range sections {
			result[i] = s.String()
		}
		return result
	}
	return &api.MockConfigResponse{
		Enabled: bool(cfg.Enabled),
		Web:     convertSections(cfg.Web),
		TUI:     convertSections(cfg.TUI),
		API:     convertSections(cfg.API),
	}
}

// runWebForeground runs the server in the current process.
func runWebForeground(cmd *cobra.Command, cfgPath string, port int, devMode bool, mockDataStore string, experimental bool) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Load config
	cfg, err := config.Load(cfgPath, &ingest.OSFileSystem{}, &ingest.ExecGitResolver{})
	if err != nil {
		return fmt.Errorf("web startup: load config %q: %w; the server stopped before exposing discovery with default selection, so fix config.yaml or run `peasant kickstart` and retry", cfgPath, err)
	}
	visibility, err := sessionvisibility.New(cfg.Selection)
	if err != nil {
		return fmt.Errorf("web startup: selection policy: %w", err)
	}

	// Apply CLI overrides for mock data store
	if mockDataStore != "" {
		opts, err := ParseMockDataStore(mockDataStore)
		if err != nil {
			return fmt.Errorf("invalid --mock-data-store: %w", err)
		}
		cfg.ApplyMockOverrides(opts, defaults.MockComponents.Web)
	}

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, defaults.SignalBufferSize)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			log.Println("Shutting down...")
			cancel()
		case <-ctx.Done():
		}
	}()

	// Open real analytics store
	var realProvider api.DataProvider
	var dbCloser func() error
	var analyticsStore *store.Store
	dataDir := string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd)))
	dbPath := string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd)))
	if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot create data directory %s: %v\n", dataDir, err)
	} else if db, err := store.Open(dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot open analytics store %s: %v\n", dbPath, err)
	} else {
		dbCloser = db.Close
		analyticsStore = db
		realProvider = api.NewStoreDataProvider(db, visibility)
	}
	if dbCloser != nil {
		defer dbCloser()
	}

	// Create progressive provider
	provider := api.NewProgressiveProvider(cfg, defaults.MockComponents.Web, mock.NewProvider(), realProvider)

	hub := api.NewHub(provider)

	devProxy := ""
	if devMode {
		devProxy = defaults.DevProxy
	}

	// Sub-directory the embedded FS to strip the "web/out" prefix
	var webFS fs.FS
	if !devMode {
		var err error
		webFS, err = fs.Sub(peasant.WebAssets, defaults.WebAssetsSubdir)
		if err != nil {
			return fmt.Errorf("embedded web assets: %w", err)
		}
	}

	srv := api.NewServer(api.ServerConfig{
		Port:         port,
		Provider:     provider,
		Hub:          hub,
		DevMode:      devMode,
		DevProxyAddr: devProxy,
		WebAssets:    webFS,
		MockConfig:   toMockConfigResponse(&cfg.Sources.Mock),
		Experimental: experimental,
		Store:        analyticsStore,
		Config:       cfg,
	})

	return srv.ListenAndServe(ctx)
}

// runWebBackground forks the server as a background process.
func runWebBackground(cfgPath string, port int, noBrowser bool, verbose bool, mockDataStore string, experimental bool) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find executable: %w", err)
	}

	args := []string{"web", "start", "--foreground", "--port", strconv.Itoa(port), "--config", cfgPath}
	if mockDataStore != "" {
		args = append(args, "--mock-data-store", mockDataStore)
	}
	if verbose {
		args = append(args, "--verbose")
	}
	if experimental {
		args = append(args, "--experimental")
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // detach from terminal
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start background server: %w", err)
	}

	pid := cmd.Process.Pid

	// Write PID file
	pidFile := pidFilePath(port)
	if err := os.MkdirAll(filepath.Dir(pidFile), defaults.PublicDirPerm); err != nil {
		return fmt.Errorf("failed to create state dir: %w", err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), defaults.PublicFilePerm); err != nil {
		return fmt.Errorf("failed to write PID file: %w", err)
	}

	// Readiness probe: poll health endpoint
	serverURL := fmt.Sprintf("http://localhost:%d", port)
	healthURL := serverURL + defaults.RouteHealth.String()
	ready := false
	for range defaults.HealthCheckAttempts {
		time.Sleep(defaults.HealthCheckInterval)
		resp, err := http.Get(healthURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
	}

	if !ready {
		fmt.Fprintf(os.Stderr, "Warning: server may not be ready (health check timed out)\n")
	}

	fmt.Printf("Peasant web dashboard running at %s\n", serverURL)
	fmt.Printf("To stop the server, run:\n")
	fmt.Printf("  peasant web stop\n")
	fmt.Printf("\nIf the server becomes unresponsive, the process ID (%d)\n", pid)
	fmt.Printf("is saved at %s\n", pidFile)

	// Auto-open browser. Failure is non-fatal (the server is already running),
	// but it MUST be surfaced so the user knows to open the URL themselves.
	if !noBrowser {
		if err := browser.Open(serverURL); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not open browser automatically: %v\n", err)
			fmt.Fprintf(os.Stderr, "Open this URL manually: %s\n", serverURL)
		}
	}

	// Detach: parent exits, child continues
	_ = cmd.Process.Release()
	return nil
}

// stopWeb sends a shutdown request to the running server.
// Falls back to SIGTERM via PID file if the HTTP request fails.
func stopWeb(port int) error {
	pidFile := pidFilePath(port)

	// Try HTTP shutdown first
	client := &http.Client{Timeout: defaults.ServerClientTimeout}
	shutdownURL := fmt.Sprintf("http://localhost:%d%s", port, defaults.RouteShutdown)
	resp, err := client.Post(shutdownURL, defaults.ContentJSON.String(), nil)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			fmt.Println("Peasant web dashboard shutting down")
			os.Remove(pidFile)
			return nil
		}
		fmt.Fprintf(os.Stderr, "Unexpected response: %d, falling back to SIGTERM\n", resp.StatusCode)
	} else {
		fmt.Fprintf(os.Stderr, "HTTP shutdown failed: %v, falling back to SIGTERM\n", err)
	}

	// Fallback: read PID file and send SIGTERM
	data, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		return fmt.Errorf("cannot contact server and no PID file at %s: %w", pidFile, readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if parseErr != nil {
		return fmt.Errorf("invalid PID in %s: %w", pidFile, parseErr)
	}
	proc, findErr := os.FindProcess(pid)
	if findErr != nil {
		os.Remove(pidFile)
		return fmt.Errorf("process %d not found: %w", pid, findErr)
	}
	if sigErr := proc.Signal(syscall.SIGTERM); sigErr != nil {
		os.Remove(pidFile)
		return fmt.Errorf("failed to send SIGTERM to PID %d: %w", pid, sigErr)
	}
	fmt.Printf("Sent SIGTERM to PID %d\n", pid)
	os.Remove(pidFile)
	return nil
}

func pidFilePath(port int) string {
	return filepath.Join(defaults.State.DirPath.String(), fmt.Sprintf("web:%d.pid", port))
}

// configureVerboseLogging sets the default slog level to Debug,
// making slog.Debug calls visible on stderr.
func configureVerboseLogging() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
}
