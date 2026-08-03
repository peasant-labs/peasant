package store_test

import (
	"context"
	"math"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
)

func TestCreateLesson(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	id, _, err := s.CreateLesson(ctx, store.CreateLessonParams{
		EpisodeAnnotationID: "ann-001",
		SessionID:           "sess-001",
		Topic:               "dependencies/compatibility",
		Rule:                "Check Python version compatibility before adding deps.",
		FailureMode:         "torch had no 3.13 wheel.",
	})
	if err != nil {
		t.Fatalf("CreateLesson: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty lesson ID")
	}

	lessons, err := s.ListLessons(ctx, "")
	if err != nil {
		t.Fatalf("ListLessons: %v", err)
	}
	if len(lessons) != 1 {
		t.Fatalf("expected 1 lesson, got %d", len(lessons))
	}

	l := lessons[0]
	if l.ID != id {
		t.Errorf("ID: got %q, want %q", l.ID, id)
	}
	if l.Topic != "dependencies/compatibility" {
		t.Errorf("Topic: got %q", l.Topic)
	}
	if l.Rule != "Check Python version compatibility before adding deps." {
		t.Errorf("Rule: got %q", l.Rule)
	}
	if l.FailureMode != "torch had no 3.13 wheel." {
		t.Errorf("FailureMode: got %q", l.FailureMode)
	}
	if l.SituationEmbedding != nil {
		t.Error("expected nil embedding before UpdateLessonEmbedding")
	}
}

func TestUpdateLessonEmbedding(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	id, _, err := s.CreateLesson(ctx, store.CreateLessonParams{
		EpisodeAnnotationID: "ann-002",
		SessionID:           "sess-002",
		Topic:               "concurrency/async",
		Rule:                "Verify methods are async before using gather.",
		FailureMode:         "Sync methods nullified asyncio.gather().",
	})
	if err != nil {
		t.Fatalf("CreateLesson: %v", err)
	}

	embedding := []float32{0.1, 0.2, 0.3, 0.4, 0.5}
	if err := s.UpdateLessonEmbedding(ctx, id, embedding); err != nil {
		t.Fatalf("UpdateLessonEmbedding: %v", err)
	}

	lessons, err := s.ListLessons(ctx, "")
	if err != nil {
		t.Fatalf("ListLessons: %v", err)
	}
	if len(lessons) != 1 {
		t.Fatalf("expected 1 lesson, got %d", len(lessons))
	}

	got := lessons[0].SituationEmbedding
	if got == nil {
		t.Fatal("expected non-nil embedding")
	}
	if len(got) != len(embedding) {
		t.Fatalf("embedding length: got %d, want %d", len(got), len(embedding))
	}
	for i := range embedding {
		if got[i] != embedding[i] {
			t.Errorf("embedding[%d]: got %f, want %f", i, got[i], embedding[i])
		}
	}
}

func TestLessonsWithEmbeddings(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// Create two lessons, embed only one.
	id1, _, _ := s.CreateLesson(ctx, store.CreateLessonParams{
		EpisodeAnnotationID: "ann-003",
		SessionID:           "sess-003",
		Topic:               "topic-a",
		Rule:                "rule-a",
		FailureMode:         "fail-a",
	})
	_, _, _ = s.CreateLesson(ctx, store.CreateLessonParams{
		EpisodeAnnotationID: "ann-004",
		SessionID:           "sess-004",
		Topic:               "topic-b",
		Rule:                "rule-b",
		FailureMode:         "fail-b",
	})

	s.UpdateLessonEmbedding(ctx, id1, []float32{1.0, 0.0, 0.0})

	embedded, err := s.LessonsWithEmbeddings(ctx)
	if err != nil {
		t.Fatalf("LessonsWithEmbeddings: %v", err)
	}
	if len(embedded) != 1 {
		t.Fatalf("expected 1 embedded lesson, got %d", len(embedded))
	}
	if embedded[0].ID != id1 {
		t.Errorf("expected embedded lesson ID %q, got %q", id1, embedded[0].ID)
	}
}

func TestListLessons_FilterBySession(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	_, _, _ = s.CreateLesson(ctx, store.CreateLessonParams{
		EpisodeAnnotationID: "ann-005",
		SessionID:           "sess-alpha",
		Topic:               "topic-1",
		Rule:                "rule-1",
		FailureMode:         "fail-1",
	})
	_, _, _ = s.CreateLesson(ctx, store.CreateLessonParams{
		EpisodeAnnotationID: "ann-006",
		SessionID:           "sess-beta",
		Topic:               "topic-2",
		Rule:                "rule-2",
		FailureMode:         "fail-2",
	})

	alpha, _ := s.ListLessons(ctx, "sess-alpha")
	if len(alpha) != 1 {
		t.Errorf("expected 1 lesson for sess-alpha, got %d", len(alpha))
	}

	all, _ := s.ListLessons(ctx, "")
	if len(all) != 2 {
		t.Errorf("expected 2 total lessons, got %d", len(all))
	}
}

