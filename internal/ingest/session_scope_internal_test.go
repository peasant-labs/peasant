package ingest

import (
	"context"
	"strings"
	"testing"
)

// TestPipelineRunRefusesScopeWithUnsetPolicy covers the one unusable scope a
// caller cannot build through NewSessionScope: ids with the zero policy. Run
// must refuse it before the first stage, so no source is read and no store row
// is touched. The exported shapes — the zero scope, and a valid scope beside an
// allowlist — are covered by the fixture-driven test alongside this one.
func TestPipelineRunRefusesScopeWithUnsetPolicy(t *testing.T) {
	t.Parallel()
	pipeline := &Pipeline{config: PipelineConfig{
		Scope: &SessionScope{ids: []SessionID{"0f3c1a2b-4d5e-4f60-8a71-9b2c3d4e5f60"}},
	}}
	result, err := pipeline.Run(context.Background())
	if err == nil {
		t.Fatalf("Run returned a result (%v), want a refusal", result)
	}
	if !strings.Contains(err.Error(), "no related policy") {
		t.Fatalf("Run error does not name the missing policy:\n%v", err)
	}
}
