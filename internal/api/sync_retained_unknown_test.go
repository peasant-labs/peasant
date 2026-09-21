package api

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/sync_retained_unknown.yaml
var syncUnknownYAML []byte

func TestSyncRetainedUnknownConsent(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name    string `yaml:"name"`
			Payload string `yaml:"payload"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(syncUnknownYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("expected one YAML document")
	}
	seen := map[string]bool{}
	for _, c := range fixture.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatal("blank or duplicate case")
		}
		seen[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("unknown consent", "case", fixture.RequiredNames, seen); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Setenv(defaults.EnvXDGConfigHome.String(), t.TempDir())
			db := seedSyncMirrorCase(t, syncPublicationMirrorCase{Name: c.Name})
			input, _, err := push.LoadPublicationInput(t.Context(), db, testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			record, err := ingest.NewRetainedUnknown(schema.HarnessClaudeCode, "record", "future", ingest.UnknownSourcePosition{Line: 2, Public: &ingest.UnknownPublicPosition{SourceRef: "source-0", RecordIndex: 1, Position: 2}}, json.RawMessage(c.Payload))
			if err != nil {
				t.Fatal(err)
			}
			carrier, err := ingest.RetainedUnknownEntry(input.Metadata.SessionID, 1, record)
			if err != nil {
				t.Fatal(err)
			}
			entries := append(input.Entries, carrier)
			versions := testutil.HarvesterVersionsForSeed(t, schema.HarnessClaudeCode)
			written := db.IndexSessionEntryBatch(t.Context(), []ingest.SessionEntryWrite{{SessionID: input.Metadata.SessionID, Result: indexformat.V1{Entries: entries}, IndexVersion: versions.IndexVersion, IndexerVersion: versions.IndexerVersion, CaptureRevision: input.CaptureRevision, RequireFullContent: true, ContentCapture: ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureIncomplete, FailureCode: ingest.ContentCaptureUnknownDataRetained, CaptureFormat: ingest.ContentCaptureFormatFull, SourceAuthority: ingest.ContentSourceNewIngest}}})
			if len(written) != 1 || written[0].Err != nil {
				t.Fatalf("seed full retained capture: %+v", written)
			}
			cfg := config.BaseConfig()
			cfg.Redaction.CustomPatterns = []config.CustomPattern{{ID: "unknown-review", Category: config.CategoryProject, Pattern: "custom-secret", Replacement: "[CUSTOM]"}}
			handler := &syncHandler{store: db, config: cfg}
			raw, err := handler.readReviewContent(t.Context(), testutil.TestSessionUUID, nil)
			if err != nil {
				t.Fatal(err)
			}
			var metadata struct {
				Diagnostics schema.DiagnosticsInfo `json:"diagnostics"`
			}
			var envelope schema.TranscriptContent
			reader := json.NewDecoder(strings.NewReader(raw))
			if err := reader.Decode(&metadata); err != nil {
				t.Fatal(err)
			}
			if err := reader.Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if metadata.Diagnostics.Partial == nil || !*metadata.Diagnostics.Partial || !envelope.SessionDetail.Diagnostics.Partial {
				t.Fatal("review partial mirror lost")
			}
			if envelope.SessionDetail.RetainedUnknown[0].Payload != c.Payload {
				t.Fatal("review mutated retained payload")
			}
			ctx, cancel := context.WithCancel(t.Context())
			server := NewServer(ServerConfig{Port: 0, Store: db, Config: cfg})
			if err := server.Listen(ctx); err != nil {
				cancel()
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- server.Serve(ctx) }()
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			})
			response, err := http.Get("http://" + server.Addr().String() + defaults.RouteSyncRedactions.String() + "?session_id=" + testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("scan refused: %d %s", response.StatusCode, body)
			}
			var scan groupedRedactionResponse
			if err := json.Unmarshal(body, &scan); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, category := range scan.Categories {
				for _, rule := range category.Rules {
					if rule.RuleID == "unknown-review" {
						for _, item := range rule.Items {
							found = found || item.OriginalText == "custom-secret"
						}
					}
				}
			}
			if !found {
				t.Fatalf("mounted consent scanner missed escaped secret: %s", body)
			}
		})
	}
}
