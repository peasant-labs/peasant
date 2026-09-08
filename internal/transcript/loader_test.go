package transcript_test

import (
	"context"
	_ "embed"
	"errors"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

var _ ingest.FullSessionEntryReader = (*store.Store)(nil)

//go:embed testdata/full_loader.yaml
var fullLoaderYAML []byte

func TestFullLoaderSQLitePageProgress(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name                   string
			Bytes, Budget, Entries int
		}
	}
	if err := yaml.Unmarshal(fullLoaderYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range fixture.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatal("invalid fixture name")
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			db := storetest.Open(t)
			id := ingest.SessionID(testutil.TestSessionUUID)
			storetest.SeedSession(t, db, string(id))
			text := strings.Repeat("x", c.Bytes) + " SAFE-END"
			toolInput := `{"body":"` + strings.Repeat("i", 5000) + `"}`
			toolOutput := strings.Repeat("o", 6000)
			var entries []schema.SessionEntry
			for i := 0; i < c.Entries; i++ {
				entries = append(entries, schema.SessionEntry{SessionID: id, EntryIndex: i, Role: schema.RoleAssistant, Harness: schema.HarnessClaudeCode, EntryType: schema.EntryTypeText, ContentPreview: &text, ToolInput: &toolInput, ToolOutput: &toolOutput})
			}
			if err := testutil.WriteFullEntries(t.Context(), db, id, entries); err != nil {
				t.Fatal(err)
			}
			loaded, capture, err := transcript.LoadEntriesForDetail(t.Context(), db, id, transcript.DetailLoadOptions{SoftMaxBytes: int64(c.Budget)})
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded) != len(entries) || capture.EntryCount != len(entries) {
				t.Fatal("page continuation lost entries")
			}
			for i, entry := range loaded {
				if entry.EntryIndex != i || entry.ContentPreview == nil || *entry.ContentPreview != text {
					t.Fatal("page hydration truncated or reordered content")
				}
				if entry.ToolInput == nil || *entry.ToolInput != toolInput || entry.ToolOutput == nil || *entry.ToolOutput != toolOutput {
					t.Fatal("full loader altered semantic tool fields")
				}
			}
		})
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing fixture %s", name)
		}
	}
}

//go:embed testdata/full_reader_delegation.yaml
var fullReaderDelegationYAML []byte

type fullReaderStub struct {
	calls   int
	budget  int64
	id      ingest.SessionID
	capture ingest.SessionContentCapture
	err     error
}

var _ ingest.FullSessionEntryReader = (*fullReaderStub)(nil)

func (s *fullReaderStub) LoadFullSessionEntries(_ context.Context, id ingest.SessionID, budget int64) ([]schema.SessionEntry, ingest.SessionContentCapture, error) {
	s.calls++
	s.id, s.budget = id, budget
	return nil, s.capture, s.err
}

func TestFullLoaderMandatoryDelegation(t *testing.T) {
	var f struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name, Failure string
			Budget        int64
		}
	}
	if err := yaml.Unmarshal(fullReaderDelegationYAML, &f); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range f.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatal("invalid fixture name")
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			id := ingest.SessionID(testutil.TestSessionUUID)
			r := &fullReaderStub{capture: ingest.SessionContentCapture{SessionID: id, Status: ingest.ContentCaptureComplete, FullCaptureSHA256: strings.Repeat("a", 64)}}
			switch c.Failure {
			case "status":
				r.capture.Status = ingest.ContentCaptureIncomplete
			case "hash":
				r.capture.FullCaptureSHA256 = ""
			case "identity":
				r.capture.SessionID = ""
			case "count":
				r.capture.EntryCount = 1
			case "reader":
				r.err = errors.New("synthetic read failure")
			}
			_, _, err := transcript.LoadEntriesForDetail(t.Context(), r, id, transcript.DetailLoadOptions{SoftMaxBytes: c.Budget})
			if (err != nil) != (c.Failure != "") {
				t.Fatalf("unexpected load result: %v", err)
			}
			if c.Budget < 0 {
				if r.calls != 0 {
					t.Fatal("negative budget reached reader")
				}
				return
			}
			if r.calls != 1 || r.id != id || r.budget != c.Budget {
				t.Fatal("full reader must receive exactly one unchanged delegation")
			}
		})
	}
	for _, name := range f.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing fixture %s", name)
		}
	}
}
