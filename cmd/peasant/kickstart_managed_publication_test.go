package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/kickstart_managed_publication.yaml
var kickstartManagedPublicationYAML []byte

// Exercise the retained stage directly, not the shipped default command wiring.
// Local ingestion and publication are real; only the external Village is fake.
// Keep this serial: the local ingest runner temporarily replaces slog.Default.
func TestKickstartManagedPublicationStage(t *testing.T) {
	var fixture struct {
		RequiredNames []string                       `yaml:"requiredNames"`
		Sessions      []selectionRunnerSourceSession `yaml:"sessions"`
		Cases         []struct {
			Name          string            `yaml:"name"`
			Visibility    schema.Visibility `yaml:"visibility"`
			License       schema.License    `yaml:"license"`
			LocalOnly     bool              `yaml:"localOnly"`
			NoSelection   bool              `yaml:"noSelection"`
			Skip          bool              `yaml:"skip"`
			InvalidRoot   bool              `yaml:"invalidRoot"`
			Incomplete    bool              `yaml:"incomplete"`
			WantUpload    bool              `yaml:"wantUpload"`
			ErrorContains []string          `yaml:"errorContains"`
			ExpectedTurns []struct {
				Role    schema.Role `yaml:"role"`
				Content string      `yaml:"content"`
				Depth   int         `yaml:"depth"`
			} `yaml:"expectedTurns"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(kickstartManagedPublicationYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatal("publication fixture must contain one document")
	}
	required := strings.Fields("selected-managed-session local-only no-selection invalid-output-root incomplete-latest-attempt")
	if !reflect.DeepEqual(required, fixture.RequiredNames) {
		t.Fatal("publication required-name manifest changed")
	}
	seen := map[string]bool{}
	for _, row := range fixture.Cases {
		if !slices.Contains(required, row.Name) || seen[row.Name] {
			t.Fatalf("unknown/duplicate publication case %q", row.Name)
		}
		seen[row.Name] = true
		if row.WantUpload && len(row.ExpectedTurns) == 0 {
			t.Fatalf("publication case %q requires expected turns", row.Name)
		}
		for _, turn := range row.ExpectedTurns {
			if (turn.Role != schema.RoleUser && turn.Role != schema.RoleAssistant) || strings.TrimSpace(turn.Content) == "" {
				t.Fatalf("publication case %q has an invalid expected text turn: %+v", row.Name, turn)
			}
		}
	}
	if err := testutil.RequireFixtureNames("managed publication", "case", required, seen); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Sessions) != 2 {
		t.Fatal("publication witness requires a selected session and an unselected sibling")
	}
	for _, session := range fixture.Sessions {
		if strings.TrimSpace(session.Prompt) == "" || strings.TrimSpace(session.Reply) == "" {
			t.Fatalf("publication source %q requires explicit prompt and reply", session.SessionID)
		}
	}
	if fixture.Sessions[0].Prompt == fixture.Sessions[1].Prompt || fixture.Sessions[0].Reply == fixture.Sessions[1].Reply {
		t.Fatal("selected and sibling publication prompts and replies must be distinct")
	}
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			root := t.TempDir()
			output := filepath.Join(root, "saved-nondefault-output")
			cfg := config.BaseConfig()
			cfg.Output.BasePath = output
			cfg.Selection.Mode = config.SelectionModeAll
			cfg.Push.Method = "all"
			configPath := filepath.Join(root, "config.yaml")
			cmd := &cobra.Command{Use: "kickstart"}
			cmd.Flags().String("data-dir", root, "")
			cmd.Flags().String("config-dir", root, "")
			if !row.Skip {
				source := filepath.Join(root, "source")
				writeSelectionRunnerSources(t, source, fixture.Sessions)
				cfg.Sources = config.SourcesConfig{ClaudeCode: config.SourceProviderConfig{Enabled: true, Paths: []string{source}}}
				if err := config.SaveAtomic(configPath, cfg); err != nil {
					t.Fatal(err)
				}
				run, _ := kickstartLocalIngest(cmd, configPath, selectionRunnerListings(fixture.Sessions))
				result, err := run(t.Context())
				if err != nil || result == nil || result.New != len(fixture.Sessions) {
					t.Fatalf("real ingestion: %+v %v", result, err)
				}
				if row.Incomplete {
					seedLatestAttemptPreviewAtRoot(t, root, output, fixture.Sessions[0].SessionID, "newer incomplete body")
				}
			}
			if row.InvalidRoot {
				cfg.Output.BasePath = filepath.Join(root, "regular-file")
				if err := os.WriteFile(cfg.Output.BasePath, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := config.SaveAtomic(configPath, cfg); err != nil {
				t.Fatal(err)
			}
			captured := &capturedPublish{parts: map[string]string{}}
			var requests, uploads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPatch {
					var request schema.OwnerTranscriptUpdateRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Visibility == nil {
						t.Errorf("decode visibility update: %v", err)
						http.Error(w, "invalid update", 400)
						return
					}
					if schema.Visibility(*request.Visibility) != row.Visibility {
						t.Error("wrong visibility update")
					}
					id, err := schema.NewTranscriptID(testutil.TestSessionUUID)
					if err != nil {
						t.Error(err)
						return
					}
					_ = json.NewEncoder(w).Encode(schema.OwnerTranscriptUpdateResponse{TranscriptID: id, TranscriptURL: "https://village.example/transcripts/" + id.String(), License: &row.License, Visibility: *request.Visibility, Tags: []string{}, UpdatedAt: 2})
					return
				}
				if !strings.Contains(r.URL.Path, "/transcripts/publish") {
					_ = json.NewEncoder(w).Encode(schema.SchemaVersionResponse{MinPushContractVersion: "0.1.0", PushContractVersion: defaults.PublishSchemaVersion, ContentCapabilities: []schema.ContentCapability{schema.ContentCapabilityObservedModelV1, schema.ContentCapabilitySessionGraphProvenanceV1}})
					return
				}
				uploads.Add(1)
				captured.record(t, r)
				receipt, err := testutil.AuthoritativePublishReceipt([]byte(captured.snapshot()["metadata"]), true)
				if err != nil {
					t.Error(err)
					http.Error(w, "invalid publish", 500)
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write(receipt)
			}))
			defer server.Close()
			writeTestCredentialsFor(t, root, server.URL)
			answers := ftue.WizardAnswers{FinalConsent: true, Destination: ftue.DestinationVillage, EffectiveVisibility: row.Visibility, License: row.License, ProviderSelections: []ftue.ProviderSelection{{Harness: defaults.HarnessClaudeCode.String()}}}
			if row.LocalOnly {
				answers.Destination = ftue.DestinationLocal
			}
			if !row.NoSelection {
				answers.SelectedSessions = selectionRunnerListings(fixture.Sessions[:1])
			}
			stage := buildKickstartJourneyOperations(cmd, configPath, cfg, nil, true, nil, kickstartJourneyDeps{})[ftue.StagePublication]
			effects, retries, err := stage(t.Context(), ftue.JourneyRequest{Answers: answers})
			if len(row.ErrorContains) > 0 {
				if err == nil {
					t.Fatal("expected publication refusal")
				}
				for _, text := range row.ErrorContains {
					if !strings.Contains(err.Error(), text) {
						t.Errorf("error %v lacks %q", err, text)
					}
				}
				want := []ftue.RetryTarget{{Stage: ftue.StagePublication, SessionIDs: []string{fixture.Sessions[0].SessionID}}}
				if !reflect.DeepEqual(retries, want) || len(effects) != 0 {
					t.Fatalf("refusal effects=%+v retries=%+v", effects, retries)
				}
				if row.InvalidRoot {
					if requests.Load() != 0 {
						t.Fatal("invalid root contacted Village before reader setup succeeded")
					}
					unchanged, readErr := os.ReadFile(cfg.Output.BasePath)
					if readErr != nil || string(unchanged) != "not a directory" {
						t.Fatal("reader setup changed the invalid configured root")
					}
					var pathErr *os.PathError
					if !errors.As(err, &pathErr) {
						t.Fatalf("lost wrapped filesystem cause: %v", err)
					}
				}
			} else if err != nil || len(retries) != 0 {
				t.Fatalf("publication: %v retries=%+v", err, retries)
			}
			if row.Skip {
				if len(effects) != 1 || effects[0].Status != ftue.StatusSkipped || requests.Load() != 0 {
					t.Fatalf("skip effects=%+v requests=%d", effects, requests.Load())
				}
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Fatalf("skip created owned root: %v", err)
				}
				return
			}
			wantUploads := int32(0)
			if row.WantUpload {
				wantUploads = 1
			}
			if uploads.Load() != wantUploads {
				t.Fatalf("uploads=%d want %d, effects=%+v", uploads.Load(), wantUploads, effects)
			}
			db, err := openPreparedStore(t, defaults.ResolveDBFilePathWith(root).String(), mustGenerationStoreOptions(t, output)...)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for index, session := range fixture.Sessions {
				input, err := db.LoadPublicationInput(t.Context(), ingest.SessionID(session.SessionID))
				if err != nil {
					t.Fatal(err)
				}
				record, err := db.Publication(t.Context(), server.URL, "user-00001", input.ReceiptProjectHash, session.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				if row.WantUpload && index == 0 {
					if record == nil || record.Receipt.Visibility != row.Visibility {
						t.Fatalf("incorrect saved receipt: %+v", record)
					}
					if record.Receipt.Applied.License == nil || *record.Receipt.Applied.License != row.License {
						t.Fatalf("incorrect saved license: %+v", record.Receipt.Applied)
					}
				} else if record != nil {
					t.Fatalf("false publication receipt for %s: %+v", session.SessionID, record)
				}
			}
			if row.WantUpload {
				if len(effects) != 1 {
					t.Fatalf("publication effects=%+v", effects)
				}
				if effects[0].SessionID != fixture.Sessions[0].SessionID || effects[0].Stage != ftue.StagePublication || effects[0].Status != ftue.StatusPersisted {
					t.Fatalf("publication effects=%+v", effects)
				}
				parts := captured.snapshot()
				body := parts["transcript_file"]
				content, err := schema.DecodeTranscriptContentRaw([]byte(body))
				if err != nil {
					t.Fatalf("decode uploaded transcript: %v", err)
				}
				if content.SessionDetail == nil {
					t.Fatal("uploaded transcript lacks session detail")
				}
				turns := content.SessionDetail.Turns
				if len(turns) != len(row.ExpectedTurns) {
					t.Fatalf("uploaded turn cardinality=%d want %d", len(turns), len(row.ExpectedTurns))
				}
				for index, want := range row.ExpectedTurns {
					if turns[index].Role != want.Role || turns[index].Content != want.Content {
						t.Errorf("uploaded turn %d role/content=%q/%q want %q/%q", index, turns[index].Role, turns[index].Content, want.Role, want.Content)
					}
					if turns[index].Depth != want.Depth {
						t.Errorf("uploaded turn %d depth=%d want %d", index, turns[index].Depth, want.Depth)
					}
					if len(turns[index].ToolCalls) != 0 {
						t.Errorf("uploaded text turn %d has unexpected tool calls: %+v", index, turns[index].ToolCalls)
					}
				}
				if strings.Contains(strings.Join(mapValues(parts), "\n"), fixture.Sessions[1].SessionID) {
					t.Fatalf("uploaded parts contain sibling session: %v", parts)
				}
				metadata, err := schema.DecodeAuthoritativePublishRequest([]byte(parts["metadata"]))
				if err != nil {
					t.Fatal(err)
				}
				if metadata.Identity.SessionID.String() != fixture.Sessions[0].SessionID || metadata.License != row.License {
					t.Fatalf("incorrect uploaded metadata: %+v", metadata)
				}
			}
		})
	}
}
