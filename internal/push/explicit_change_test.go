package push_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestPipeline_RefusesAVisibilityChangeItCannotApply pins the pipeline's own
// refusal, which every caller that sets ChangeVisibility meets. Downgraded to
// the private fallback, an explicit group would narrow a transcript shared
// with collectives, so nothing is uploaded.
func TestPipeline_RefusesAVisibilityChangeItCannotApply(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	seedMemFS(t, fs, testutil.TestHostSlug, testutil.TestSessionUUID, defaults.HarnessClaudeCode)
	storeDouble := &testutil.StubPushStore{Sessions: []ingest.PushSessionRow{makeSession(testutil.TestSessionUUID, testutil.TestHostSlug, defaults.HarnessClaudeCode.String(), nil)}}
	publisher := &testutil.StubPublisher{}
	var stderr bytes.Buffer
	_, err := newTestPipeline(storeDouble, publisher, fs, baseTestConfig(), push.PipelineConfig{Visibility: schema.VisibilityGroup, ChangeVisibility: true}, &stderr).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot be applied") {
		t.Fatalf("an explicit group change must be refused; err = %v", err)
	}
	if len(publisher.AuthoritativeCalls) != 0 || len(storeDouble.SavedPublicationIDs) != 0 {
		t.Fatalf("a refused change must upload nothing and record nothing; uploads=%d receipts=%d", len(publisher.AuthoritativeCalls), len(storeDouble.SavedPublicationIDs))
	}
}

// TestPipeline_UnreadableAttemptLedgerSavesNoReceipt covers the upload the
// village answered as an update while no receipt exists. Whether it finishes a
// first publish depends on the attempt ledger; when that cannot be read the
// session fails without a receipt, so the next run decides again, instead of
// recording the transcript at the private visibility it landed at.
func TestPipeline_UnreadableAttemptLedgerSavesNoReceipt(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	seedMemFS(t, fs, testutil.TestHostSlug, testutil.TestSessionUUID, defaults.HarnessClaudeCode)
	storeDouble := &testutil.StubPushStore{
		Sessions:                    []ingest.PushSessionRow{makeSession(testutil.TestSessionUUID, testutil.TestHostSlug, defaults.HarnessClaudeCode.String(), nil)},
		LatestPublicationAttemptErr: errors.New("attempt ledger unavailable"),
	}
	publisher := &testutil.StubPublisher{StatusCode: 200}
	var stderr bytes.Buffer
	result, err := newTestPipeline(storeDouble, publisher, fs, baseTestConfig(), push.PipelineConfig{}, &stderr).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Errors != 1 || len(storeDouble.SavedPublicationIDs) != 0 {
		t.Fatalf("an unreadable attempt ledger must fail the session without a receipt; result=%+v receipts=%d", result, len(storeDouble.SavedPublicationIDs))
	}
	if !strings.Contains(result.Sessions[0].Error.Error(), "earlier attempts could not be read") {
		t.Fatalf("the failure must say why; got %v", result.Sessions[0].Error)
	}
}

// TestPipeline_UnreadableReceiptHistorySendsNothing covers the read that tells
// an update from a first publication before the upload. When it cannot be
// read, the session fails before anything is sent: carrying on as "never
// published" would send the configured license to a transcript that may
// already carry another one.
func TestPipeline_UnreadableReceiptHistorySendsNothing(t *testing.T) {
	t.Parallel()
	fs := testutil.NewMemFS()
	seedMemFS(t, fs, testutil.TestHostSlug, testutil.TestSessionUUID, defaults.HarnessClaudeCode)
	storeDouble := &testutil.StubPushStore{
		Sessions:              []ingest.PushSessionRow{makeSession(testutil.TestSessionUUID, testutil.TestHostSlug, defaults.HarnessClaudeCode.String(), nil)},
		PublishedToVillageErr: errors.New("receipt history unavailable"),
	}
	publisher := &testutil.StubPublisher{}
	var stderr bytes.Buffer
	result, err := newTestPipeline(storeDouble, publisher, fs, baseTestConfig(), push.PipelineConfig{}, &stderr).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Errors != 1 || len(publisher.AuthoritativeCalls) != 0 {
		t.Fatalf("an unreadable receipt history must fail the session before any upload; result=%+v uploads=%d", result, len(publisher.AuthoritativeCalls))
	}
}
