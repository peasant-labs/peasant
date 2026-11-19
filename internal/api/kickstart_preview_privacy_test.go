package api

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema/route"
)

//go:embed testdata/kickstart_preview_privacy.yaml
var kickstartPreviewPrivacyYAML []byte

const privacySessionRow = "session"

type previewPrivacySecrets struct {
	RepoPath    string `yaml:"repoPath"`
	Branch      string `yaml:"branch"`
	SetupID     string `yaml:"setupID"`
	SourceID    string `yaml:"sourceID"`
	NodeID      string `yaml:"nodeID"`
	MissingRow  string `yaml:"missingRow"`
	Transcript  string `yaml:"transcript"`
	ReaderError string `yaml:"readerError"`
	Cursor      string `yaml:"cursor"`
}

type previewPrivacyCase struct {
	Name         string `yaml:"name"`
	Row          string `yaml:"row"`
	Route        string `yaml:"route"`
	Method       string `yaml:"method"`
	Host         string `yaml:"host"`
	Origin       string `yaml:"origin"`
	Suffix       string `yaml:"suffix"`
	UnknownSetup bool   `yaml:"unknownSetup"`
	Noncanonical bool   `yaml:"noncanonical"`
	EscapedRoute bool   `yaml:"escapedRoute"`
	FallbackPath bool   `yaml:"fallbackPath"`
	Status       int    `yaml:"status"`
	Code         string `yaml:"code"`
	Expected     string `yaml:"expected"`
	NoStore      bool   `yaml:"noStore"`
	RawLog       string `yaml:"rawLog"`
}

type previewPrivacyFixture struct {
	Secrets       previewPrivacySecrets `yaml:"secrets"`
	SocketCases   []string              `yaml:"socketCases"`
	RequiredNames []string              `yaml:"requiredNames"`
	Cases         []previewPrivacyCase  `yaml:"cases"`
}

