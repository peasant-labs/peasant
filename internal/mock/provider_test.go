package mock

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// The capture fixtures below are the mounted proof that the flat local read
// carries authorized current-target navigation and retained earlier history
// through the real read path, and that an absent target degrades to an honest
// reference instead of a link.
func TestContextNavigationFixtureServesAuthorizedLinksAndRetainedHistory(t *testing.T) {
	read, err := api.SessionDetailReadForProvider(context.Background(), NewProvider(), contextNavigationChildID)
	if err != nil {
		t.Fatalf("session detail read for %s failed: %v", contextNavigationChildID, err)
	}

	byKind := make(map[schema.SessionRelationshipKind]schema.SessionRelationshipNavigation)
	for _, entry := range read.RelationshipNavigation {
		byKind[entry.Kind] = entry
	}
	if len(read.RelationshipNavigation) != 2 {
		t.Fatalf("relationshipNavigation = %+v, want one entry per stored relationship", read.RelationshipNavigation)
	}
	contextFrom, ok := byKind[schema.SessionRelationshipContextFrom]
	if !ok || contextFrom.Status != schema.RelationshipNavigationResolved || contextFrom.LocalID == nil || string(*contextFrom.LocalID) != contextNavigationSourceID {
		t.Fatalf("context_from navigation = %+v, want a resolved link to %s", contextFrom, contextNavigationSourceID)
	}
	started, ok := byKind[schema.SessionRelationshipStartedBy]
	if !ok || started.Status != schema.RelationshipNavigationResolved || started.LocalID == nil || string(*started.LocalID) != contextNavigationStartedID {
		t.Fatalf("started_by navigation = %+v, want a resolved link to %s", started, contextNavigationStartedID)
	}

	if len(read.EarlierHistory) != 1 {
		t.Fatalf("earlierHistory sections = %d, want the one retained section", len(read.EarlierHistory))
	}
	if read.EarlierHistory[0].State != schema.EarlierHistoryUncertainMigrated || len(read.EarlierHistory[0].Turns) != 2 {
		t.Fatalf("earlierHistory[0] = %+v, want two retained turns with uncertain ownership", read.EarlierHistory[0])
	}
	for _, turn := range read.EarlierHistory[0].Turns {
		if turn.Provenance == nil || turn.Provenance.Ownership != schema.ContentOwnershipUncertain {
			t.Fatalf("retained earlier turn %d provenance = %+v, want retained uncertain ownership", turn.Index, turn.Provenance)
		}
	}
	if read.InputSubmissionCount == nil || *read.InputSubmissionCount != 1 {
		t.Fatalf("inputSubmissionCount = %v, want the measured single submission", read.InputSubmissionCount)
	}
	if read.TurnCount != len(read.Turns) {
		t.Fatalf("turnCount = %d, want the %d current main turns; retained earlier history never counts as a current turn", read.TurnCount, len(read.Turns))
	}
}

func TestContextNavigationFixtureReportsAbsentTargetWithoutAnIdentifier(t *testing.T) {
	read, err := api.SessionDetailReadForProvider(context.Background(), NewProvider(), contextNavigationUnresolvedID)
	if err != nil {
		t.Fatalf("session detail read for %s failed: %v", contextNavigationUnresolvedID, err)
	}
	if len(read.RelationshipNavigation) != 1 {
		t.Fatalf("relationshipNavigation = %+v, want one entry for the known target", read.RelationshipNavigation)
	}
	entry := read.RelationshipNavigation[0]
	if entry.Status != schema.RelationshipNavigationKnownUnavailable {
		t.Fatalf("navigation status = %q, want known_unavailable for a stored-absent target", entry.Status)
	}
	if entry.LocalID != nil || entry.TranscriptID != nil {
		t.Fatalf("unavailable navigation = %+v, want no identifier that could route to another session", entry)
	}
	if len(read.Turns) == 0 {
		t.Fatal("the child must stay readable when its current-parent target is unavailable")
	}
}

func TestProvider_Sessions(t *testing.T) {
	p := NewProvider()
	ctx := context.Background()

	sessions, err := p.Sessions(ctx)
	if err != nil {
		t.Fatalf("Sessions failed: %v", err)
	}

	if len(sessions) == 0 {
		t.Error("expected sessions, got 0")
	}

	for _, s := range sessions {
		if string(s.ID) == "" {
			t.Error("session ID is empty")
		}
		if s.Project == "" {
			t.Errorf("session %s project is empty", s.ID)
		}
		if len(s.Turns) == 0 && s.Metadata.TurnCount > 0 {
			t.Errorf("session %s has turnCount %d but 0 turns", s.ID, s.Metadata.TurnCount)
		}
	}
}

func TestProvider_DashboardMetrics(t *testing.T) {
	p := NewProvider()
	ctx := context.Background()

	metrics, err := p.DashboardMetrics(ctx)
	if err != nil {
		t.Fatalf("DashboardMetrics failed: %v", err)
	}

	if metrics.TotalSessions == 0 {
		t.Error("expected totalSessions > 0")
	}
	if metrics.AcceptanceRate == 0 {
		t.Error("expected acceptanceRate > 0")
	}
}

func TestProvider_QualitySessions(t *testing.T) {
	p := NewProvider()
	ctx := context.Background()

	// Test all
	sessions, err := p.QualitySessions(ctx, api.QualityFilter{})
	if err != nil {
		t.Fatalf("QualitySessions failed: %v", err)
	}
	if len(sessions) == 0 {
		t.Error("expected quality sessions, got 0")
	}

	// Test project filter
	if len(sessions) > 0 {
		project := sessions[0].Project
		filtered, err := p.QualitySessions(ctx, api.QualityFilter{
			Projects: []string{project},
		})
		if err != nil {
			t.Fatalf("Filtered QualitySessions failed: %v", err)
		}
		for _, s := range filtered {
			if s.Project != project {
				t.Errorf("expected project %s, got %s", project, s.Project)
			}
		}
	}
}

func TestMockTurns(t *testing.T) {
	count := 10
	base := testutil.TestSessionStartTime
	turns := MockTurns(count, base)

	if len(turns) != count {
		t.Errorf("expected %d turns, got %d", count, len(turns))
	}

	for i, turn := range turns {
		if turn.Index != i {
			t.Errorf("turn %d: expected index %d, got %d", i, i, turn.Index)
		}
		if turn.Role == "" {
			t.Errorf("turn %d: role is empty", i)
		}
		if turn.Content == "" {
			t.Errorf("turn %d: content is empty", i)
		}
		if turn.Timestamp.IsZero() {
			t.Errorf("turn %d: timestamp is zero", i)
		}
	}
}
