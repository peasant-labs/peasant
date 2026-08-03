package ingest

import (
	"testing"
)

func TestSourceConfig_EmptyPaths(t *testing.T) {
	// A zero-value SourceConfig must be safe to use without panicking.
	var cfg SourceConfig

	if cfg.Paths != nil {
		t.Errorf("zero-value SourceConfig.Paths = %v, want nil", cfg.Paths)
	}
	if cfg.Enabled {
		t.Errorf("zero-value SourceConfig.Enabled = true, want false")
	}

	// Ranging over nil slice must not panic.
	count := 0
	for range cfg.Paths {
		count++
	}
	if count != 0 {
		t.Errorf("ranging nil Paths yielded %d iterations, want 0", count)
	}
}

func TestDiscoveredSession_ZeroValue(t *testing.T) {
	// A zero-value DiscoveredSession must be safe to inspect without panicking.
	var ds DiscoveredSession

	if ds.SessionID != "" {
		t.Errorf("zero-value SessionID = %q, want empty string", ds.SessionID)
	}
	if ds.Harness != "" {
		t.Errorf("zero-value Harness = %q, want empty string", ds.Harness)
	}
	if ds.ParentUUID != nil {
		t.Errorf("zero-value ParentUUID = %v, want nil", ds.ParentUUID)
	}
	if ds.SubagentPaths != nil {
		t.Errorf("zero-value SubagentPaths = %v, want nil", ds.SubagentPaths)
	}
	if ds.DebugPaths != nil {
		t.Errorf("zero-value DebugPaths = %v, want nil", ds.DebugPaths)
	}

	// Ranging over nil slices must not panic.
	for range ds.SubagentPaths {
		t.Errorf("unexpected iteration over nil SubagentPaths")
	}
	for range ds.DebugPaths {
		t.Errorf("unexpected iteration over nil DebugPaths")
	}
}
