package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultCommandTimeout is the per-git-command timeout. Guards against
	// pathologically slow git on large repos; each logical query is one call.
	DefaultCommandTimeout = 5 * time.Second

	// mergeLogScanLimit caps how many first-parent merge commits are scanned
	// when correlating merged branches to their merge commits. One bounded
	// call regardless of branch count.
	mergeLogScanLimit = 400

	// gitFieldSep is the ASCII unit separator used in --format strings so
	// commit subjects containing tabs/spaces parse unambiguously.
	gitFieldSep = "\x1f"

	// originHeadRef is the symbolic ref that names the remote default branch.
	originHeadRef = "refs/remotes/origin/HEAD"
	// originRefPrefix is stripped from the resolved origin HEAD target.
	originRefPrefix = "refs/remotes/origin/"
	// localHeadsPrefix prefixes local branch refs.
	localHeadsPrefix = "refs/heads/"

	// numstatBinaryMarker is the placeholder `git diff --numstat` emits in the
	// added/removed columns for binary files.
	numstatBinaryMarker = "-"
	// numstatRenameSep separates old and new paths in numstat rename notation
	// ("old => new", "pre/{old => new}/post").
	numstatRenameSep = " => "
)

// defaultBranchCandidates are tried in order when origin/HEAD is not set.
var defaultBranchCandidates = []string{"main", "master", "develop", "trunk"}

// mergeSubjectRe extracts the branch name from conventional merge-commit
// subjects: `Merge branch 'feat/x'`, `Merge branch 'feat/x' into develop`.
var mergeSubjectRe = regexp.MustCompile(`^Merge (?:remote-tracking )?branch '([^']+)'`)

// ExecGitRepository implements Repository by shelling out to git using
// exec.CommandContext in argument-list form (never `sh -c`). Each method
// issues one git call per logical query, bounded by Timeout.
type ExecGitRepository struct {
	repoPath string
	// Timeout is the per-command timeout. DefaultCommandTimeout when zero.
	Timeout time.Duration
}

var _ Repository = (*ExecGitRepository)(nil)

// NewExecGitRepository returns an ExecGitRepository rooted at repoPath with
// the default per-command timeout.
func NewExecGitRepository(repoPath string) *ExecGitRepository {
	return &ExecGitRepository{repoPath: repoPath, Timeout: DefaultCommandTimeout}
}

func (r *ExecGitRepository) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return DefaultCommandTimeout
}

// runGit executes one git command against the repo and returns raw stdout.
// The context is bounded by the per-command timeout.
func (r *ExecGitRepository) runGit(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	full := append([]string{"-C", r.repoPath}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).Output()
	if err != nil {
		return nil, fmt.Errorf("git %s in %s: %w", strings.Join(args, " "), r.repoPath, err)
	}
	return out, nil
}

