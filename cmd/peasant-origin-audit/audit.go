//go:build origin_audit

package main

import (
	"context"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
)

// Report is the tallied result of one read-only pass of the production
// session-origin rule over a Claude Code harness directory. It is the
// evidence for acceptance gate G1: does the rule, run over a real person's
// real transcript history, produce the distribution the plan predicted.
type Report struct {
	SourcePath       string
	ExaminedFiles    int
	Sessions         int
	RootSessions     int
	SubagentSessions int
	BySignal         map[sessionorigin.Signal]int
	ByOrigin         map[sessionorigin.Origin]int
	// UnreadablePaths are transcript files the production adapter could not
	// read. Discover fails OPEN on a read error -- it still reports the
	// session, classified Unknown/no-evidence, so the operator sees it rather
	// than losing it silently -- but that means an unreadable file and a
	// genuinely evidence-free transcript land in the same signal bucket. This
	// list is how a read failure stays visible instead of being quietly
	// absorbed into "no evidence".
	UnreadablePaths []string
	// UnaccountedPaths are transcript files this pass examined that produced
	// no DiscoveredSession at all and were not a read failure: a filename that
	// does not parse as a session identifier, or a transcript whose content
	// carries no conversation record. Never silently dropped.
	UnaccountedPaths []string
}

// RunAudit discovers every Claude Code session under sourcePath through the
// SAME production discovery path Peasant's ingest pipeline uses --
// ingest.NewClaudeAdapter(...).Discover -- and tallies the deciding signal the
// production sessionorigin.Classify rule reached for each one. It contains no
// second copy of the classification rule or the transcript parser: Classify
// and the Claude adapter's mining loop are the only places either is written,
// and this function only reads their output.
//
// READ ONLY, by construction rather than by convention: fs is never given an
// evidence cache (ingest.AttachClaudeEvidenceCache is never called on the
// adapter this function builds), so ClaudeAdapter.Discover takes the
// no-cache-attached branch of its own logic and never calls
// ClaudeEvidenceCache.SaveClaudeEvidence -- see ClaudeAdapter.saveMinedEvidence
// in internal/ingest/claude.go, which returns immediately when no cache is
// set. Discover also never calls ExtractMetadata, so it never reads a
// transcript through the one path that could plausibly grow a write later
// without this file changing. Net effect: this pass touches no database, no
// evidence cache file, and no path outside the transcripts it reads.
func RunAudit(ctx context.Context, fs ingest.FileSystem, sourcePath ingest.ResolvedPath) (Report, error) {
	adapter := ingest.NewClaudeAdapter(fs, nil, salt.Salt{})

	cfg := ingest.SourceConfig{Paths: []ingest.ResolvedPath{sourcePath}, Enabled: true}
	sessions, err := adapter.Discover(ctx, cfg)
	if err != nil {
		return Report{}, fmt.Errorf(
			"RunAudit: discover Claude sessions under %q: %w (where: cmd/peasant-origin-audit.RunAudit; "+
				"means: no measurement was produced; fix: confirm the path exists and is a Claude Code "+
				"projects directory)",
			sourcePath, err,
		)
	}

	examined, err := listJSONLFiles(fs, sourcePath.String())
	if err != nil {
		return Report{}, fmt.Errorf(
			"RunAudit: list transcript files under %q: %w (where: cmd/peasant-origin-audit.RunAudit; "+
				"means: the examined-file total would undercount; fix: confirm the path is a readable directory)",
			sourcePath, err,
		)
	}

	report := Report{
		SourcePath: sourcePath.String(),
		BySignal:   make(map[sessionorigin.Signal]int, len(sessionorigin.AllSignals)),
		ByOrigin:   make(map[sessionorigin.Origin]int, len(sessionorigin.All)),
	}
	for _, signal := range sessionorigin.AllSignals {
		report.BySignal[signal] = 0
	}
	for _, origin := range sessionorigin.All {
		report.ByOrigin[origin] = 0
	}

	accounted := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		accounted[session.SourcePath.String()] = true
		report.Sessions++
		if session.ParentUUID == nil {
			report.RootSessions++
		} else {
			report.SubagentSessions++
		}
		report.BySignal[session.Signal]++
		report.ByOrigin[session.Origin]++

		// A root classified Unknown/no-evidence is exactly the bucket a
		// read failure fails open into (see the doc comment on
		// UnreadablePaths), so it is the only accounted-for case worth a
		// second, deliberate readability check.
		if session.ParentUUID == nil && session.Signal == sessionorigin.SignalNoEvidence {
			readable, checkErr := isReadable(fs, session.SourcePath.String())
			if checkErr != nil {
				return Report{}, fmt.Errorf("RunAudit: check readability of %q: %w", session.SourcePath, checkErr)
			}
			if !readable {
				report.UnreadablePaths = append(report.UnreadablePaths, session.SourcePath.String())
			}
		}
	}

	report.ExaminedFiles = len(examined)
	for _, path := range examined {
		if accounted[path] {
			continue
		}
		readable, checkErr := isReadable(fs, path)
		if checkErr != nil {
			return Report{}, fmt.Errorf("RunAudit: check readability of %q: %w", path, checkErr)
		}
		if readable {
			report.UnaccountedPaths = append(report.UnaccountedPaths, path)
		} else {
			report.UnreadablePaths = append(report.UnreadablePaths, path)
		}
	}
	sort.Strings(report.UnreadablePaths)
	sort.Strings(report.UnaccountedPaths)

	return report, nil
}

