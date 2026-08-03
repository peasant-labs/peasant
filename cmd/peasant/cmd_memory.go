//go:build experimental

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/memory"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/spf13/cobra"
)

// BuildMemoryCommand constructs the memory command group.
//
// Workflow:
//  1. Work normally (baseline) — sessions ingested automatically
//  2. Annotate friction episodes (peasant annotate)
//  3. Build lessons: peasant memory build --from-file lessons.jsonl
//  4. Inject into eval project: peasant memory inject on --dir /path/to/project
//  5. Work with lessons active, annotate new sessions, compare
//  6. Benchmark: peasant memory augment --prompt "..." | benchmark-runner
func BuildMemoryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Agent memory construction and retrieval (experimental)",
	}
	cmd.AddCommand(
		buildMemoryBuildCommand(),
		buildMemoryEmbedCommand(),
		buildMemoryInjectCommand(),
		buildMemoryRetrieveCommand(),
		buildMemoryAugmentCommand(),
		buildMemoryListCommand(),
		buildMemoryEvalCommand(),
	)
	return cmd
}

// ---------------------------------------------------------------------------
// memory build — import lessons from JSONL + compute embeddings
// ---------------------------------------------------------------------------

func buildMemoryBuildCommand() *cobra.Command {
	var (
		fromFile string
		model    string
	)
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Import lessons from JSONL and compute embeddings",
		Long: `Import pre-extracted lessons from a JSONL file and compute embeddings.
Each line should contain:
  {"annotation_id":"...","session_id":"...","topic":"...","rule":"...","failure_mode":"..."}

Lessons are typically extracted by an LLM pass over annotated friction episodes.
After importing, embeddings are computed via OpenAI text-embedding-3-small.

Requires OPENAI_API_KEY environment variable.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if fromFile == "" {
				return fmt.Errorf("--from-file is required")
			}
			return runMemoryBuild(cmd, fromFile, model)
		},
	}
	cmd.Flags().StringVar(&fromFile, "from-file", "", "JSONL file with extracted lessons")
	cmd.Flags().StringVar(&model, "model", "text-embedding-3-small", "OpenAI embedding model")
	return cmd
}

type extractedLesson struct {
	AnnotationID string `json:"annotation_id"`
	SessionID    string `json:"session_id"`
	Topic        string `json:"topic"`
	Rule         string `json:"rule"`
	FailureMode  string `json:"failure_mode"`
}

func runMemoryBuild(cmd *cobra.Command, path string, embeddingModel string) error {
	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	// Step 1: Import lessons from JSONL.
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	ctx := context.Background()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var imported, duplicates, skipped int
	// Track newly imported lesson IDs so we only embed those, not all
	// un-embedded lessons in the DB (avoids surprise API costs on re-runs).
	newIDs := make(map[string]bool)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var lesson extractedLesson
		if err := json.Unmarshal([]byte(line), &lesson); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: line %d: %v\n", lineNum, err)
			skipped++
			continue
		}
		if lesson.Topic == "" || lesson.Rule == "" || lesson.FailureMode == "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: line %d: missing required fields (topic, rule, failure_mode)\n", lineNum)
			skipped++
			continue
		}

		id, created, createErr := db.CreateLesson(ctx, store.CreateLessonParams{
			EpisodeAnnotationID: lesson.AnnotationID,
			SessionID:           lesson.SessionID,
			Topic:               lesson.Topic,
			Rule:                lesson.Rule,
			FailureMode:         lesson.FailureMode,
		})
		if createErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: line %d: %v\n", lineNum, createErr)
			skipped++
			continue
		}
		if !created {
			// Duplicate: same (topic, rule, failure_mode) already in DB.
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: duplicate lesson: session=%s annotation=%s topic=%s rule=%s failure_mode=%s\n",
				lesson.SessionID, lesson.AnnotationID, lesson.Topic, lesson.Rule, lesson.FailureMode)
			duplicates++
		} else {
			newIDs[id] = true
			imported++
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "imported %d lessons (%d duplicates skipped, %d errors)\n", imported, duplicates, skipped)

	// Step 2: Compute embeddings for newly imported lessons only.
	// Fetch all lessons and filter to those in newIDs that lack embeddings.
	allLessons, err := db.ListLessons(ctx, "")
	if err != nil {
		return err
	}

	var toEmbed []store.Lesson
	for _, l := range allLessons {
		if newIDs[l.ID] && l.SituationEmbedding == nil {
			toEmbed = append(toEmbed, l)
		}
	}
	if len(toEmbed) == 0 {
		if imported > 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "all new lessons already have embeddings")
		}
		// Check for any remaining lessons with NULL embeddings (from prior failed runs).
		unembedded, uErr := db.LessonsWithoutEmbeddings(ctx)
		if uErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not check for unembedded lessons: %v\n", uErr)
		} else if len(unembedded) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %d lessons have no embeddings. Run 'peasant memory embed' to fix.\n", len(unembedded))
		}
		return nil
	}

	embedder, err := memory.NewOpenAIEmbedder(embeddingModel)
	if err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "embedding %d new lessons...\n", len(toEmbed))

	texts := make([]string, len(toEmbed))
	for i, l := range toEmbed {
		texts[i] = fmt.Sprintf("[%s] %s Failure: %s", l.Topic, l.Rule, l.FailureMode)
	}

	const batchSize = 64
	for start := 0; start < len(texts); start += batchSize {
		end := min(start+batchSize, len(texts))
		batch := texts[start:end]
		batchLessons := toEmbed[start:end]

		embeddings, embedErr := embedder.Embed(ctx, batch)
		if embedErr != nil {
			return fmt.Errorf("embed batch starting at %d: %w", start, embedErr)
		}

		for i, emb := range embeddings {
			if err := db.UpdateLessonEmbedding(ctx, batchLessons[i].ID, emb); err != nil {
				return fmt.Errorf("update embedding for %s: %w", batchLessons[i].ID, err)
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  embedded %d/%d\n", end, len(texts))
	}

	fmt.Fprintf(cmd.OutOrStdout(), "done: %d lessons imported, %d embedded\n", imported, len(toEmbed))

	// Check for any remaining lessons with NULL embeddings (from prior failed runs).
	unembedded, uErr := db.LessonsWithoutEmbeddings(ctx)
	if uErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not check for unembedded lessons: %v\n", uErr)
	} else if len(unembedded) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %d lessons have no embeddings. Run 'peasant memory embed' to fix.\n", len(unembedded))
	}
	return nil
}

// ---------------------------------------------------------------------------
// memory embed — retry embedding for lessons with NULL situation_embedding
// ---------------------------------------------------------------------------

func buildMemoryEmbedCommand() *cobra.Command {
	var (
		model string
		yes   bool
	)
	cmd := &cobra.Command{
		Use:   "embed",
		Short: "Embed lessons that are missing embeddings",
		Long: `Retry embedding for lessons that have NULL situation_embedding.

This handles the case where a prior 'memory build' failed partway through
(e.g. missing API key, network error, quota exhausted), leaving some lessons
without embeddings.

Requires OPENAI_API_KEY environment variable.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMemoryEmbed(cmd, model, yes)
		},
	}
	cmd.Flags().StringVar(&model, "model", "text-embedding-3-small", "OpenAI embedding model")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip interactive confirmation")
	return cmd
}