func TestDeleteLessonsForSession(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	_, _, _ = s.CreateLesson(ctx, store.CreateLessonParams{
		EpisodeAnnotationID: "ann-007",
		SessionID:           "sess-del",
		Topic:               "topic",
		Rule:                "rule",
		FailureMode:         "fail",
	})
	_, _, _ = s.CreateLesson(ctx, store.CreateLessonParams{
		EpisodeAnnotationID: "ann-008",
		SessionID:           "sess-del",
		Topic:               "topic-2",
		Rule:                "rule-2",
		FailureMode:         "fail-2",
	})
	_, _, _ = s.CreateLesson(ctx, store.CreateLessonParams{
		EpisodeAnnotationID: "ann-009",
		SessionID:           "sess-keep",
		Topic:               "topic-3",
		Rule:                "rule-3",
		FailureMode:         "fail-3",
	})

	deleted, err := s.DeleteLessonsForSession(ctx, "sess-del")
	if err != nil {
		t.Fatalf("DeleteLessonsForSession: %v", err)
	}
	if deleted != 2 {
		t.Errorf("expected 2 deleted, got %d", deleted)
	}

	remaining, _ := s.ListLessons(ctx, "")
	if len(remaining) != 1 {
		t.Errorf("expected 1 remaining lesson, got %d", len(remaining))
	}
	if remaining[0].SessionID != "sess-keep" {
		t.Errorf("expected remaining lesson from sess-keep, got %s", remaining[0].SessionID)
	}
}

func TestCreateLesson_Idempotent(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	params := store.CreateLessonParams{
		EpisodeAnnotationID: "ann-idem-1",
		SessionID:           "sess-idem",
		Topic:               "testing/idempotent",
		Rule:                "Make imports idempotent.",
		FailureMode:         "Duplicate rows from re-runs.",
	}

	id1, created1, err := s.CreateLesson(ctx, params)
	if err != nil {
		t.Fatalf("first CreateLesson: %v", err)
	}
	if !created1 {
		t.Error("first CreateLesson: expected created=true for new lesson")
	}

	// Second call with same (topic, rule, failure_mode) returns existing ID and created=false.
	id2, created2, err := s.CreateLesson(ctx, params)
	if err != nil {
		t.Fatalf("second CreateLesson: %v", err)
	}
	if id2 != id1 {
		t.Errorf("expected same ID on duplicate, got %q vs %q", id1, id2)
	}
	if created2 {
		t.Error("second CreateLesson: expected created=false for duplicate lesson")
	}

	// Only one row should exist, with original values preserved.
	lessons, _ := s.ListLessons(ctx, "")
	if len(lessons) != 1 {
		t.Fatalf("expected 1 lesson after duplicate insert, got %d", len(lessons))
	}
	if lessons[0].EpisodeAnnotationID != "ann-idem-1" {
		t.Errorf("expected original EpisodeAnnotationID preserved, got %q", lessons[0].EpisodeAnnotationID)
	}
	if lessons[0].SessionID != "sess-idem" {
		t.Errorf("expected original SessionID preserved, got %q", lessons[0].SessionID)
	}

	// Third call with different source episode — same lesson content but different provenance.
	params3 := store.CreateLessonParams{
		EpisodeAnnotationID: "ann-idem-2",
		SessionID:           "sess-idem-2",
		Topic:               "testing/idempotent",
		Rule:                "Make imports idempotent.",
		FailureMode:         "Duplicate rows from re-runs.",
	}
	id3, created3, err := s.CreateLesson(ctx, params3)
	if err != nil {
		t.Fatalf("third CreateLesson: %v", err)
	}
	if created3 {
		t.Error("expected created=false for third call with same content")
	}
	if id3 != id1 {
		t.Errorf("expected same ID on third duplicate, got %q vs %q", id3, id1)
	}

	// Verify lesson_sources: should have 2 rows (one per unique source episode).
	// The second call (same params as first) was INSERT OR IGNORE'd,
	// but the third call (different annotation+session) should succeed.
	sources, err := s.LessonSources(ctx, id1)
	if err != nil {
		t.Fatalf("LessonSources: %v", err)
	}
	if len(sources) != 2 {
		t.Errorf("expected 2 lesson_sources rows (one per unique source episode), got %d", len(sources))
	}
}

