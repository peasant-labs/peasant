//go:build experimental

package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/store"
)

// DefaultMaxLessons is the maximum number of lessons to return.
const DefaultMaxLessons = 3

// DefaultMinSimilarity is the minimum cosine similarity threshold.
// Lessons below this threshold are not returned even if fewer than MaxLessons.
const DefaultMinSimilarity = 0.3

// RetrievedLesson is a lesson with its similarity score.
type RetrievedLesson struct {
	Lesson     store.Lesson
	Similarity float64
}

// RetrieveOptions controls retrieval behavior.
type RetrieveOptions struct {
	MaxLessons    int     // 0 → DefaultMaxLessons
	MinSimilarity float64 // 0 → DefaultMinSimilarity
}

// Retrieve finds the top-K most relevant lessons for a given prompt.
// Returns up to opts.MaxLessons lessons above opts.MinSimilarity threshold.
func Retrieve(ctx context.Context, db *store.Store, embedder Embedder, prompt string, opts RetrieveOptions) ([]RetrievedLesson, error) {
	if opts.MaxLessons <= 0 {
		opts.MaxLessons = DefaultMaxLessons
	}
	if opts.MinSimilarity <= 0 {
		opts.MinSimilarity = DefaultMinSimilarity
	}

	// Embed the prompt.
	embeddings, err := embedder.Embed(ctx, []string{prompt})
	if err != nil {
		return nil, fmt.Errorf("embed prompt: %w", err)
	}
	if len(embeddings) == 0 {
		return nil, fmt.Errorf("empty embedding response")
	}
	queryVec := embeddings[0]

	// Load all lessons with embeddings.
	lessons, err := db.LessonsWithEmbeddings(ctx)
	if err != nil {
		return nil, fmt.Errorf("load lessons: %w", err)
	}
	if len(lessons) == 0 {
		return nil, nil
	}

	// Score each lesson.
	scored := make([]RetrievedLesson, 0, len(lessons))
	for _, l := range lessons {
		sim := store.CosineSimilarity(queryVec, l.SituationEmbedding)
		if sim >= opts.MinSimilarity {
			scored = append(scored, RetrievedLesson{Lesson: l, Similarity: sim})
		}
	}

	// Sort by descending similarity.
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].Similarity > scored[j].Similarity
	})

	// Truncate to max.
	if len(scored) > opts.MaxLessons {
		scored = scored[:opts.MaxLessons]
	}

	return scored, nil
}

// FormatLessons renders retrieved lessons as a prepend block for agent context.
func FormatLessons(lessons []RetrievedLesson) string {
	if len(lessons) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Lessons from past sessions\n\n")
	for i, rl := range lessons {
		fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, rl.Lesson.Topic, rl.Lesson.Rule)
		fmt.Fprintf(&b, "   Failure: %s\n\n", rl.Lesson.FailureMode)
	}
	return b.String()
}