func runMemoryEmbed(cmd *cobra.Command, embeddingModel string, skipConfirm bool) error {
	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx := context.Background()
	lessons, err := db.LessonsWithoutEmbeddings(ctx)
	if err != nil {
		return fmt.Errorf("query unembedded lessons: %w", err)
	}

	if len(lessons) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "all lessons have embeddings")
		return nil
	}

	// Collect deduplicated topics for display.
	topicSeen := make(map[string]bool)
	var topics []string
	for _, l := range lessons {
		if !topicSeen[l.Topic] {
			topicSeen[l.Topic] = true
			topics = append(topics, l.Topic)
		}
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Found %d lessons without embeddings.\n", len(lessons))
	fmt.Fprintln(cmd.OutOrStdout(), "These lessons were imported but not yet embedded, possibly due to a missing API key or failed prior build.")
	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintf(cmd.OutOrStdout(), "Topics: %s\n", strings.Join(topics, ", "))
	fmt.Fprintln(cmd.OutOrStdout())

	if !skipConfirm {
		fmt.Fprint(cmd.OutOrStdout(), "Proceed with embedding? (requires OPENAI_API_KEY) [y/N]: ")
		scanner := bufio.NewScanner(cmd.InOrStdin())
		if !scanner.Scan() || !strings.EqualFold(strings.TrimSpace(scanner.Text()), "y") {
			fmt.Fprintln(cmd.OutOrStdout(), "aborted")
			return nil
		}
	}

	embedder, err := memory.NewOpenAIEmbedder(embeddingModel)
	if err != nil {
		return err
	}

	texts := make([]string, len(lessons))
	for i, l := range lessons {
		texts[i] = fmt.Sprintf("[%s] %s Failure: %s", l.Topic, l.Rule, l.FailureMode)
	}

	const batchSize = 64
	embedded := 0
	for start := 0; start < len(texts); start += batchSize {
		end := min(start+batchSize, len(texts))
		batch := texts[start:end]
		batchLessons := lessons[start:end]

		embeddings, embedErr := embedder.Embed(ctx, batch)
		if embedErr != nil {
			return fmt.Errorf("embed batch starting at %d: %w", start, embedErr)
		}

		for i, emb := range embeddings {
			if err := db.UpdateLessonEmbedding(ctx, batchLessons[i].ID, emb); err != nil {
				return fmt.Errorf("update embedding for %s: %w", batchLessons[i].ID, err)
			}
			embedded++
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  embedded %d/%d\n", end, len(texts))
	}

	fmt.Fprintf(cmd.OutOrStdout(), "done: embedded %d lessons\n", embedded)
	return nil
}

// ---------------------------------------------------------------------------
// memory inject on/off — enable/disable lesson injection in a project
// ---------------------------------------------------------------------------

// hookConfig is the JSON structure for the UserPromptSubmit hook.
func makeHookConfig() []map[string]any {
	// Use the absolute path of the current binary so the hook works
	// regardless of PATH in the target project's shell context.
	bin, err := os.Executable()
	if err != nil {
		bin = "peasant" // fallback to PATH
	} else {
		bin, _ = filepath.EvalSymlinks(bin)
	}
	return []map[string]any{
		{
			"hooks": []map[string]any{
				{
					"type":          "command",
					"command":       bin + " memory retrieve",
					"timeout":       15,
					"statusMessage": "Retrieving lessons...",
				},
			},
		},
	}
}

func buildMemoryInjectCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inject",
		Short: "Control lesson injection in a project",
		Long: `Enable or disable lesson injection for a specific project.

  peasant memory inject on  --dir /path/to/project   # start injecting lessons
  peasant memory inject off --dir /path/to/project   # stop injecting lessons

This writes a UserPromptSubmit hook to the project's .claude/settings.local.json.
When enabled, each message in a Claude Code session will have relevant lessons
prepended to the agent's context.`,
	}
	cmd.AddCommand(
		buildInjectOnCommand(),
		buildInjectOffCommand(),
	)
	return cmd
}