func TestCreateLesson_UniqueComposite(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// Base lesson — the anchor for all comparisons.
	baseID, created, err := s.CreateLesson(ctx, store.CreateLessonParams{
		EpisodeAnnotationID: "ann-comp-base",
		SessionID:           "sess-comp-base",
		Topic:               "t/base",
		Rule:                "base rule",
		FailureMode:         "base fail",
	})
	if err != nil {
		t.Fatalf("base CreateLesson: %v", err)
	}
	if !created {
		t.Fatal("base CreateLesson: expected created=true")
	}

	// Only topic differs — must be a new row.
	idTopicChanged, created, err := s.CreateLesson(ctx, store.CreateLessonParams{
		EpisodeAnnotationID: "ann-comp-topic",
		SessionID:           "sess-comp-topic",
		Topic:               "t/other",
		Rule:                "base rule",
		FailureMode:         "base fail",
	})
	if err != nil {
		t.Fatalf("topic-changed CreateLesson: %v", err)
	}
	if !created {
		t.Error("topic-changed CreateLesson: expected created=true (different topic)")
	}
	if idTopicChanged == baseID {
		t.Error("topic-changed lesson must have a distinct ID from base")
	}

	// Only rule differs — must be a new row.
	idRuleChanged, created, err := s.CreateLesson(ctx, store.CreateLessonParams{
		EpisodeAnnotationID: "ann-comp-rule",
		SessionID:           "sess-comp-rule",
		Topic:               "t/base",
		Rule:                "other rule",
		FailureMode:         "base fail",
	})
	if err != nil {
		t.Fatalf("rule-changed CreateLesson: %v", err)
	}
	if !created {
		t.Error("rule-changed CreateLesson: expected created=true (different rule)")
	}
	if idRuleChanged == baseID {
		t.Error("rule-changed lesson must have a distinct ID from base")
	}

	// Only failure_mode differs — must be a new row.
	idFailChanged, created, err := s.CreateLesson(ctx, store.CreateLessonParams{
		EpisodeAnnotationID: "ann-comp-fail",
		SessionID:           "sess-comp-fail",
		Topic:               "t/base",
		Rule:                "base rule",
		FailureMode:         "other fail",
	})
	if err != nil {
		t.Fatalf("failure_mode-changed CreateLesson: %v", err)
	}
	if !created {
		t.Error("failure_mode-changed CreateLesson: expected created=true (different failure_mode)")
	}
	if idFailChanged == baseID {
		t.Error("failure_mode-changed lesson must have a distinct ID from base")
	}

	// Exact duplicate of base — must return the existing row, not a new one.
	idDup, created, err := s.CreateLesson(ctx, store.CreateLessonParams{
		EpisodeAnnotationID: "ann-comp-dup",
		SessionID:           "sess-comp-dup",
		Topic:               "t/base",
		Rule:                "base rule",
		FailureMode:         "base fail",
	})
	if err != nil {
		t.Fatalf("exact-duplicate CreateLesson: %v", err)
	}
	if created {
		t.Error("exact-duplicate CreateLesson: expected created=false")
	}
	if idDup != baseID {
		t.Errorf("exact-duplicate must return base ID %q, got %q", baseID, idDup)
	}

	// Total rows: base + topic-changed + rule-changed + failure_mode-changed = 4.
	// The exact duplicate must NOT produce a fifth row.
	all, err := s.ListLessons(ctx, "")
	if err != nil {
		t.Fatalf("ListLessons: %v", err)
	}
	if len(all) != 4 {
		t.Errorf("expected 4 lessons (base + 3 near-duplicates), got %d", len(all))
	}
}

func TestCosineSimilarity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b []float32
		want float64
	}{
		{
			name: "identical",
			a:    []float32{1, 0, 0},
			b:    []float32{1, 0, 0},
			want: 1.0,
		},
		{
			name: "orthogonal",
			a:    []float32{1, 0, 0},
			b:    []float32{0, 1, 0},
			want: 0.0,
		},
		{
			name: "opposite",
			a:    []float32{1, 0, 0},
			b:    []float32{-1, 0, 0},
			want: -1.0,
		},
		{
			name: "45 degrees",
			a:    []float32{1, 0},
			b:    []float32{1, 1},
			want: 1.0 / math.Sqrt(2),
		},
		{
			name: "empty",
			a:    []float32{},
			b:    []float32{},
			want: 0.0,
		},
		{
			name: "length mismatch",
			a:    []float32{1, 0},
			b:    []float32{1, 0, 0},
			want: 0.0,
		},
		{
			name: "zero vector",
			a:    []float32{0, 0, 0},
			b:    []float32{1, 0, 0},
			want: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := store.CosineSimilarity(tt.a, tt.b)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("CosineSimilarity(%v, %v) = %f, want %f", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
