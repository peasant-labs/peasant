package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// classifyScope is a ClassifierFunc that infers the breadth and nature of a session's
// work scope by analysing file paths in tool call arguments.
//
// Strategy:
//  1. Extract file paths from depth=1 tool_use entries (file_path, notebook_path keys in ToolInput JSON).
//  2. Collect the parent directory of each path.
//  3. Walk up to find the longest common directory prefix across all paths.
//  4. Return that prefix as the scope value.
//
// Returns nil if no file paths are found (insufficient data to determine scope).
func classifyScope(
	_ context.Context,
	_ ingest.SessionID,
	entries []schema.SessionEntry,
	_ *ingest.SessionMetrics,
) *ClassifierResult {
	if len(entries) == 0 {
		return nil
	}

	// Collect unique file paths from tool_use entries.
	var dirs []string
	fileCount := 0

	for i := range entries {
		if entries[i].Depth != 1 || entries[i].EntryType != ingest.EntryTypeToolUse || entries[i].ToolInput == nil {
			continue
		}

		var parsed map[string]any
		if err := json.Unmarshal([]byte(*entries[i].ToolInput), &parsed); err != nil {
			continue
		}

		filePath, _ := parsed["file_path"].(string)
		if filePath == "" {
			filePath, _ = parsed["notebook_path"].(string)
		}
		if filePath == "" {
			continue
		}

		fileCount++
		dir := filepath.Dir(filepath.Clean(filePath))
		if dir != "" && dir != "." && dir != "/" {
			dirs = append(dirs, dir)
		}
	}

	if fileCount == 0 || len(dirs) == 0 {
		return nil
	}

	var scope, reason, signal string
	prefix := longestCommonPrefix(dirs)
	if prefix == "" || prefix == "." || prefix == "/" {
		scope = mostFrequentDir(dirs)
		reason = fmt.Sprintf("most frequent directory %q across %d file paths", scope, fileCount)
		signal = "file_path_most_frequent"
	} else {
		scope = prefix
		reason = fmt.Sprintf("common directory prefix %q across %d file paths", scope, fileCount)
		signal = "file_path_prefix"
	}

	if scope == "" {
		return nil
	}

	return &ClassifierResult{
		TypeID:     typeIDSessionScope,
		Value:      scope,
		Confidence: scopeConfidence(len(dirs), fileCount),
		Reason:     reason,
		Provenance: &schema.Provenance{
			Method:  "heuristic",
			Version: "1",
			Details: map[string]string{
				"signal":     signal,
				"file_count": fmt.Sprintf("%d", fileCount),
			},
		},
	}
}

// longestCommonPrefix finds the longest common directory prefix across all paths.
// Returns "" if no common prefix exists beyond root.
func longestCommonPrefix(dirs []string) string {
	if len(dirs) == 0 {
		return ""
	}
	if len(dirs) == 1 {
		return dirs[0]
	}

	// Split the first path into components for comparison.
	refParts := strings.Split(dirs[0], "/")

	for _, dir := range dirs[1:] {
		parts := strings.Split(dir, "/")
		// Shrink refParts to the common length.
		minLen := min(len(refParts), len(parts))
		match := 0
		for j := range minLen {
			if refParts[j] != parts[j] {
				break
			}
			match++
		}
		refParts = refParts[:match]
	}

	if len(refParts) == 0 {
		return ""
	}

	result := strings.Join(refParts, "/")
	// Restore leading slash for absolute paths.
	if strings.HasPrefix(dirs[0], "/") && !strings.HasPrefix(result, "/") {
		result = "/" + result
	}
	return result
}

// mostFrequentDir returns the directory with the highest count.
// Ties are broken alphabetically for deterministic output.
func mostFrequentDir(dirs []string) string {
	counts := make(map[string]int)
	for _, d := range dirs {
		counts[d]++
	}

	best := ""
	bestCount := 0
	for d, c := range counts {
		if c > bestCount || (c == bestCount && d < best) {
			best = d
			bestCount = c
		}
	}
	return best
}

// scopeConfidence returns a confidence score based on the proportion of file paths
// that contributed directory signals.
func scopeConfidence(dirsWithPrefix, totalFiles int) float64 {
	if totalFiles == 0 {
		return 0
	}
	ratio := float64(dirsWithPrefix) / float64(totalFiles)
	// Scale: 60% base + up to 35% from signal coverage.
	return 0.6 + ratio*0.35
}
