package main

import (
	"errors"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

func TestPublicationListsNeverLoadTranscriptBundles(t *testing.T) {
	row := ingest.PushSessionRow{SessionID: testutil.TestSessionUUID}
	db := &testutil.StubPushStore{
		Sessions:          []ingest.PushSessionRow{row},
		ListEntriesErr:    errors.New("transcript reads forbidden during list construction"),
		PublicationInputs: map[ingest.SessionID]ingest.PublicationInputBundle{testutil.TestSessionUUID: {Metadata: schema.UnifiedMetadata{Model: testutil.TestModel}, Readiness: ingest.PublicationReady}},
	}
	wizard, err := buildPushWizardSessions(t.Context(), db, push.PushCandidateQuery{Method: config.PushMethodAll}, nil)
	if err != nil || len(wizard) != 1 || wizard[0].NeedsIngest || wizard[0].Meta == nil {
		t.Fatalf("wizard projection: %+v %v", wizard, err)
	}
	record := buildRedactionRecord(t.Context(), db.Sessions, db, redact.Standard)
	if record.MissingMetadataCount != 0 || db.PublicationMetadataCalls != 2 || db.PublicationInputCalls != 0 || db.ListEntriesCalls != 0 {
		t.Fatalf("list/report read transcripts: record=%+v projection=%d full=%d entries=%d", record, db.PublicationMetadataCalls, db.PublicationInputCalls, db.ListEntriesCalls)
	}
	db.PublicationInputErr = errors.New("database unavailable")
	wizard, err = buildPushWizardSessions(t.Context(), db, push.PushCandidateQuery{Method: config.PushMethodAll}, nil)
	if err != nil || len(wizard) != 1 || !wizard[0].NeedsIngest {
		t.Fatalf("database failure did not hold wizard: %+v %v", wizard, err)
	}
	if record := buildRedactionRecord(t.Context(), db.Sessions, db, redact.Standard); record.MissingMetadataCount != 1 {
		t.Fatalf("database failure report: %+v", record)
	}
}
