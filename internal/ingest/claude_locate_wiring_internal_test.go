package ingest

import (
	"context"
	"testing"
)

// TestScopeLocatorAttachesClaudeDerivedLookup pins the production wiring of the
// indexed identity reader. Without it a located Claude build refuses to run, so
// every scoped Claude id would surface as unreadable or lost; the assertion is
// on the adapter the pipeline actually builds for a scoped run.
func TestScopeLocatorAttachesClaudeDerivedLookup(t *testing.T) {
	t.Parallel()

	locator := newScopeLocator(&Pipeline{adapters: DefaultAdapterRegistry})
	locator.derived = wiringDerivedLookup{}

	adapter, ok := locator.adapterFor(HarnessClaudeCode)
	if !ok {
		t.Fatal("the Claude adapter is not registered")
	}
	claude, ok := adapter.(*ClaudeAdapter)
	if !ok {
		t.Fatalf("adapterFor returned %T, want *ClaudeAdapter", adapter)
	}
	if claude.derivedLookup == nil {
		t.Fatal("the scoped Claude adapter has no derived lookup reader; a located build would refuse to run")
	}
}

// wiringDerivedLookup is the minimal reader the wiring test needs.
type wiringDerivedLookup struct{}

func (wiringDerivedLookup) CountClaudeIdentityDeclarers(context.Context, ClaudeTeammateIdentity) (int, error) {
	return 0, nil
}

func (wiringDerivedLookup) CountClaudeIdentitySpawners(context.Context, ClaudeTeammateIdentity) (int, error) {
	return 0, nil
}

func (wiringDerivedLookup) ClaudeIdentitySpawnerPaths(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (wiringDerivedLookup) CountCodexRolloutsBySession(context.Context, SessionID) (int, error) {
	return 0, nil
}

func (wiringDerivedLookup) CodexRolloutPaths(context.Context, SessionID) ([]ResolvedPath, error) {
	return nil, nil
}

var _ DerivedLookupReader = wiringDerivedLookup{}