func buildInjectOnCommand() *cobra.Command {
	var targetDir string
	cmd := &cobra.Command{
		Use:   "on",
		Short: "Enable lesson injection in a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInjectOn(cmd, targetDir)
		},
	}
	cmd.Flags().StringVar(&targetDir, "dir", ".", "Target project directory")
	return cmd
}

func runInjectOn(cmd *cobra.Command, dir string) error {
	settingsPath := dir + "/.claude/settings.local.json"

	var settings map[string]any
	data, err := os.ReadFile(settingsPath)
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			return fmt.Errorf("parse existing settings: %w", err)
		}
	} else {
		settings = make(map[string]any)
	}

	// Check if hook already exists.
	if hooks, ok := settings["hooks"].(map[string]any); ok {
		if _, hasUPS := hooks["UserPromptSubmit"]; hasUPS {
			fmt.Fprintln(cmd.OutOrStdout(), "lesson injection already enabled — skipping")
			return nil
		}
	}

	// Add hooks section.
	existingHooks, isMap := settings["hooks"].(map[string]any)
	if !isMap {
		existingHooks = make(map[string]any)
	}
	existingHooks["UserPromptSubmit"] = makeHookConfig()
	settings["hooks"] = existingHooks

	if err := os.MkdirAll(dir+"/.claude", 0o755); err != nil {
		return fmt.Errorf("create .claude directory: %w", err)
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}

	// Log the event to the DB for eval tracking.
	absDir, _ := filepath.Abs(dir)
	if db, cleanup, dbErr := openDB(cmd); dbErr == nil {
		defer cleanup()
		db.LogInjectionEvent(context.Background(), absDir, "on")
	}

	fmt.Fprintf(cmd.OutOrStdout(), "lesson injection enabled in %s\n", settingsPath)
	fmt.Fprintln(cmd.OutOrStdout(), "new Claude Code sessions in this project will have relevant lessons prepended.")
	return nil
}

