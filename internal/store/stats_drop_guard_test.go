package store_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The stats drop guard: every read of a retired session_metrics measurement
// column must appear only in an inventoried site. Native sessions read these
// quantities from session_captured_stats; the legacy homes stay only where a
// non-native session or a retired writer still needs them, and each of those
// sites is named below. Adding a new consumer outside the inventory fails
// here before it can anchor the next release's column drop.
//
// Scope: non-test Go sources under the module. Shipped DDL and migration
// history are excluded (immutable); test seeds and fixtures are excluded
// (scaffolding, not consumers). What remains is production code that runs.
func TestDeprecatedStatsColumnsGuarded(t *testing.T) {
	root := moduleRoot(t)
	deprecated := []*regexp.Regexp{
		regexp.MustCompile(`\bm\.turn_count\b`),
		regexp.MustCompile(`\bm\.tool_calls\b`),
		regexp.MustCompile(`\bm\.output_tokens\b`),
		regexp.MustCompile(`\bm\.duration_minutes\b`),
		regexp.MustCompile(`\bm\.subagent_count\b`),
		regexp.MustCompile(`\bmetric_seed_json\b`),
	}
	// Each entry names one surviving reference: the file that holds it and a
	// substring the offending line must contain. Lines carrying a dual-path
	// CASE (active_generation_id IS NOT NULL) pass without an entry.
	allowed := []allowEntry{
		// The legacy seed branch serves non-native sessions only; native
		// sessions read the harness-only seed home.
		{file: "internal/store/metric_seeds.go", contains: "SELECT metric_seed_json FROM sessions", why: "legacy non-native seed branch"},
		// The ingest upsert keeps its column shape; the value is nulled for
		// sessions that already own an active generation.
		{file: "internal/store/writer.go", contains: "metric_seed_json, source_fingerprint", why: "ingest upsert column shape"},
		{file: "internal/store/writer.go", contains: "metric_seed_json = excluded.metric_seed_json", why: "ingest conflict update, value-gated to legacy"},
		// The metrics placeholder keeps its shape; measurement values are
		// gated to sessions without an active generation.
		{file: "internal/store/writer.go", contains: "session_id, turn_count, subagent_count,", why: "placeholder seeding shape"},
		// The COMPUTE save keeps the full analysis row shape; moved
		// measurements bind NULL for native sessions.
		{file: "internal/store/metrics_writer.go", contains: "session_id, turn_count, subagent_count,", why: "analysis record shape"},
		// The retirement clear itself.
		{file: "internal/store/harmonized_stats.go", contains: "UPDATE sessions SET metric_seed_json = NULL", why: "retired seed clear for natives"},
		{file: "internal/store/harmonized_stats.go", contains: "SELECT model_harness, metric_seed_json FROM sessions", why: "file-backed refresh preserves unknown prior legacy seed keys before cutover replaces its authority"},
		// The current activation's legacy mirrors; the activation commit
		// records them alongside the generation rows.
		{file: "internal/store/index_format_v2.go", contains: "INSERT INTO session_metrics (session_id, turn_count, tool_calls, title)", why: "activation legacy mirrors"},
	}
	var violations []string
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "third_party", "testdata", "web", "node_modules", ".beads":
				return filepath.SkipDir
			}
			// A nested checkout (a linked worktree or a submodule) carries its
			// own copy of the module; scanning it reports the same files twice
			// and reads stale branches. Skip any directory that is itself a
			// repository root, except the module root we were asked to scan.
			if path != root {
				if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if isShippedSchemaFile(rel) {
			return nil
		}
		for _, hit := range deprecatedHits(path, deprecated) {
			if strings.Contains(hit.line, "active_generation_id IS NOT NULL") {
				continue
			}
			if allowlisted(rel, hit.line, allowed) {
				continue
			}
			violations = append(violations, rel+":"+hit.desc)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk module: %v", walkErr)
	}
	if len(violations) > 0 {
		t.Fatalf("found %d uninventoried retired-column reference(s):\n%s\nMove the reader to session_captured_stats (natives) or name the legacy site in the allowlist.",
			len(violations), strings.Join(violations, "\n"))
	}
}

// allowEntry names one surviving retired-column reference.
type allowEntry struct {
	file     string
	contains string
	why      string
}

// isShippedSchemaFile reports the immutable DDL and migration history the
// guard does not police: definitions, not consumers.
func isShippedSchemaFile(rel string) bool {
	base := filepath.Base(rel)
	if filepath.Dir(rel) != "internal/store" {
		return false
	}
	baseSchema := base == "schema.go" || base == "migrations.go"
	baselineSchema := base == "baseline.go" || base == "baseline_fresh.go"
	if baseSchema || baselineSchema {
		return true
	}
	versionedSchema := strings.HasPrefix(base, "schema_v") || strings.HasPrefix(base, "migration_v")
	migrationSchema := versionedSchema || strings.HasPrefix(base, "migration_")
	if strings.HasSuffix(base, ".go") && migrationSchema {
		return true
	}
	return false
}

type deprecatedHit struct {
	line string
	desc string
}

// deprecatedHits returns the code lines matching a retired-column pattern.
// Full-line comments are skipped; trailing commentary stays scanned with
// its code, so a commented-out reader still fails the guard.
func deprecatedHits(path string, deprecated []*regexp.Regexp) []deprecatedHit {
	content, err := os.ReadFile(path)
	if err != nil {
		return []deprecatedHit{{line: "", desc: "unreadable: " + err.Error()}}
	}
	var hits []deprecatedHit
	for i, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		for _, pattern := range deprecated {
			if pattern.MatchString(line) {
				hits = append(hits, deprecatedHit{
					line: line,
					desc: fmt.Sprintf("line %d: %s", i+1, trimmed),
				})
				break
			}
		}
	}
	return hits
}

func allowlisted(rel, line string, allowed []allowEntry) bool {
	for _, entry := range allowed {
		if rel == entry.file && strings.Contains(line, entry.contains) {
			return true
		}
	}
	return false
}

// moduleRoot returns the peasant module root for the source walk.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("module root not found above the working directory")
		}
		dir = parent
	}
}
