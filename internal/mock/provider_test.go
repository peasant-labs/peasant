package mock

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/testutil"
)

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