func buildInjectOffCommand() *cobra.Command {
	var targetDir string
	cmd := &cobra.Command{
		Use:   "off",
		Short: "Disable lesson injection in a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInjectOff(cmd, targetDir)
		},
	}
	cmd.Flags().StringVar(&targetDir, "dir", ".", "Target project directory")
	return cmd
}

func runInjectOff(cmd *cobra.Command, dir string) error {
	settingsPath := dir + "/.claude/settings.local.json"

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		fmt.Fprintln(cmd.OutOrStdout(), "no settings file found — nothing to disable")
		return nil
	}

	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("parse settings: %w", err)
	}

	hooks, isMap := settings["hooks"].(map[string]any)
	if !isMap {
		fmt.Fprintln(cmd.OutOrStdout(), "no hooks configured — lesson injection not active")
		return nil
	}

	if _, hasUPS := hooks["UserPromptSubmit"]; !hasUPS {
		fmt.Fprintln(cmd.OutOrStdout(), "lesson injection not active — nothing to disable")
		return nil
	}

	delete(hooks, "UserPromptSubmit")
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}

	// Log the event to the DB for eval tracking.
	absDir, _ := filepath.Abs(dir)
	if db, cleanup, dbErr := openDB(cmd); dbErr == nil {
		defer cleanup()
		db.LogInjectionEvent(context.Background(), absDir, "off")
	}

	fmt.Fprintf(cmd.OutOrStdout(), "lesson injection disabled in %s\n", settingsPath)
	return nil
}

// ---------------------------------------------------------------------------
// memory retrieve — find relevant lessons for a prompt (used by hooks)
// ---------------------------------------------------------------------------

func buildMemoryRetrieveCommand() *cobra.Command {
	var (
		prompt        string
		maxLessons    int
		minSimilarity float64
		outputJSON    bool
	)
	cmd := &cobra.Command{
		Use:   "retrieve",
		Short: "Retrieve relevant lessons for a prompt",
		Long: `Retrieve the most relevant lessons for a given prompt text.
Reads from --prompt flag or stdin (for use in hooks).

Output is formatted lesson text suitable for prepending to agent context.
Use --output-json for structured output.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMemoryRetrieve(cmd, prompt, maxLessons, minSimilarity, outputJSON)
		},
	}
	cmd.Flags().StringVar(&prompt, "prompt", "", "Prompt text to match against lessons")
	cmd.Flags().IntVar(&maxLessons, "max", memory.DefaultMaxLessons, "Maximum lessons to return")
	cmd.Flags().Float64Var(&minSimilarity, "min-similarity", memory.DefaultMinSimilarity, "Minimum cosine similarity threshold")
	cmd.Flags().BoolVar(&outputJSON, "output-json", false, "Output as JSON")
	return cmd
}

func runMemoryRetrieve(cmd *cobra.Command, prompt string, maxLessons int, minSimilarity float64, outputJSON bool) error {
	if prompt == "" {
		scanner := bufio.NewScanner(cmd.InOrStdin())
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		if scanner.Scan() {
			var hookInput struct {
				Prompt string `json:"prompt"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &hookInput); err == nil && hookInput.Prompt != "" {
				prompt = hookInput.Prompt
			} else {
				prompt = scanner.Text()
			}
		}
		if prompt == "" {
			return fmt.Errorf("no prompt provided (use --prompt flag or pipe to stdin)")
		}
	}

	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	embedder, err := memory.NewOpenAIEmbedder("")
	if err != nil {
		return err
	}

	ctx := context.Background()
	results, err := memory.Retrieve(ctx, db, embedder, prompt, memory.RetrieveOptions{
		MaxLessons:    maxLessons,
		MinSimilarity: minSimilarity,
	})
	if err != nil {
		return err
	}

	if outputJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	}

	text := memory.FormatLessons(results)
	if text == "" {
		return nil
	}
	fmt.Fprint(cmd.OutOrStdout(), text)
	return nil
}

// ---------------------------------------------------------------------------
// memory augment — prepend lessons to a prompt (for benchmark harnesses)
// ---------------------------------------------------------------------------

