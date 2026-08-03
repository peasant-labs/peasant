//go:build experimental

package memory_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/memory"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
)

// stubEmbedder returns fixed embeddings keyed by input text.
type stubEmbedder struct {
	vectors map[string][]float32
}

func (s *stubEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for i, t := range texts {
		if v, ok := s.vectors[t]; ok {
			result[i] = v
		} else {
			// Default: zero vector
			result[i] = make([]float32, 3)
		}
	}
	return result, nil
}

func seedLessons(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()

	lessons := []struct {
		params    store.CreateLessonParams
		embedding []float32
	}{
		{
			params: store.CreateLessonParams{
				EpisodeAnnotationID: "ann-1",
				SessionID:           "sess-1",
				Topic:               "dependencies",
				Rule:                "Check Python version compatibility.",
				FailureMode:         "torch had no wheel.",
			},
			embedding: []float32{1, 0, 0}, // "deps" direction
		},
		{
			params: store.CreateLessonParams{
				EpisodeAnnotationID: "ann-2",
				SessionID:           "sess-1",
				Topic:               "concurrency",
				Rule:                "Verify methods are async.",
				FailureMode:         "gather() was no-op.",
			},
			embedding: []float32{0, 1, 0}, // "async" direction
		},
		{
			params: store.CreateLessonParams{
				EpisodeAnnotationID: "ann-3",
				SessionID:           "sess-1",
				Topic:               "llm/prompts",
				Rule:                "Account for politeness bias.",
				FailureMode:         "All ratings were 10/10.",
			},
			embedding: []float32{0, 0, 1}, // "llm" direction
		},
	}

	for _, l := range lessons {
		id, _, err := s.CreateLesson(ctx, l.params)
		if err != nil {
			t.Fatalf("CreateLesson: %v", err)
		}
		if err := s.UpdateLessonEmbedding(ctx, id, l.embedding); err != nil {
			t.Fatalf("UpdateLessonEmbedding: %v", err)
		}
	}
}

func TestRetrieve_RelevantLessons(t *testing.T) {
	t.Parallel()
	s := storetest.Open(t)
	seedLessons(t, s)

	// Query vector points toward "deps" direction.
	embedder := &stubEmbedder{
		vectors: map[string][]float32{
			"add numpy to requirements": {0.9, 0.1, 0.0},
		},
	}

	results, err := memory.Retrieve(context.Background(), s, embedder, "add numpy to requirements", memory.RetrieveOptions{
		MaxLessons:    3,
		MinSimilarity: 0.3,
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result (only deps lesson above 0.3), got %d", len(results))
	}
	if results[0].Lesson.Topic != "dependencies" {
		t.Errorf("expected topic 'dependencies', got %q", results[0].Lesson.Topic)
	}
	if results[0].Similarity < 0.3 {
		t.Errorf("similarity %f below threshold", results[0].Similarity)
	}
}

func TestRetrieve_NoRelevantLessons(t *testing.T) {
	t.Parallel()
	s := storetest.Open(t)
	seedLessons(t, s)

	// Query vector orthogonal to all lesson vectors.
	embedder := &stubEmbedder{
		vectors: map[string][]float32{
			"fix CSS styling": {0.0, 0.0, 0.0},
		},
	}

	results, err := memory.Retrieve(context.Background(), s, embedder, "fix CSS styling", memory.RetrieveOptions{
		MaxLessons:    3,
		MinSimilarity: 0.3,
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results for irrelevant query, got %d", len(results))
	}
}

func TestRetrieve_MaxLessonsRespected(t *testing.T) {
	t.Parallel()
	s := storetest.Open(t)
	seedLessons(t, s)

	// Query vector similar to all 3 lessons.
	embedder := &stubEmbedder{
		vectors: map[string][]float32{
			"everything": {0.6, 0.6, 0.6},
		},
	}

	results, err := memory.Retrieve(context.Background(), s, embedder, "everything", memory.RetrieveOptions{
		MaxLessons:    2,
		MinSimilarity: 0.1,
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected max 2 results, got %d", len(results))
	}
}

func TestRetrieve_NoEmbeddedLessons(t *testing.T) {
	t.Parallel()
	s := storetest.Open(t)
	ctx := context.Background()

	// Create lesson without embedding.
	s.CreateLesson(ctx, store.CreateLessonParams{
		EpisodeAnnotationID: "ann-x",
		SessionID:           "sess-x",
		Topic:               "topic",
		Rule:                "rule",
		FailureMode:         "fail",
	})

	embedder := &stubEmbedder{
		vectors: map[string][]float32{
			"anything": {1, 0, 0},
		},
	}

	results, err := memory.Retrieve(context.Background(), s, embedder, "anything", memory.RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results when no lessons have embeddings, got %d", len(results))
	}
}

func TestFormatLessons_Empty(t *testing.T) {
	t.Parallel()
	out := memory.FormatLessons(nil)
	if out != "" {
		t.Errorf("expected empty string for nil lessons, got %q", out)
	}
}

func TestFormatLessons_NonEmpty(t *testing.T) {
	t.Parallel()
	lessons := []memory.RetrievedLesson{
		{
			Lesson: store.Lesson{
				Topic:       "deps",
				Rule:        "Check compatibility.",
				FailureMode: "Build failed.",
			},
			Similarity: 0.85,
		},
	}
	out := memory.FormatLessons(lessons)
	if out == "" {
		t.Fatal("expected non-empty output")
	}
	if !contains(out, "[deps]") {
		t.Error("expected topic tag in output")
	}
	if !contains(out, "Check compatibility.") {
		t.Error("expected rule in output")
	}
	if !contains(out, "Build failed.") {
		t.Error("expected failure mode in output")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
