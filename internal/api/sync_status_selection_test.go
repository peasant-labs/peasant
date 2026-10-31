package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// TestSyncIngestStatusKeepsExcludedInUnchanged pins the Local API meaning of
// the sync result's "unchanged" count: sessions the saved selection left out
// stay inside it, as before ExcludedBySelection was split out of the CLI
// summary, so the endpoint's counts still add up to the run's sessions.
func TestSyncIngestStatusKeepsExcludedInUnchanged(t *testing.T) {
	t.Parallel()
	h := &syncHandler{}
	h.ingestResult = &ingest.PipelineResult{Summary: ingest.PipelineSummary{Unchanged: 2, ExcludedBySelection: 3}}
	rec := httptest.NewRecorder()
	h.handleSyncIngestStatus(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sync/ingest/status", nil))
	var body struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	if body.Result["unchanged"] != float64(5) {
		t.Fatalf("result.unchanged = %v, want 5 (2 unchanged + 3 excluded by selection)", body.Result["unchanged"])
	}
	if _, ok := body.Result["excluded"]; ok {
		t.Fatalf("result gained an excluded field; adding one changes the Local API contract")
	}
}