func buildMemoryAugmentCommand() *cobra.Command {
	var (
		prompt        string
		maxLessons    int
		minSimilarity float64
	)
	cmd := &cobra.Command{
		Use:   "augment",
		Short: "Augment a prompt with relevant lessons (for benchmarks)",
		Long: `Takes a prompt and outputs it with relevant lessons prepended.
Designed for piping into benchmark harnesses like SWE-bench.

Example:
  peasant memory augment --prompt "Fix the bug in parser.py" | benchmark-runner`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMemoryAugment(cmd, prompt, maxLessons, minSimilarity)
		},
	}
	cmd.Flags().StringVar(&prompt, "prompt", "", "Prompt to augment")
	cmd.Flags().IntVar(&maxLessons, "max", memory.DefaultMaxLessons, "Maximum lessons to prepend")
	cmd.Flags().Float64Var(&minSimilarity, "min-similarity", memory.DefaultMinSimilarity, "Minimum cosine similarity threshold")
	return cmd
}

func runMemoryAugment(cmd *cobra.Command, prompt string, maxLessons int, minSimilarity float64) error {
	if prompt == "" {
		scanner := bufio.NewScanner(cmd.InOrStdin())
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		var lines []string
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		prompt = strings.Join(lines, "\n")
	}
	if prompt == "" {
		return fmt.Errorf("no prompt provided (use --prompt or pipe to stdin)")
	}

	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	embedder, err := memory.NewOpenAIEmbedder("")
	if err != nil {
		return err
	}

	ctx := context.Background()
	results, err := memory.Retrieve(ctx, db, embedder, prompt, memory.RetrieveOptions{
		MaxLessons:    maxLessons,
		MinSimilarity: minSimilarity,
	})
	if err != nil {
		return err
	}

	lessons := memory.FormatLessons(results)
	if lessons != "" {
		fmt.Fprint(cmd.OutOrStdout(), lessons)
		fmt.Fprintln(cmd.OutOrStdout(), "---")
		fmt.Fprintln(cmd.OutOrStdout())
	}
	fmt.Fprint(cmd.OutOrStdout(), prompt)
	return nil
}

// ---------------------------------------------------------------------------
// memory list — show stored lessons
// ---------------------------------------------------------------------------

func buildMemoryListCommand() *cobra.Command {
	var sessionID string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List stored lessons",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMemoryList(cmd, sessionID)
		},
	}
	cmd.Flags().StringVar(&sessionID, "session", "", "Filter by session ID")
	return cmd
}

func runMemoryList(cmd *cobra.Command, sessionID string) error {
	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx := context.Background()
	lessons, err := db.ListLessons(ctx, sessionID)
	if err != nil {
		return err
	}

	if len(lessons) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no lessons found")
		return nil
	}

	for i, l := range lessons {
		hasEmb := "no"
		if l.SituationEmbedding != nil {
			hasEmb = "yes"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%d. [%s] %s\n   Failure: %s\n   Session: %s | Embedded: %s\n\n",
			i+1, l.Topic, l.Rule, l.FailureMode, l.SessionID, hasEmb)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "total: %d lessons\n", len(lessons))
	return nil
}

// ---------------------------------------------------------------------------
// memory eval — compare friction rates between baseline and treatment periods
// ---------------------------------------------------------------------------