// runGitLines executes one git command and returns non-empty trimmed output lines.
func (r *ExecGitRepository) runGitLines(ctx context.Context, args ...string) ([]string, error) {
	out, err := r.runGit(ctx, args...)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// DefaultBranch resolves origin/HEAD first; when unset (common for repos that
// were never cloned), it falls back to the first of main/master/develop/trunk
// that exists as a local branch.
func (r *ExecGitRepository) DefaultBranch(ctx context.Context) (string, error) {
	if out, err := r.runGit(ctx, "symbolic-ref", "--quiet", originHeadRef); err == nil {
		ref := strings.TrimSpace(string(out))
		if name, ok := strings.CutPrefix(ref, originRefPrefix); ok && name != "" {
			return name, nil
		}
	}
	for _, name := range defaultBranchCandidates {
		if _, err := r.runGit(ctx, "rev-parse", "--verify", "--quiet", localHeadsPrefix+name); err == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("gitops: cannot determine default branch in %s", r.repoPath)
}

// Branches lists local branches, excluding the default branch. Sorted by
// refname (git for-each-ref order). Default-branch resolution is best-effort:
// when it cannot be determined, no branch is excluded.
func (r *ExecGitRepository) Branches(ctx context.Context) ([]string, error) {
	lines, err := r.runGitLines(ctx, "for-each-ref", "--format=%(refname:short)", localHeadsPrefix)
	if err != nil {
		return nil, err
	}
	def, _ := r.DefaultBranch(ctx) // best-effort exclusion
	branches := make([]string, 0, len(lines))
	for _, name := range lines {
		if name == def {
			continue
		}
		branches = append(branches, name)
	}
	return branches, nil
}

// BranchState compares branch against the default branch: merge-base,
// ahead/behind counts (one rev-list --left-right --count call), and changed
// files (one diff --name-status -M call against the merge-base).
func (r *ExecGitRepository) BranchState(ctx context.Context, branch string) (*BranchState, error) {
	def, err := r.DefaultBranch(ctx)
	if err != nil {
		return nil, err
	}

	baseOut, err := r.runGit(ctx, "merge-base", def, branch)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSpace(string(baseOut))

	countOut, err := r.runGit(ctx, "rev-list", "--left-right", "--count", def+"..."+branch)
	if err != nil {
		return nil, err
	}
	behind, ahead, err := parseLeftRightCount(string(countOut))
	if err != nil {
		return nil, err
	}

	diffLines, err := r.runGitLines(ctx, "diff", "--name-status", "-M", base, branch)
	if err != nil {
		return nil, err
	}
	changed := parseNameStatus(diffLines)

	return &BranchState{
		Name:         branch,
		MergeBase:    base,
		AheadCount:   ahead,
		BehindCount:  behind,
		ChangedFiles: changed,
	}, nil
}

// DiffStats returns line-level diff statistics for head measured against its
// merge-base with base. One git call: `diff --numstat -M base...head` —
// three-dot merge-base semantics, the same baseline BranchState diffs against,
// with the same rename detection (-M). Binary files (numstat "-" placeholders)
// contribute 0/0 to the totals but keep their PerFile row.
func (r *ExecGitRepository) DiffStats(ctx context.Context, base, head string) (*DiffStats, error) {
	lines, err := r.runGitLines(ctx, "diff", "--numstat", "-M", base+"..."+head)
	if err != nil {
		return nil, err
	}
	return parseNumstat(lines)
}

// defaultDiffMaxLinesPerFile caps the DiffLines kept per file so a single
// generated/vendored file can't return megabytes; over-cap files come back
// Truncated. Mirrors the bounded-call discipline of the other methods.
const defaultDiffMaxLinesPerFile = 2000

// diffContextLines is the -U<n> context shown around each change.
const diffContextLines = 3

// DiffHunks returns the structured unified-diff hunks for head measured against
// its merge-base with base. One git call: `diff -M -U3 --no-color base...head
// [-- paths...]` — three-dot (merge-base) semantics, the same baseline DiffStats
// and BranchState use, with the same rename detection (-M). When paths is
// non-empty only those files are diffed (the lazy per-file path Review uses).
// Binary files come back with Binary=true and no hunks; files exceeding
// maxLinesPerFile come back Truncated with the lines kept so far.
func (r *ExecGitRepository) DiffHunks(ctx context.Context, base, head string, paths []string, maxLinesPerFile int) ([]FileDiff, error) {
	if maxLinesPerFile <= 0 {
		maxLinesPerFile = defaultDiffMaxLinesPerFile
	}
	args := []string{
		"diff", "-M", "--no-color", "--no-ext-diff",
		fmt.Sprintf("-U%d", diffContextLines), base + "..." + head,
	}
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	// Raw bytes, not runGitLines: patch bodies must preserve empty lines and
	// leading whitespace.
	out, err := r.runGit(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseUnifiedDiff(string(out), maxLinesPerFile), nil
}

// hunkHeaderRe parses "@@ -oldStart[,oldLines] +newStart[,newLines] @@ header".
var hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(?: (.*))?$`)

// parseUnifiedDiff turns `git diff` porcelain output into structured FileDiffs.
// It is tolerant: unknown header lines are ignored, and a malformed hunk header
// simply starts no hunk. maxLinesPerFile caps the lines kept per file.
func parseUnifiedDiff(out string, maxLinesPerFile int) []FileDiff {
	var files []FileDiff
	var cur *FileDiff
	var hunk *Hunk

	// finalize attaches the in-progress hunk to the current file.
	flushHunk := func() {
		if cur != nil && hunk != nil {
			cur.Hunks = append(cur.Hunks, *hunk)
			hunk = nil
		}
	}
	flushFile := func() {
		flushHunk()
		if cur != nil {
			files = append(files, *cur)
			cur = nil
		}
	}

	lineCount := 0
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flushFile()
			cur = &FileDiff{Status: FileStatusModified}
			hunk = nil
			lineCount = 0
			// Path comes from the +++/--- or rename lines below; the a/ b/
			// header is an imperfect source for paths with spaces.
		case cur == nil:
			// Preamble before the first file header — ignore.
			continue
		case strings.HasPrefix(line, "new file mode"):
			cur.Status = FileStatusAdded
		case strings.HasPrefix(line, "deleted file mode"):
			cur.Status = FileStatusDeleted
		case strings.HasPrefix(line, "rename from "):
			old := strings.TrimPrefix(line, "rename from ")
			cur.OldPath = &old
			cur.Status = FileStatusRenamed
		case strings.HasPrefix(line, "rename to "):
			cur.Path = strings.TrimPrefix(line, "rename to ")
			cur.Status = FileStatusRenamed
		case strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "GIT binary patch"):
			cur.Binary = true
		// The ---/+++ file headers appear ONLY in the preamble before the first
		// @@ (hunk == nil). Gating on that prevents an in-hunk content line whose
		// text starts with "-- " or "++ " (e.g. a SQL/Lua comment) — which git
		// emits as "--- " / "+++ " once the +/- diff marker is prepended — from
		// being mis-parsed as a header and dropped.
		case hunk == nil && strings.HasPrefix(line, "--- "):
			// old path; only used to fill Path for deletes (+++ is /dev/null).
			if p, ok := diffPathFromHeader(line, "--- "); ok && cur.Status == FileStatusDeleted {
				cur.Path = p
			}
		case hunk == nil && strings.HasPrefix(line, "+++ "):
			if p, ok := diffPathFromHeader(line, "+++ "); ok && cur.Path == "" {
				cur.Path = p
			}
		case strings.HasPrefix(line, "@@"):
			flushHunk()
			if m := hunkHeaderRe.FindStringSubmatch(line); m != nil {
				hunk = &Hunk{
					OldStart: atoiOr(m[1], 0),
					OldLines: atoiOr(m[2], 1),
					NewStart: atoiOr(m[3], 0),
					NewLines: atoiOr(m[4], 1),
					Header:   m[5],
				}
			}
		case hunk != nil && (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") || strings.HasPrefix(line, " ")):
			if cur.Truncated {
				continue
			}
			if lineCount >= maxLinesPerFile {
				cur.Truncated = true
				continue
			}
			kind := DiffLineContext
			switch line[0] {
			case '+':
				kind = DiffLineAdded
			case '-':
				kind = DiffLineRemoved
			}
			hunk.Lines = append(hunk.Lines, DiffLine{Kind: kind, Text: line[1:]})
			lineCount++
		case line == `\ No newline at end of file`:
			// Informational git marker — not a content line; ignore.
			continue
		}
	}
	flushFile()
	return files
}

// diffPathFromHeader extracts the repo-relative path from a "--- a/path" or
// "+++ b/path" line, stripping the a/ or b/ prefix. Returns ok=false for
// /dev/null (added/deleted placeholder).
func diffPathFromHeader(line, prefix string) (string, bool) {
	p := strings.TrimPrefix(line, prefix)
	if p == "/dev/null" {
		return "", false
	}
	// git uses a/ and b/ prefixes by default.
	if len(p) > 2 && (p[:2] == "a/" || p[:2] == "b/") {
		p = p[2:]
	}
	return p, true
}

// atoiOr parses s as an int, returning fallback on empty/parse error.
func atoiOr(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

// blameHeaderRe matches a porcelain blame header line: "<40-hex> <orig> <final>
// [<group-size>]". Only the hash is captured; the rest positions the group.
var blameHeaderRe = regexp.MustCompile(`^([0-9a-f]{40}) \d+ \d+`)

// BlameCommits returns, for each 1-based line of path at ref, the commit hash
// that last modified that line. One git call: `blame --porcelain <ref> -- path`.
// Porcelain emits, per line, a header ("<hash> <orig> <final> [<n>]") followed
// by porcelain fields and the content line (tab-prefixed); the most recent
// header's hash owns each content line. A path absent at ref returns an empty
// slice (git blame errors), not a hard error.
func (r *ExecGitRepository) BlameCommits(ctx context.Context, ref, path string) ([]string, error) {
	out, err := r.runGit(ctx, "blame", "--porcelain", ref, "--", path)
	if err != nil {
		// Absent path / unborn ref / binary: no attribution rather than a 500.
		return []string{}, nil
	}
	var commits []string
	currentHash := ""
	for _, line := range strings.Split(string(out), "\n") {
		if m := blameHeaderRe.FindStringSubmatch(line); m != nil {
			currentHash = m[1]
			continue
		}
		// Content lines are tab-prefixed; each maps to the current header hash.
		if strings.HasPrefix(line, "\t") {
			commits = append(commits, currentHash)
		}
	}
	return commits, nil
}

// revertTrailerRe extracts the reverted hash from git's revert-commit trailer
// ("This reverts commit <hash>."). Hashes may be abbreviated (7+ hex).
var revertTrailerRe = regexp.MustCompile(`This reverts commit ([0-9a-f]{7,40})`)

// revertScanLimit caps how many default-branch commits are scanned for revert
// trailers — one bounded log call, independent of history depth.
const revertScanLimit = 1000

// RevertedCommits returns the set of commit hashes undone by a "git revert" on
// ref, parsed from the "This reverts commit <hash>" trailer. One bounded git
// call (`log -n <limit> --format=%b`). Hashes are returned as written (possibly
// abbreviated); callers match by prefix.
func (r *ExecGitRepository) RevertedCommits(ctx context.Context, ref string) (map[string]bool, error) {
	out, err := r.runGit(ctx, "log", ref, "-n", strconv.Itoa(revertScanLimit), "--format=%b"+recordSep)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool)
	for _, m := range revertTrailerRe.FindAllStringSubmatch(string(out), -1) {
		set[m[1]] = true
	}
	return set, nil
}

// recordSep terminates each commit's body in the revert scan (ASCII record
// separator) so multi-line bodies don't bleed together.
const recordSep = "\x1e"

// MergedBranches lists local branches merged into the default branch, most
// recently merged first, capped at limit (limit <= 0 means no cap). Three
// bounded git calls total, independent of branch count:
//
//  1. `branch --merged <default>` — candidate names.
//  2. `log --merges --first-parent <default>` (capped) — merge commits,
//     correlated to branch names via conventional merge subjects.
//  3. `for-each-ref refs/heads` — branch tip times, the fast-forward fallback
//     for MergedAtMs (no merge commit exists; MergeCommit stays empty).
func (r *ExecGitRepository) MergedBranches(ctx context.Context, limit int) ([]MergedBranch, error) {
	def, err := r.DefaultBranch(ctx)
	if err != nil {
		return nil, err
	}

	names, err := r.runGitLines(ctx, "branch", "--merged", def, "--format=%(refname:short)")
	if err != nil {
		return nil, err
	}

	type mergeInfo struct {
		hash   string
		timeMs int64
	}
	// Merge commits on the default branch's first-parent history, most recent
	// first. The first subject naming a branch wins (latest merge).
	merges := map[string]mergeInfo{}
	logLines, err := r.runGitLines(ctx, "log", def, "--merges", "--first-parent",
		"-n", strconv.Itoa(mergeLogScanLimit), "--format=%H"+gitFieldSep+"%ct"+gitFieldSep+"%s")
	if err == nil { // empty merge history is fine; correlation is best-effort
		for _, line := range logLines {
			fields := strings.SplitN(line, gitFieldSep, 3)
			if len(fields) != 3 {
				continue
			}
			m := mergeSubjectRe.FindStringSubmatch(fields[2])
			if m == nil {
				continue
			}
			if _, seen := merges[m[1]]; seen {
				continue
			}
			ct, _ := strconv.ParseInt(fields[1], 10, 64)
			merges[m[1]] = mergeInfo{hash: fields[0], timeMs: ct * 1000}
		}
	}

	// Branch tip committer times (fast-forward fallback).
	tipTimes := map[string]int64{}
	tipLines, err := r.runGitLines(ctx, "for-each-ref", localHeadsPrefix,
		"--format=%(refname:short)"+gitFieldSep+"%(committerdate:unix)")
	if err == nil {
		for _, line := range tipLines {
			fields := strings.SplitN(line, gitFieldSep, 2)
			if len(fields) != 2 {
				continue
			}
			ts, _ := strconv.ParseInt(fields[1], 10, 64)
			tipTimes[fields[0]] = ts * 1000
		}
	}

	merged := make([]MergedBranch, 0, len(names))
	for _, name := range names {
		if name == def {
			continue
		}
		mb := MergedBranch{Name: name}
		if mi, ok := merges[name]; ok {
			mb.MergeCommit = mi.hash
			mb.MergedAtMs = mi.timeMs
		} else if ts, ok := tipTimes[name]; ok {
			mb.MergedAtMs = ts
		}
		merged = append(merged, mb)
	}

	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].MergedAtMs != merged[j].MergedAtMs {
			return merged[i].MergedAtMs > merged[j].MergedAtMs
		}
		return merged[i].Name < merged[j].Name
	})
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

// FileAtCommit returns the file content at commit (git show commit:path).
func (r *ExecGitRepository) FileAtCommit(ctx context.Context, commit, path string) ([]byte, error) {
	return r.runGit(ctx, "show", commit+":"+path)
}

// FilesAtCommit returns the contents of many paths at a commit using ONE
// `git cat-file --batch` process fed "<commit>:<path>" queries on stdin —
// the per-file `git show` exec overhead (~10ms each) dominates codegraph
// builds on real repos, so bulk reads are the difference between seconds
// and minutes on the Map/Review endpoints. Paths missing at the commit and
// non-blob entries (submodules, trees) are absent from the result map.
// Paths containing newlines cannot be queried over the line-based batch
// protocol and are skipped.
func (r *ExecGitRepository) FilesAtCommit(ctx context.Context, commit string, paths []string) (map[string][]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()

	queried := make([]string, 0, len(paths))
	var input strings.Builder
	for _, p := range paths {
		if strings.ContainsRune(p, '\n') {
			continue
		}
		queried = append(queried, p)
		input.WriteString(commit + ":" + p + "\n")
	}

	cmd := exec.CommandContext(ctx, "git", "-C", r.repoPath, "cat-file", "--batch")
	cmd.Stdin = strings.NewReader(input.String())
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file --batch in %s: %w", r.repoPath, err)
	}
	return parseCatFileBatch(out, queried)
}

// parseCatFileBatch parses `git cat-file --batch` output. Responses arrive
// in request order: either "<oid> <type> <size>\n<contents>\n" or
// "<spec> missing\n" (also "ambiguous"/"dangling" for malformed specs).
// Only blob entries are kept.
func parseCatFileBatch(out []byte, paths []string) (map[string][]byte, error) {
	const catFileBlobType = "blob"
	contents := make(map[string][]byte, len(paths))
	rest := out
	for _, p := range paths {
		nl := bytes.IndexByte(rest, '\n')
		if nl < 0 {
			return nil, fmt.Errorf("gitops: cat-file batch output truncated before %q", p)
		}
		header := string(rest[:nl])
		rest = rest[nl+1:]

		fields := strings.Fields(header)
		// Non-existence markers: "<spec> missing" and friends (2 fields).
		if len(fields) != 3 {
			continue
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("gitops: cat-file batch header %q: %w", header, err)
		}
		if len(rest) < size+1 { // content + trailing newline
			return nil, fmt.Errorf("gitops: cat-file batch output truncated inside %q", p)
		}
		if fields[1] == catFileBlobType {
			blob := make([]byte, size)
			copy(blob, rest[:size])
			contents[p] = blob
		}
		rest = rest[size+1:]
	}
	return contents, nil
}

// ListFiles lists all tracked files at ref.
func (r *ExecGitRepository) ListFiles(ctx context.Context, ref string) ([]string, error) {
	return r.runGitLines(ctx, "ls-tree", "-r", "--name-only", ref)
}

// Commits returns commit metadata for ref, newest first, capped at limit
// (limit <= 0 means no cap).
func (r *ExecGitRepository) Commits(ctx context.Context, ref string, limit int) ([]Commit, error) {
	args := []string{"log", ref, "--format=%H" + gitFieldSep + "%s" + gitFieldSep + "%ct" + gitFieldSep + "%ae"}
	if limit > 0 {
		args = append(args, "-n", strconv.Itoa(limit))
	}
	lines, err := r.runGitLines(ctx, args...)
	if err != nil {
		return nil, err
	}
	commits := make([]Commit, 0, len(lines))
	for _, line := range lines {
		fields := strings.SplitN(line, gitFieldSep, 4)
		if len(fields) != 4 {
			continue
		}
		ct, _ := strconv.ParseInt(fields[2], 10, 64)
		commits = append(commits, Commit{
			Hash:        fields[0],
			Subject:     fields[1],
			TimeMs:      ct * 1000,
			AuthorEmail: fields[3],
		})
	}
	return commits, nil
}

// CommitsInRange returns hashes reachable from head but not from base
// (commits ahead of the merge-base), newest first.
func (r *ExecGitRepository) CommitsInRange(ctx context.Context, base, head string) ([]string, error) {
	return r.runGitLines(ctx, "rev-list", base+".."+head)
}

// parseLeftRightCount parses `rev-list --left-right --count A...B` output
// ("<left>\t<right>") into (left, right) = (behind, ahead) when A is the
// default branch and B the feature branch.
func parseLeftRightCount(out string) (left, right int, err error) {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("gitops: unexpected rev-list --count output %q", out)
	}
	left, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("gitops: parse rev-list left count %q: %w", fields[0], err)
	}
	right, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("gitops: parse rev-list right count %q: %w", fields[1], err)
	}
	return left, right, nil
}

// parseNumstat parses `diff --numstat -M` lines into DiffStats. Lines:
// "12\t3\tpath" for text files, "-\t-\tpath" for binary files (kept as a 0/0
// PerFile row), "1\t1\told => new" or "0\t0\tpre/{old => new}/post" for
// renames (normalized to the new path).
func parseNumstat(lines []string) (*DiffStats, error) {
	stats := &DiffStats{PerFile: make([]FileDiffStat, 0, len(lines))}
	for _, line := range lines {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue // defensive: numstat rows are always 3 tab-separated fields
		}
		var added, removed int
		if fields[0] != numstatBinaryMarker || fields[1] != numstatBinaryMarker {
			var err error
			added, err = strconv.Atoi(fields[0])
			if err != nil {
				return nil, fmt.Errorf("gitops: parse numstat added count %q: %w", fields[0], err)
			}
			removed, err = strconv.Atoi(fields[1])
			if err != nil {
				return nil, fmt.Errorf("gitops: parse numstat removed count %q: %w", fields[1], err)
			}
		}
		stats.LinesAdded += added
		stats.LinesRemoved += removed
		stats.PerFile = append(stats.PerFile, FileDiffStat{
			Path:    numstatPath(fields[2]),
			Added:   added,
			Removed: removed,
		})
	}
	return stats, nil
}

// numstatPath normalizes a numstat path field. Plain paths pass through;
// rename notations resolve to the new path (matching FileChange.Path):
//
//	"old.go => new.go"          → "new.go"
//	"pre/{old => new}/post.go"  → "pre/new/post.go"
//	"a/{b => }/c.go"            → "a/c.go" (vanished component collapsed)
func numstatPath(field string) string {
	if start := strings.Index(field, "{"); start >= 0 {
		if end := strings.Index(field[start:], "}"); end >= 0 {
			inner := field[start+1 : start+end]
			if _, newPart, ok := strings.Cut(inner, numstatRenameSep); ok {
				return path.Clean(field[:start] + newPart + field[start+end+1:])
			}
		}
	}
	if _, newPath, ok := strings.Cut(field, numstatRenameSep); ok {
		return newPath
	}
	return field
}

// parseNameStatus parses `diff --name-status -M` lines into FileChanges.
// Lines: "M\tpath", "A\tpath", "D\tpath", "R100\told\tnew". Unmodeled status
// codes (copies, typechanges, unmerged) are skipped — the Map/Review contract
// only models M/A/D/R.
func parseNameStatus(lines []string) []FileChange {
	changes := make([]FileChange, 0, len(lines))
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		status, err := NewFileStatus(fields[0])
		if err != nil {
			continue // unmodeled status code
		}
		if status == FileStatusRenamed {
			if len(fields) < 3 {
				continue
			}
			oldPath := fields[1]
			changes = append(changes, FileChange{Path: fields[2], Status: status, OldPath: &oldPath})
			continue
		}
		changes = append(changes, FileChange{Path: fields[1], Status: status})
	}
	return changes
}
