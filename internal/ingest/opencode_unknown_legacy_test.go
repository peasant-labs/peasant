package ingest_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/opencode_unknown_legacy.yaml
var openCodeLegacyUnknownYAML []byte

func TestOpenCodeLegacyJSONUnknownStoreAndReindex(t *testing.T) {
	var fixture struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name        string            `yaml:"name"`
			Message     string            `yaml:"message"`
			Parts       map[string]string `yaml:"parts"`
			SourceID    string            `yaml:"source_id"`
			Pointer     string            `yaml:"pointer"`
			RecordIndex int64             `yaml:"record_index"`
			Position    int64             `yaml:"position"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(openCodeLegacyUnknownYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("trailing fixture document", err)
	}
	names := []string{}
	if len(fixture.Required) == 0 {
		t.Fatal("missing required-name manifest")
	}
	seen := map[string]bool{}
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatal("duplicate or empty fixture name")
		}
		seen[row.Name] = true
		names = append(names, row.Name)
	}
	slices.Sort(names)
	slices.Sort(fixture.Required)
	if !slices.Equal(names, fixture.Required) {
		t.Fatal("required fixture membership changed")
	}
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			const sid schema.SessionID = "ses_opencodeLegacyUnknown"
			root := t.TempDir()
			messageDir := filepath.Join(root, "storage", "message", string(sid))
			partDir := filepath.Join(root, "storage", "part", "msg_1")
			for _, dir := range []string{messageDir, partDir} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			secret := "ghp_" + strings.Repeat("A", 36)
			payload := strings.Repeat("synthetic-long-", 1024) + " " + secret + " FULL_UNKNOWN_TAIL"
			messagePath := filepath.Join(messageDir, "msg_1.json")
			if err := os.WriteFile(messagePath, []byte(strings.ReplaceAll(row.Message, "PAYLOAD", payload)), 0600); err != nil {
				t.Fatal(err)
			}
			for id, data := range row.Parts {
				if err := os.WriteFile(filepath.Join(partDir, id+".json"), []byte(strings.ReplaceAll(data, "PAYLOAD", payload)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			session := ingest.DiscoveredSession{SessionID: sid, Harness: ingest.HarnessOpenCode, OriginalRoot: ingest.ResolvedPath(root)}
			idx := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{})
			capture, err := idx.IndexTranscriptForCapture(t.Context(), session)
			if err != nil {
				t.Fatal(err)
			}
			if len(capture.RetainedUnknown) != 1 {
				t.Fatalf("evidence: %+v", capture.RetainedUnknown)
			}
			evidence := capture.RetainedUnknown[0]
			if evidence.Position.SourceID != row.SourceID || evidence.Position.JSONPointer != row.Pointer || evidence.Position.Public == nil || evidence.Position.Public.RecordIndex != row.RecordIndex || evidence.Position.Public.Position != row.Position {
				t.Fatalf("source position lost: %+v", evidence.Position)
			}
			if len(evidence.Payload) < 8192 || !bytes.Contains(evidence.Payload, []byte("9007199254740993")) || !bytes.Contains(evidence.Payload, []byte("FULL_UNKNOWN_TAIL")) || bytes.Contains(evidence.Payload, []byte(secret)) {
				t.Fatal("opaque payload truncated, rounded or unredacted")
			}
			db, err := store.Open(filepath.Join(t.TempDir(), "legacy.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			seedOpenCodeRepairSession(t, db, string(sid), sid)
			write := func(entries []schema.SessionEntry) {
				if err := db.IndexSessionEntries(t.Context(), sid, entries); err != nil {
					t.Fatal(err)
				}
			}
			write(capture.Entries)
			stored, err := db.ListEntries(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			again, err := idx.IndexTranscriptForCapture(t.Context(), session)
			if err != nil {
				t.Fatal(err)
			}
			write(again.Entries)
			reindexed, err := db.ListEntries(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored, reindexed) {
				t.Fatal("retained reindex changed source evidence")
			}
			for _, entry := range reindexed {
				records, err := ingest.RetainedUnknownOf(entry)
				if err != nil {
					t.Fatal(err)
				}
				for _, got := range records {
					if !bytes.Equal(got.Payload, evidence.Payload) {
						t.Fatal("stored evidence changed")
					}
				}
			}
			if err := os.WriteFile(messagePath, []byte(`{"role":42}`), 0600); err != nil {
				t.Fatal(err)
			}
			if failed, err := idx.IndexTranscriptForCapture(t.Context(), session); err == nil || len(failed.Entries) != 0 {
				t.Fatal("known corruption authorized a replacement")
			}
			after, err := db.ListEntries(t.Context(), sid)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, stored) {
				t.Fatal("failed recapture changed prior good evidence")
			}
		})
	}
}