func buildMemoryEvalCommand() *cobra.Command {
	var (
		cutoffDate  string
		byInjection bool
		annotator   string
		outputJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Compare friction rates between baseline and treatment periods",
		Long: `Split annotated sessions and compare friction episode rates.

Two modes:
  --cutoff YYYY-MM-DD    Split by date (before = baseline, after = treatment)
  --by-injection         Split by whether lesson injection was active
                         (uses inject on/off log to determine which sessions
                         received lessons)

Example:
  peasant memory eval --cutoff 2026-04-10
  peasant memory eval --by-injection
  peasant memory eval --by-injection --output-json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !byInjection && cutoffDate == "" {
				return fmt.Errorf("either --cutoff or --by-injection is required")
			}
			return runMemoryEval(cmd, cutoffDate, byInjection, annotator, outputJSON)
		},
	}
	cmd.Flags().StringVar(&cutoffDate, "cutoff", "", "Date cutoff (YYYY-MM-DD) separating baseline from treatment")
	cmd.Flags().BoolVar(&byInjection, "by-injection", false, "Split by injection status (uses inject on/off log)")
	cmd.Flags().StringVar(&annotator, "annotator", "llm-judge", "Annotator name prefix to filter by")
	cmd.Flags().BoolVar(&outputJSON, "output-json", false, "Output as JSON")
	return cmd
}

type evalResult struct {
	Baseline  periodStats `json:"baseline"`
	Treatment periodStats `json:"treatment"`
}

type periodStats struct {
	Sessions     int                `json:"sessions"`
	Episodes     int                `json:"episodes"`
	FrictionRate float64            `json:"friction_rate"`
	ByType       map[string]float64 `json:"by_type"`
}

func runMemoryEval(cmd *cobra.Command, cutoffDate string, byInjection bool, annotator string, outputJSON bool) error {
	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx := context.Background()
	var baseline, treatment *store.FrictionStats
	var header string

	if byInjection {
		baseline, treatment, err = db.FrictionStatsByInjection(ctx, annotator)
		if err != nil {
			return fmt.Errorf("injection stats: %w", err)
		}
		header = fmt.Sprintf("Split: by injection | Annotator: %s*", annotator)
	} else {
		t, parseErr := parseDate(cutoffDate)
		if parseErr != nil {
			return fmt.Errorf("invalid cutoff date %q: %w (expected YYYY-MM-DD)", cutoffDate, parseErr)
		}
		cutoffMs := t.UnixMilli()
		baseline, err = db.FrictionStatsBefore(ctx, cutoffMs, annotator)
		if err != nil {
			return fmt.Errorf("baseline stats: %w", err)
		}
		treatment, err = db.FrictionStatsAfter(ctx, cutoffMs, annotator)
		if err != nil {
			return fmt.Errorf("treatment stats: %w", err)
		}
		header = fmt.Sprintf("Cutoff: %s | Annotator: %s*", cutoffDate, annotator)
	}

	if outputJSON {
		result := evalResult{
			Baseline:  toPeriodStats(baseline),
			Treatment: toPeriodStats(treatment),
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	// Table output.
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s\n\n", header)
	fmt.Fprintf(out, "%-20s %10s %10s %10s\n", "", "Baseline", "Treatment", "Delta")
	fmt.Fprintf(out, "%-20s %10s %10s %10s\n", "", "--------", "---------", "-----")

	fmt.Fprintf(out, "%-20s %10d %10d\n", "Sessions", baseline.SessionCount, treatment.SessionCount)
	fmt.Fprintf(out, "%-20s %10d %10d\n", "Episodes", baseline.EpisodeCount, treatment.EpisodeCount)

	bRate := rate(baseline.EpisodeCount, baseline.SessionCount)
	tRate := rate(treatment.EpisodeCount, treatment.SessionCount)
	fmt.Fprintf(out, "%-20s %10s %10s %10s\n", "Friction rate",
		fmtRate(bRate), fmtRate(tRate), fmtDelta(bRate, tRate))

	// Per-type breakdown.
	types := []string{"bad_handoff", "bad_output", "bad_process"}
	for _, typ := range types {
		bCount := baseline.ByType[typ]
		tCount := treatment.ByType[typ]
		br := rate(bCount, baseline.SessionCount)
		tr := rate(tCount, treatment.SessionCount)
		fmt.Fprintf(out, "  %-18s %10s %10s %10s\n", typ,
			fmtRate(br), fmtRate(tr), fmtDelta(br, tr))
	}

	if baseline.SessionCount == 0 && treatment.SessionCount == 0 {
		fmt.Fprintln(out, "\nNo annotated sessions found. Run annotations first.")
	}

	return nil
}

func parseDate(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}

func toPeriodStats(s *store.FrictionStats) periodStats {
	ps := periodStats{
		Sessions:     s.SessionCount,
		Episodes:     s.EpisodeCount,
		FrictionRate: rate(s.EpisodeCount, s.SessionCount),
		ByType:       make(map[string]float64),
	}
	for k, v := range s.ByType {
		ps.ByType[k] = rate(v, s.SessionCount)
	}
	return ps
}

func rate(episodes, sessions int) float64 {
	if sessions == 0 {
		return 0
	}
	return float64(episodes) / float64(sessions)
}

func fmtRate(r float64) string {
	if r == 0 {
		return "0"
	}
	return fmt.Sprintf("%.2f/s", r)
}

func fmtDelta(baseline, treatment float64) string {
	if baseline == 0 {
		if treatment == 0 {
			return "-"
		}
		return "+∞"
	}
	pct := ((treatment - baseline) / baseline) * 100
	if pct > 0 {
		return fmt.Sprintf("+%.1f%%", pct)
	}
	return fmt.Sprintf("%.1f%%", pct)
}
