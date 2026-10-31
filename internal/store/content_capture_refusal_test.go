package store_test

import (
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_capture_refusals.yaml
var contentCaptureRefusalsYAML []byte

type contentCaptureRefusalCase struct {
	Name           string `yaml:"name"`
	Status         string `yaml:"status"`
	Format         string `yaml:"format"`
	Code           string `yaml:"code"`
	Message        string `yaml:"message"`
	Rowless        bool   `yaml:"rowless"`
	Producer       string `yaml:"producer"`
	WantReason     string `yaml:"wantReason"`
	WantDetail     string `yaml:"wantDetail"`
	WantRepairable bool   `yaml:"wantRepairable"`
}

// TestFullReadRefusalNamesTheStoredReasonAndOneRepair walks both full readers
// over every refused capture state. The proof must hold (no transcript is
// returned, and the error stays in the incomplete category), and the refusal
// must state the stored reason and exactly one way forward.
func TestFullReadRefusalNamesTheStoredReasonAndOneRepair(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string                    `yaml:"requiredNames"`
		Cases         []contentCaptureRefusalCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(contentCaptureRefusalsYAML, &fixture); err != nil {
		t.Fatalf("content_capture_refusals.yaml: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	for _, required := range fixture.RequiredNames {
		if !slices.Contains(names, required) {
			t.Fatalf("content_capture_refusals.yaml lost required case %q", required)
		}
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			s := openTestStore(t)
			e := publicationEntry(t, "11111111-1111-4111-8111-111111111111")
			id := e.Metadata.SessionID
			revision := capturePublication(t, s, e)
			if r := indexPublication(t, s, e, revision, batchTestEntries(id, "entry", 1)); r.Err != nil {
				t.Fatal(r.Err)
			}
			if c.Rowless {
				publicationSQL(t, s, `DELETE FROM session_content_captures WHERE session_id = ?`, string(id))
			} else {
				publicationSQL(t, s, `UPDATE session_content_captures SET status=?, capture_format=?, failure_code=NULLIF(?,''), failure_message=NULLIF(?,'') WHERE session_id=?`,
					c.Status, c.Format, c.Code, c.Message, string(id))
			}
			producer := ingest.GenerationRepairTargets[ingest.HarnessClaudeCode].IndexerVersion
			if c.Producer == "stale" {
				producer = 1
			}
			execContentSQL(t, s, fmt.Sprintf("UPDATE sessions SET index_version=%d, indexed_input_hash='%s' WHERE session_id='%s'", producer, strings.Repeat("b", 64), id))

			snapshot, readErr := s.ReadSessionContent(t.Context(), string(id))
			if readErr == nil || snapshot != nil {
				t.Fatalf("ReadSessionContent served a refused capture: snapshot=%v err=%v", snapshot, readErr)
			}
			page, pageErr := s.ReadSessionEntries(t.Context(), id, ingest.SessionEntryReadOptions{Mode: ingest.SessionEntryReadFullContent})
			if pageErr == nil || len(page.Entries) != 0 {
				t.Fatalf("ReadSessionEntries served a refused capture: entries=%d err=%v", len(page.Entries), pageErr)
			}
			for _, err := range []error{readErr, pageErr} {
				if !errors.Is(err, store.ErrContentCaptureIncomplete) {
					t.Fatalf("refusal left the incomplete category: %v", err)
				}
				var refusal *store.ContentCaptureRefusal
				if !errors.As(err, &refusal) {
					t.Fatalf("refusal carries no stored reason: %v", err)
				}
				if refusal.Reason != c.WantReason || refusal.Detail != c.WantDetail || refusal.Repairable != c.WantRepairable {
					t.Fatalf("refusal = %+v, want reason %q detail %q repairable %v", *refusal, c.WantReason, c.WantDetail, c.WantRepairable)
				}
				text := err.Error()
				if !strings.Contains(text, "stored reason "+c.WantReason) {
					t.Fatalf("refusal does not state the stored reason: %s", text)
				}
				repair := "run peasant harvest index --session " + string(id) + " to repair it"
				if c.WantRepairable != strings.Contains(text, repair) {
					t.Fatalf("repairable=%v but the repair text disagrees: %s", c.WantRepairable, text)
				}
				if !c.WantRepairable && !strings.Contains(text, "upgrade Peasant") {
					t.Fatalf("a final refusal must say what would lift it: %s", text)
				}
				for _, stale := range []string{"retry", "--force", "harvest index --force"} {
					if strings.Contains(text, stale) {
						t.Fatalf("refusal offers a second remedy %q: %s", stale, text)
					}
				}
			}
		})
	}
}

// TestFullReadIntegrityFailureNamesTheForcedRepairOnce proves damaged content
// keeps its own category and is told the one command that replaces it: an
// ordinary index reads the capture as settled and leaves the damage in place.
func TestFullReadIntegrityFailureNamesTheForcedRepairOnce(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := publicationEntry(t, "11111111-1111-4111-8111-111111111111")
	id := e.Metadata.SessionID
	revision := capturePublication(t, s, e)
	if r := indexPublication(t, s, e, revision, batchTestEntries(id, "entry", 1)); r.Err != nil {
		t.Fatal(r.Err)
	}
	publicationSQL(t, s, `UPDATE session_content_captures SET full_capture_sha256 = ? WHERE session_id = ?`, strings.Repeat("b", 64), string(id))

	_, err := s.ReadSessionContent(t.Context(), string(id))
	if !errors.Is(err, store.ErrContentIntegrity) || errors.Is(err, store.ErrContentCaptureIncomplete) {
		t.Fatalf("damaged content changed category: %v", err)
	}
	if got := strings.Count(err.Error(), "harvest index --force"); got != 1 {
		t.Fatalf("the error names the repair %d times, want once: %v", got, err)
	}
	if strings.Contains(err.Error(), "retry") {
		t.Fatalf("the error offers a retry that cannot help: %v", err)
	}
}