// listJSONLFiles enumerates every *.jsonl file under root, mirroring only the
// file-shape filter ClaudeAdapter.Discover itself applies before it ever asks
// what kind of session a path names (skip symlinks per RFC 6.4, skip
// directories, keep the ".jsonl" suffix). It does not parse a single byte of
// any file or decide what a path means to the classifier -- it exists solely
// so the total "examined" count reflects what is really on disk, independent
// of whatever Discover chose to report.
func listJSONLFiles(fs ingest.FileSystem, root string) ([]string, error) {
	var paths []string
	err := fs.WalkDir(root, func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !strings.HasSuffix(path, defaults.ExtJSONL.String()) {
			return nil
		}
		paths = append(paths, filepath.Clean(path))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

// isReadable reports whether path can be read in full, discarding the
// content. It exists purely to distinguish a read failure from a genuine
// absence of evidence -- see the doc comment on Report.UnreadablePaths -- and
// it performs no parsing of what it reads.
func isReadable(fs ingest.FileSystem, path string) (bool, error) {
	_, err := fs.ReadFile(path)
	if err == nil {
		return true, nil
	}
	if os.IsPermission(err) || os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// printReport writes a plain, unambiguous account of the pass: totals, the
// per-signal and per-origin tallies, and every path this pass could not read
// or could not classify, so a person comparing against the recorded reference
// tiers never has to guess what a silent gap meant.
func printReport(w io.Writer, r Report) {
	fmt.Fprintln(w, "peasant origin audit -- read only, writes nothing")
	fmt.Fprintf(w, "source: %s\n\n", r.SourcePath)
	fmt.Fprintf(w, "examined .jsonl files: %d\n", r.ExaminedFiles)
	fmt.Fprintf(w, "sessions classified:   %d (%d root, %d subagent)\n\n", r.Sessions, r.RootSessions, r.SubagentSessions)

	fmt.Fprintln(w, "by deciding signal:")
	for _, signal := range sessionorigin.AllSignals {
		fmt.Fprintf(w, "  %-20s %d\n", signal.String(), r.BySignal[signal])
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "by origin:")
	for _, origin := range sessionorigin.All {
		fmt.Fprintf(w, "  %-10s %d\n", origin.String(), r.ByOrigin[origin])
	}

	if len(r.UnreadablePaths) > 0 {
		fmt.Fprintf(w, "\ncould not read (%d) -- counted above as unknown/no-evidence because Discover fails open, "+
			"listed here so a read failure is never mistaken for a genuine absence of evidence:\n", len(r.UnreadablePaths))
		for _, path := range r.UnreadablePaths {
			fmt.Fprintf(w, "  %s\n", path)
		}
	}

	if len(r.UnaccountedPaths) > 0 {
		fmt.Fprintf(w, "\nexamined but produced no session (%d) -- an unrecognised filename, or a transcript with "+
			"no conversation record:\n", len(r.UnaccountedPaths))
		for _, path := range r.UnaccountedPaths {
			fmt.Fprintf(w, "  %s\n", path)
		}
	}
}