// TestKickstartPreviewPrivacy drives the handler installed by Server.Listen.
// It stays serial because slog.SetDefault changes process-wide state. No user
// store or source path is opened; the listeners and isolated home are cleaned up.
func TestKickstartPreviewPrivacy(t *testing.T) {
	var fixture previewPrivacyFixture
	if err := testutil.DecodeNamedFixtureYAML(kickstartPreviewPrivacyYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	s := fixture.Secrets
	source := filepath.Join(t.TempDir(), "session.jsonl")
	record := fmt.Sprintf("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":%q}}\n", s.Transcript)
	if err := os.WriteFile(source, []byte(record), 0600); err != nil {
		t.Fatal(err)
	}
	projectID := fmt.Sprintf("repo:%d:%s", len(s.RepoPath), s.RepoPath)
	branchID := fmt.Sprintf("branch:%d:%s%s", len(projectID), projectID, s.Branch)
	homes := newTestXDGHomes(t)
	server := NewServer(homes.config(ServerConfig{ConfigPath: filepath.Join(homes.Config, "config.yaml")}))
	if err := server.Listen(context.Background()); err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		server.kickstart.shutdown()
		for _, listener := range server.lns {
			_ = listener.Close()
		}
	})
	server.kickstart.byID[s.SetupID] = &kickstartSession{listings: []onboarding.SessionListing{{
		SessionID: s.SourceID, Harness: "claude-code",
		Source: onboarding.SessionSource{Path: source, Origin: onboarding.SessionSourceOriginFile},
	}}, forest: onboarding.Forest{Nodes: []onboarding.Node{
		{ID: projectID, Kind: onboarding.NodeKindProject, Label: "synthetic project"},
		{ID: branchID, ParentID: projectID, Kind: onboarding.NodeKindBranch, Label: s.Branch},
		{ID: s.NodeID, ParentID: branchID, Kind: onboarding.NodeKindSession, Label: "synthetic session", SessionID: s.SourceID, Labels: onboarding.Labels{Location: s.RepoPath}},
	}}}
	rows := map[string]string{"project": projectID, "branch": branchID, "session": s.NodeID, "missing": s.MissingRow + s.RepoPath + s.Branch}
	secrets := []string{s.RepoPath, s.Branch, s.SetupID, s.SourceID, s.NodeID, s.MissingRow, s.Transcript, s.ReaderError, s.Cursor}

	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			method := c.Method
			if method == "" {
				method = http.MethodGet
			}
			requestPath := c.Route
			if requestPath == "" {
				row, ok := rows[c.Row]
				if !ok {
					t.Fatalf("unknown fixture row %q", c.Row)
				}
				setup := s.SetupID
				if c.UnknownSetup {
					setup += "-unknown"
				}
				previewPath, err := route.LocalGetKickstartPreviewPath(setup, row)
				if err != nil {
					t.Fatalf("build preview path: %v", err)
				}
				requestPath = previewPath + c.Suffix
				if c.Noncanonical {
					requestPath = strings.Replace(requestPath, "/tree/", "//tree/", 1)
				}
				if c.EscapedRoute {
					requestPath = strings.Replace(requestPath, "/tree/", "/%74ree/", 1)
				}
				requestPath += "?cursor=" + url.QueryEscape(s.Cursor)
			}
			request := httptest.NewRequest(method, "http://localhost"+requestPath, nil)
			if c.Host != "" {
				request.Host = c.Host
			}
			if c.Origin != "" {
				request.Header.Set("Origin", c.Origin)
			}
			response := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(response, request)
			if response.Code != c.Status {
				t.Fatalf("status = %d, want %d; body %s", response.Code, c.Status, response.Body)
			}
			if c.NoStore {
				got := response.Header().Values("Cache-Control")
				if len(got) != 1 || got[0] != "no-store" {
					t.Errorf("Cache-Control = %q, want exactly [no-store]", got)
				}
			}
			body := response.Body.String()
			if c.Row == privacySessionRow && strings.Contains(body, s.Transcript) {
				t.Error("HTTP session preview contains transcript content")
			}
			if c.Expected != "" && !strings.Contains(body, c.Expected) {
				t.Errorf("body %q does not contain %q", body, c.Expected)
			}
			if c.Code != "" {
				var result struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Code != c.Code {
					t.Errorf("error code = %q, want %q (decode error %v)", result.Code, c.Code, err)
				}
			}
			for _, secret := range secrets {
				for _, representation := range []string{secret, url.PathEscape(secret), url.QueryEscape(secret)} {
					if strings.Contains(logs.String(), representation) {
						t.Errorf("application log contains synthetic marker %q: %s", representation, &logs)
					}
					// Repository previews intentionally serve repository metadata. A mux
					// redirect intentionally returns its URL, not an error response.
					if c.Status >= 400 && strings.Contains(body, representation) {
						t.Errorf("error body contains synthetic marker %q: %s", representation, body)
					}
				}
			}
			type requestLogEntry struct {
				Message string `json:"msg"`
				Path    string `json:"path"`
			}
			var entries []requestLogEntry
			decoder := json.NewDecoder(&logs)
			for decoder.More() {
				var entry requestLogEntry
				if err := decoder.Decode(&entry); err != nil {
					t.Fatalf("decode request log: %v", err)
				}
				entries = append(entries, entry)
			}
			wantPath := c.RawLog
			if wantPath == "" {
				wantPath = defaults.RouteKickstartPreview.String()
				if c.FallbackPath {
					wantPath = defaults.RouteKickstartSessions.String() + "/{path omitted}"
				}
			}
			var loggedPath string
			for _, entry := range entries {
				if entry.Message == "http" {
					loggedPath = entry.Path
				}
				if entry.Message == "http: refused a request that did not come from this local server" && entry.Path != wantPath {
					t.Errorf("guard log path = %q, want %q", entry.Path, wantPath)
				}
			}
			if loggedPath != wantPath {
				t.Errorf("request log path = %q, want %q", loggedPath, wantPath)
			}
		})
	}
}
