package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

type capturedGitMetric struct {
	Enabled bool
	Commit  string
	Files   map[string]string
	Result  *ingest.SessionMetrics
}

func captureGitMetric(ctx context.Context, analyzer ingest.GitDiffAnalyzer, input *ingest.MetricInput) (capturedGitMetric, error) {
	captured := capturedGitMetric{Enabled: analyzer != nil}
	if analyzer == nil {
		return captured, nil
	}
	type writtenFile struct{ path, content string }
	var writes []writtenFile
	var startMS int64
	for _, entry := range input.Entries {
		if entry.TimestampMs != nil && (startMS == 0 || *entry.TimestampMs < startMS) {
			startMS = *entry.TimestampMs
		}
		if entry.Depth != 1 || entry.EntryType != ingest.EntryTypeToolUse || entry.ToolInput == nil {
			continue
		}
		var tool struct {
			FilePath  string `json:"file_path"`
			Content   string `json:"content"`
			NewString string `json:"new_string"`
		}
		if json.Unmarshal([]byte(*entry.ToolInput), &tool) != nil {
			continue
		}
		if tool.Content == "" {
			tool.Content = tool.NewString
		}
		if tool.FilePath != "" && tool.Content != "" {
			writes = append(writes, writtenFile{tool.FilePath, tool.Content})
		}
	}
	if len(writes) == 0 {
		return captured, nil
	}
	if input.ProjectPath == "" || startMS == 0 {
		return captured, fmt.Errorf("capture enabled Git survival input: session project path or timestamp is unavailable")
	}
	since := time.UnixMilli(startMS)
	commits, err := analyzer.GetSessionCommits(ctx, input.ProjectPath, since, since.Add(7*24*time.Hour))
	if err != nil {
		return captured, fmt.Errorf("capture Git commits in %s: %w", input.ProjectPath, err)
	}
	if len(commits) == 0 {
		return captured, nil // A successful empty lookup is authoritative.
	}
	captured.Commit = commits[len(commits)-1]
	captured.Files = make(map[string]string)
	files := make(map[string][]byte)
	for _, write := range writes {
		path := write.path
		if filepath.IsAbs(path) {
			path, err = filepath.Rel(input.ProjectPath, path)
			if err != nil {
				return captured, err
			}
		}
		if path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
			return captured, fmt.Errorf("capture Git survival input: written path %s is outside session project %s", write.path, input.ProjectPath)
		}
		if _, seen := files[write.path]; seen {
			continue
		}
		content, err := analyzer.GetFileAtCommit(ctx, input.ProjectPath, path, captured.Commit)
		if err != nil {
			// The existing analyzer cannot distinguish absence from failed I/O.
			// Neither can certify an authoritative zero-survival result.
			return captured, fmt.Errorf("capture Git file %s at %s: %w", path, captured.Commit, err)
		}
		files[write.path] = content
		captured.Files[path] = schema.ComputeTranscriptHash(content)
	}
	var total, survived int
	for _, write := range writes {
		lines := strings.Split(write.content, "\n")
		total += len(lines)
		present := make(map[string]bool)
		for _, line := range strings.Split(string(files[write.path]), "\n") {
			present[line] = true
		}
		for _, line := range lines {
			if present[line] {
				survived++
			}
		}
	}
	pct := float64(survived) / float64(total) * 100
	captured.Result = &ingest.SessionMetrics{QualityMetrics: schema.QualityMetrics{
		M6OutputSurvivalPct: &pct, M6LinesSurvived: &survived, M6LinesTotal: &total,
	}}
	return captured, nil
}
