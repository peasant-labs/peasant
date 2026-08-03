package api

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/peasant-labs/schema"
)

// sessionFileRecord holds per-session, per-file interaction data
// accumulated during file extraction.
type sessionFileRecord struct {
	FilePath    string
	Interaction schema.InteractionType
	TurnCount   int
	HumanTurns  int
}

// extractFilePaths scans session entries for file paths referenced in tool calls.
// Returns a map of file_path → sessionFileRecord with interaction classification.
func extractFilePaths(entries []schema.SessionEntry) map[string]*sessionFileRecord {
	records := make(map[string]*sessionFileRecord)

	// Pass 1: Extract files from tool_use entries.
	for _, e := range entries {
		if e.Depth == 1 && e.EntryType == schema.EntryTypeToolUse && e.ToolInput != nil {
			paths := extractFilePathsFromToolInput(*e.ToolInput)
			for _, fp := range paths {
				if fp == "" {
					continue
				}
				rec, exists := records[fp]
				if !exists {
					rec = &sessionFileRecord{
						FilePath:    fp,
						Interaction: schema.InteractionMentioned,
					}
					records[fp] = rec
				}
				rec.TurnCount++

				// Classify based on tool kind.
				if e.ToolKind != nil {
					kind := *e.ToolKind
					if kind == schema.ToolCallKindRead || kind == schema.ToolCallKindEdit {
						if rec.Interaction == schema.InteractionMentioned {
							rec.Interaction = schema.InteractionRead
						}
					}
				}
			}
		}

		// Depth=0 tool_use entries (backward compat).
		if e.Depth == 0 && e.EntryType == schema.EntryTypeToolUse && e.ToolInput != nil {
			paths := extractFilePathsFromToolInput(*e.ToolInput)
			for _, fp := range paths {
				if fp == "" {
					continue
				}
				rec, exists := records[fp]
				if !exists {
					rec = &sessionFileRecord{
						FilePath:    fp,
						Interaction: schema.InteractionMentioned,
					}
					records[fp] = rec
				}
				rec.TurnCount++

				if e.ToolKind != nil {
					kind := *e.ToolKind
					if kind == schema.ToolCallKindRead || kind == schema.ToolCallKindEdit {
						if rec.Interaction == schema.InteractionMentioned {
							rec.Interaction = schema.InteractionRead
						}
					}
				}
			}
		}
	}

	// Pass 2: Scan user messages for file path mentions to upgrade to discussed/questioned.
	for _, e := range entries {
		if e.Depth == 0 && e.Role == schema.RoleUser && e.ContentPreview != nil {
			content := *e.ContentPreview
			for fp, rec := range records {
				if strings.Contains(content, fp) || strings.Contains(content, filepath.Base(fp)) {
					rec.HumanTurns++
				}
			}
		}
	}

	// Pass 3: Count assistant turns referencing each file to detect "discussed".
	for _, e := range entries {
		if e.Depth == 0 && e.Role == schema.RoleAssistant && e.ContentPreview != nil {
			content := *e.ContentPreview
			for fp, rec := range records {
				if strings.Contains(content, fp) || strings.Contains(content, filepath.Base(fp)) {
					// If assistant discussed this file 2+ times, upgrade to discussed.
					if rec.Interaction == schema.InteractionRead || rec.Interaction == schema.InteractionMentioned {
						rec.Interaction = schema.InteractionDiscussed
					}
				}
			}
		}
	}

	// Pass 4: Upgrade to "questioned" if human asked about the file.
	for _, rec := range records {
		if rec.HumanTurns >= 1 && (rec.Interaction == schema.InteractionRead || rec.Interaction == schema.InteractionDiscussed) {
			rec.Interaction = schema.InteractionQuestioned
		}
	}

	return records
}

// extractFilePathsFromToolInput parses tool input JSON for file paths.
// Checks keys: file_path, notebook_path, path, and pattern (for Glob/Grep).
func extractFilePathsFromToolInput(toolInput string) []string {
	var args map[string]any
	if err := json.Unmarshal([]byte(toolInput), &args); err != nil {
		return nil
	}

	var paths []string
	for _, key := range []string{"file_path", "notebook_path", "path"} {
		if v, ok := args[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				paths = append(paths, s)
			}
		}
	}

	// For Glob/Grep tool calls, the "pattern" field may contain a file path.
	// Only include if it looks like a file path (contains a slash or dot).
	if v, ok := args["pattern"]; ok {
		if s, ok := v.(string); ok && s != "" && (strings.Contains(s, "/") || strings.Contains(s, ".")) {
			// Skip glob patterns — only real file paths.
			if !strings.ContainsAny(s, "*?[") {
				paths = append(paths, s)
			}
		}
	}

	return paths
}

// computeFamiliarityDepth computes 0-3 depth from aggregated file data.
//
//	0 = file exists but never appeared in any session
//	1 = mentioned or read (interaction = mentioned|read)
//	2 = discussed (interaction = discussed, OR total_human_turns >= 2)
//	3 = questioned across 2+ sessions OR total_human_turns >= 5
func computeFamiliarityDepth(sessionCount, totalTurns, totalHumanTurns int, maxInteraction schema.InteractionType) int {
	if sessionCount == 0 {
		return 0
	}

	if totalHumanTurns >= 5 {
		return 3
	}

	if sessionCount >= 2 && maxInteraction == schema.InteractionQuestioned {
		return 3
	}

	if totalHumanTurns >= 2 || maxInteraction == schema.InteractionDiscussed || maxInteraction == schema.InteractionQuestioned {
		return 2
	}

	return 1
}

