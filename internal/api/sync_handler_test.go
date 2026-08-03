package api

import (
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// TestSyncSessionResponse_UnifiedHarnessKey verifies the GET /api/v1/sync/sessions
// feed emits the harness under json:"harness", not the legacy json:"provider".
// This keeps the sync feed aligned with the local API session payloads.
func TestSyncSessionResponse_UnifiedHarnessKey(t *testing.T) {
	resp := syncSessionResponse{
		ID:      "99d59925-36bc-424c-a789-8be54d9702ba",
		Harness: defaults.HarnessClaudeCode.String(),
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if _, ok := keys["harness"]; !ok {
		t.Errorf("sync feed must emit json:\"harness\"; got: %s", b)
	}
	if _, ok := keys["provider"]; ok {
		t.Errorf("sync feed must NOT emit legacy json:\"provider\"; got: %s", b)
	}
}
