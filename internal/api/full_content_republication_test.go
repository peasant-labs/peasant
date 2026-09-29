package api

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/full_content_republication.yaml
var fullRepublicationYAML []byte

func TestFullContentRepublicationPreservesIdentityAndExplicitMetadata(t *testing.T) {
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name                 string
			CurrentVisibility    string `yaml:"currentVisibility"`
			CurrentLicense       string `yaml:"currentLicense"`
			RequestedVisibility  string `yaml:"requestedVisibility"`
			RequestedLicense     string `yaml:"requestedLicense"`
			Explicit             bool   `yaml:"explicit"`
			ExpectedOwnerUpdates int    `yaml:"expectedOwnerUpdates"`
			ExpectedVisibility   string `yaml:"expectedVisibility"`
			ExpectedLicense      string `yaml:"expectedLicense"`
		}
	}
	if err := testutil.DecodeNamedFixtureYAML(fullRepublicationYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range fixtures.Cases {
		if f.Name == "" || seen[f.Name] {
			t.Fatal("invalid republication fixture")
		}
		seen[f.Name] = true
		t.Run(f.Name, func(t *testing.T) {
			t.Parallel()
			hs := newTestXDGHomes(t)
			const id = "eeee5555-eeee-4eee-8eee-eeeeeeeeeeee"
			base := filepath.Join(hs.Data, "retained")
			db := seedSyncDoorSession(t, hs.dbPath(), id, base)
			defer db.Close()
			cfg := config.BaseConfig()
			cfg.Output.BasePath = base
			currentVisibility := schema.Visibility(f.CurrentVisibility)
			currentLicense := schema.License(f.CurrentLicense)
			requestedVisibility := schema.Visibility(f.RequestedVisibility)
			requestedLicense := schema.License(f.RequestedLicense)
			expectedVisibility := schema.Visibility(f.ExpectedVisibility)
			expectedLicense := schema.License(f.ExpectedLicense)
			if !currentVisibility.IsValid() || !requestedVisibility.IsValid() || !expectedVisibility.IsValid() || !currentLicense.IsValid() || !requestedLicense.IsValid() || !expectedLicense.IsValid() {
				t.Fatal("invalid access fixture")
			}
			var mu sync.Mutex
			var previousIdentity json.RawMessage
			var receipt schema.AuthoritativePublishResponse
			publishes, updates := 0, 0
			// The controlled dependency owns shares; publication must not request a
			// destructive replacement or issue a share mutation during content repair.
			shares := []string{"synthetic-collective"}
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.Contains(r.URL.Path, "/transcripts/publish"):
					captured := &syncCapturedPublish{parts: map[string]string{}}
					captured.record(r)
					parts := captured.snapshot()
					var request map[string]json.RawMessage
					if err := json.Unmarshal([]byte(parts["metadata"]), &request); err != nil {
						t.Error(err)
						return
					}
					if publishes > 0 && !bytesEqualJSON(previousIdentity, request["identity"]) {
						t.Error("repair changed publication identity")
					}
					previousIdentity = request["identity"]
					if _, ok := request["shares"]; ok {
						t.Error("content publish unexpectedly mutates shares")
					}
					raw, err := testutil.AuthoritativePublishReceipt([]byte(parts["metadata"]), publishes == 0)
					if err != nil {
						t.Error(err)
						return
					}
					if err := json.Unmarshal(raw, &receipt); err != nil {
						t.Error(err)
						return
					}
					receipt.Visibility = currentVisibility
					receipt.Applied.NormalizedValues.Visibility = currentVisibility
					if receipt.Applied.License == nil {
						// A publish that sends no license keeps the one the
						// transcript already has.
						kept := currentLicense
						receipt.Applied.License = &kept
					}
					if publishes > 0 {
						for name, part := range parts {
							if !strings.Contains(part, "REPAIRED-SAFE-TAIL") || strings.Contains(part, syncDoorSecret) {
								t.Errorf("replacement multipart %s lost full redacted tail", name)
							}
						}
						_, sent := request["license"]
						if f.Explicit && (!sent || *receipt.Applied.License != requestedLicense) {
							t.Error("explicitly requested license ignored")
						}
						if !f.Explicit && sent {
							t.Error("an update sent the configured default license")
						}
					}
					status := http.StatusOK
					if publishes == 0 {
						status = http.StatusCreated
					}
					publishes++
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(receipt)
				case r.Method == http.MethodPatch:
					var update schema.OwnerTranscriptUpdateRequest
					if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
						t.Error(err)
						return
					}
					if update.Visibility == nil {
						t.Error("owner update missing explicit visibility")
						return
					}
					updates++
					_ = json.NewEncoder(w).Encode(schema.OwnerTranscriptUpdateResponse{TranscriptID: receipt.TranscriptID, TranscriptURL: receipt.TranscriptURL, Visibility: *update.Visibility, Tags: []string{}, UpdatedAt: 2})
				case r.Method == http.MethodGet:
					_, _ = w.Write([]byte(`{}`))
				default:
					t.Errorf("unexpected remote mutation %s %s", r.Method, r.URL.Path)
				}
			}))
			defer remote.Close()
			creds := &auth.Credentials{VillageURL: remote.URL, APIKey: "synthetic-key", UserID: "user-1", Username: "tester", KeyID: "key-1"}
			redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
			if err != nil {
				t.Fatal(err)
			}
			// An explicit run passes the values the way --visibility and
			// --license do; otherwise they are the configured defaults.
			run := func(visibility schema.Visibility, license schema.License, explicit bool) {
				t.Helper()
				runCfg := push.PipelineConfig{Force: true, FilterSessionIDs: []string{id}}
				if explicit {
					runCfg.Visibility, runCfg.ChangeVisibility = visibility, true
					runCfg.License, runCfg.ChangeLicense = license, true
				} else {
					cfg.Push.Visibility, cfg.Push.License = visibility, license
				}
				pipeline, err := push.NewPipeline(db, village.NewVillageClient(remote.URL, creds.APIKey, nil), creds, cfg, &ingest.OSFileSystem{}, runCfg, redactor, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				result, err := pipeline.Run(t.Context())
				if err != nil || result.Errors != 0 || result.New+result.Updated != 1 {
					t.Fatalf("publication failed: %+v %v", result, err)
				}
			}
			run(currentVisibility, currentLicense, false)
			before, err := db.Publication(t.Context(), remote.URL, creds.UserID, testutil.TestProjectHash, id)
			if err != nil || before == nil {
				t.Fatalf("missing initial receipt: %v", err)
			}
			full := strings.Repeat("safe prose ", 300) + "REPAIRED-SAFE-TAIL " + syncDoorSecret
			input, err := db.LoadPublicationInput(t.Context(), ingest.SessionID(id))
			if err != nil {
				t.Fatal(err)
			}
			testutil.SeedReadyPublication(t, db, &input.Metadata, []schema.SessionEntry{{SessionID: ingest.SessionID(id), EntryIndex: 1, Harness: defaults.HarnessClaudeCode, Role: schema.RoleAssistant, EntryType: schema.EntryTypeText, ContentPreview: &full}})
			// Repair passes no flags, so the update keeps the current access and
			// license whatever the configuration now says. Only an explicit
			// request changes a published transcript.
			run(requestedVisibility, requestedLicense, f.Explicit)
			after, err := db.Publication(t.Context(), remote.URL, creds.UserID, testutil.TestProjectHash, id)
			if err != nil || after == nil {
				t.Fatalf("missing replacement receipt: %v", err)
			}
			if before.Receipt.TranscriptID != after.Receipt.TranscriptID || before.Receipt.TranscriptURL != after.Receipt.TranscriptURL || before.Receipt.ContentHash == after.Receipt.ContentHash || after.Receipt.Visibility != expectedVisibility || after.Receipt.Applied.License == nil || *after.Receipt.Applied.License != expectedLicense {
				t.Fatal("replacement receipt lost identity, access, license, or changed content")
			}
			mu.Lock()
			defer mu.Unlock()
			if updates != f.ExpectedOwnerUpdates || publishes != 2 || !reflect.DeepEqual(shares, []string{"synthetic-collective"}) {
				t.Fatal("replacement changed remote shares or used incorrect owner-update behavior")
			}
		})
	}
	for _, name := range fixtures.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing fixture %s", name)
		}
	}
}

func bytesEqualJSON(a, b []byte) bool {
	var left, right any
	return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && reflect.DeepEqual(left, right)
}