// computeDecayLevel returns a DecayLevel based on days since last engagement.
func computeDecayLevel(lastEngagedAt *string, now time.Time) schema.DecayLevel {
	if lastEngagedAt == nil {
		return schema.DecayUnexplored
	}

	t, err := time.Parse(time.RFC3339, *lastEngagedAt)
	if err != nil {
		return schema.DecayUnexplored
	}

	days := int(now.Sub(t).Hours() / 24)
	if days < 7 {
		return schema.DecayFresh
	}
	if days <= 30 {
		return schema.DecayFading
	}
	return schema.DecayStale
}

// daysSince returns the number of days between the given ISO 8601 time and now.
func daysSince(lastEngagedAt *string, now time.Time) *int {
	if lastEngagedAt == nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *lastEngagedAt)
	if err != nil {
		return nil
	}
	d := int(now.Sub(t).Hours() / 24)
	return &d
}

// isSourceFile returns true if the file path represents a source file
// (not generated, vendored, or lock files).
func isSourceFile(path string) bool {
	// Exclude common non-source directories.
	excludeDirs := []string{"node_modules", "vendor", ".git", "__pycache__", ".cache", "dist", "build"}
	for _, dir := range excludeDirs {
		if strings.Contains(path, dir+"/") || strings.HasPrefix(path, dir+"/") {
			return false
		}
	}

	// Exclude lock/sum/generated files.
	excludeSuffixes := []string{
		".lock", ".sum", ".mod",
		"package-lock.json", "yarn.lock", "pnpm-lock.yaml",
		".min.js", ".min.css", ".map",
	}
	lower := strings.ToLower(filepath.Base(path))
	for _, suffix := range excludeSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return false
		}
	}

	return true
}

// buildWalkthroughTrail extracts a trail from a session's entries.
func buildWalkthroughTrail(sessionID, title, date string, entries []schema.SessionEntry) *WalkthroughTrail {
	var steps []WalkthroughStep
	uniqueFiles := make(map[string]bool)
	totalRefs := 0

	for _, e := range entries {
		if e.ToolInput == nil {
			continue
		}
		if e.EntryType != schema.EntryTypeToolUse {
			continue
		}

		paths := extractFilePathsFromToolInput(*e.ToolInput)
		for _, fp := range paths {
			if fp == "" {
				continue
			}
			totalRefs++
			uniqueFiles[fp] = true

			// Extract line number if available.
			var line *int
			var args map[string]any
			if err := json.Unmarshal([]byte(*e.ToolInput), &args); err == nil {
				if v, ok := args["offset"]; ok {
					if f, ok := v.(float64); ok {
						l := int(f)
						line = &l
					}
				}
				if v, ok := args["line"]; ok {
					if f, ok := v.(float64); ok {
						l := int(f)
						line = &l
					}
				}
			}

			excerpt := ""
			if e.ToolNamesCSV != nil {
				excerpt = *e.ToolNamesCSV
			}

			steps = append(steps, WalkthroughStep{
				File:    fp,
				Line:    line,
				Excerpt: excerpt,
			})
		}
	}

	if len(steps) == 0 {
		return nil
	}

	// Coherence: if unique files / total references > 0.5, it's a linear walkthrough.
	coherent := totalRefs > 0 && float64(len(uniqueFiles))/float64(totalRefs) > 0.5

	turnCount := 0
	for _, e := range entries {
		if e.Depth == 0 {
			turnCount++
		}
	}

	return &WalkthroughTrail{
		SessionID:  sessionID,
		Title:      title,
		Date:       date,
		TurnCount:  turnCount,
		Steps:      steps,
		IsCoherent: coherent,
	}
}

// buildReviewSuggestions finds stale files with depth >= 2 and generates prompts.
func buildReviewSuggestions(files []FileFamiliarity) []ReviewSuggestion {
	var suggestions []ReviewSuggestion
	for _, f := range files {
		if f.DecayLevel == schema.DecayStale && f.Depth >= 2 && f.LastEngagedAt != nil && f.DaysSince != nil {
			suggestions = append(suggestions, ReviewSuggestion{
				Path:        f.Path,
				LastEngaged: *f.LastEngagedAt,
				DaysSince:   *f.DaysSince,
				SuggestedPrompt: fmt.Sprintf(
					"Review %s — it was last discussed %d days ago. What has changed since then?",
					filepath.Base(f.Path), *f.DaysSince,
				),
			})
		}
	}
	// Cap suggestions at 5.
	if len(suggestions) > 5 {
		suggestions = suggestions[:5]
	}
	return suggestions
}

// computeFamiliarityPct returns the percentage of source files that have been engaged.
func computeFamiliarityPct(files []FileFamiliarity) float64 {
	var sourceCount, engagedCount int
	for _, f := range files {
		if f.IsSourceFile {
			sourceCount++
			if f.Depth > 0 {
				engagedCount++
			}
		}
	}
	if sourceCount == 0 {
		return 0
	}
	pct := float64(engagedCount) / float64(sourceCount) * 100
	return math.Round(pct*10) / 10
}

// computeUnexploredCount returns the number of source files with 0 engagement.
func computeUnexploredCount(files []FileFamiliarity) int {
	count := 0
	for _, f := range files {
		if f.IsSourceFile && f.Depth == 0 {
			count++
		}
	}
	return count
}

// computeFreshnessDays returns the days since the most recent engagement across all files.
func computeFreshnessDays(files []FileFamiliarity, now time.Time) *int {
	var latest *time.Time
	for _, f := range files {
		if f.LastEngagedAt != nil {
			t, err := time.Parse(time.RFC3339, *f.LastEngagedAt)
			if err == nil {
				if latest == nil || t.After(*latest) {
					latest = &t
				}
			}
		}
	}
	if latest == nil {
		return nil
	}
	d := int(now.Sub(*latest).Hours() / 24)
	return &d
}
