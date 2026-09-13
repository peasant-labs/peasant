package push_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestPreflightSeparatesRunWideFailureFromSessionRefusal pins the boundary the
// preflight decides on, from both sides, on one arrangement each.
//
// A refused candidate is that candidate's own verdict: the run keeps going and
// the healthy session still publishes. An unusable database is the run's verdict:
// the push stops with one run-level error instead of handing every candidate the
// same unusable "re-ingest this session" advice.
//
// The run-wide case uses the error a REAL closed store produces, captured from
// one, so the classification is measured against the database's own words rather
// than a message this test invented. Discovery is answered by the stub, so the
// refusal reaches the preflight rather than stopping at the candidate query.
func TestPreflightSeparatesRunWideFailureFromSessionRefusal(t *testing.T) {
	closedStoreErr := closedStoreReadError(t)

	t.Run("closed-database-stops-the-run", func(t *testing.T) {
		db := &testutil.StubPushStore{
			Sessions: []ingest.PushSessionRow{
				makeSession(testutil.TestSessionUUID, testutil.TestHostSlug, string(defaults.HarnessClaudeCode), nil),
				makeSession(testutil.TestSubagentID, testutil.TestHostSlug, string(defaults.HarnessClaudeCode), nil),
			},
			PublicationInputErr: closedStoreErr,
		}
		publisher := &testutil.StubPublisher{StatusCode: 201}
		var output bytes.Buffer
		pipeline := newTestPipeline(db, publisher, testutil.NewMemFS(), baseTestConfig(), push.PipelineConfig{}, &output)
		result, err := pipeline.Run(context.Background())
		if err == nil {
			t.Fatalf("an unreadable database did not stop the run: %+v", result)
		}
		if result != nil && len(result.Sessions) != 0 {
			t.Fatalf("an unreadable database produced per-session refusals: %+v", result.Sessions)
		}
		if len(publisher.Calls) != 0 {
			t.Fatalf("an unreadable database reached the village: %d calls", len(publisher.Calls))
		}
	})

	t.Run("refused-candidate-keeps-the-healthy-one", func(t *testing.T) {
		fs := testutil.NewMemFS()
		healthy := testutil.TestSubagentID
		seedMemFS(t, fs, testutil.TestHostSlug, healthy, defaults.HarnessClaudeCode)
		db := &testutil.StubPushStore{
			Sessions: []ingest.PushSessionRow{
				// Refused: nothing seeded it, so its capture is not publishable.
				makeSession(testutil.TestSessionUUID, testutil.TestHostSlug, string(defaults.HarnessClaudeCode), nil),
				makeSession(healthy, testutil.TestHostSlug, string(defaults.HarnessClaudeCode), nil),
			},
		}
		publisher := &testutil.StubPublisher{StatusCode: 201}
		var output bytes.Buffer
		pipeline := newTestPipeline(db, publisher, fs, baseTestConfig(), push.PipelineConfig{Concurrency: 1}, &output)
		result, err := pipeline.Run(context.Background())
		if err != nil {
			t.Fatalf("one refused candidate stopped the run: %v", err)
		}
		if result.Errors != 1 || result.New != 1 {
			t.Fatalf("refused and healthy candidates were not both reported: %+v", result)
		}
		if len(publisher.Calls) != 1 {
			t.Fatalf("uploads = %d, want only the healthy candidate", len(publisher.Calls))
		}
		var refused error
		for _, session := range result.Sessions {
			if session.SessionID == testutil.TestSessionUUID {
				refused = session.Error
			}
		}
		if push.ClassifyPushError(refused) != push.CategoryMetadataMissing {
			t.Fatalf("the refusal lost its category: %v", refused)
		}
	})
}

// closedStoreReadError returns the error a real store produces for a publication
// read after Close, so the run-wide classification is tested against the
// database's actual failure rather than a hand-written message.
func closedStoreReadError(t *testing.T) error {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "peasant.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	id, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := db.LoadPublicationInput(context.Background(), id)
	if readErr == nil {
		t.Fatal("a closed store answered a publication read; this test needs its real failure")
	}
	return readErr
}
