package ingest

import "regexp"

// CommandParser scans transcript bytes for git command patterns to validate
// whether a session contained intentional git activity.
//
// Detection is intentionally coarse: if the transcript contains any git command
// (commit, push, rebase, or merge), all timestamp-based candidates are accepted.
// Per-hash command correlation is deferred to a future enhancement since transcript
// formats are not yet stable enough for reliable hash extraction.
type CommandParser struct {
	commitRe *regexp.Regexp
	pushRe   *regexp.Regexp
	rebaseRe *regexp.Regexp
	mergeRe  *regexp.Regexp
}

func newCommandParser() *CommandParser {
	return &CommandParser{
		commitRe: regexp.MustCompile(`\bgit\s+commit\b`),
		pushRe:   regexp.MustCompile(`\bgit\s+push\b`),
		rebaseRe: regexp.MustCompile(`\bgit\s+rebase\b`),
		mergeRe:  regexp.MustCompile(`\bgit\s+merge\b`),
	}
}

// hasGitActivity returns true if the transcript bytes contain any recognised
// git command pattern.
func (cp *CommandParser) hasGitActivity(data []byte) bool {
	s := string(data)
	return cp.commitRe.MatchString(s) ||
		cp.pushRe.MatchString(s) ||
		cp.rebaseRe.MatchString(s) ||
		cp.mergeRe.MatchString(s)
}

// ValidateCommits returns the candidates that are supported by the transcript
// evidence. If the transcript contains any git activity, all candidates are
// returned (deduped by hash). If no git activity is found, an empty slice is
// returned to signal "no intent detected".
func (cp *CommandParser) ValidateCommits(transcriptData []byte, candidates []CommitInfo) []CommitInfo {
	if !cp.hasGitActivity(transcriptData) {
		return []CommitInfo{} // no git commands found in session transcript
	}

	// Git activity confirmed — deduplicate candidates by hash.
	seen := make(map[string]struct{}, len(candidates))
	result := make([]CommitInfo, 0, len(candidates))
	for _, c := range candidates {
		if _, dup := seen[c.Hash]; !dup {
			seen[c.Hash] = struct{}{}
			result = append(result, c)
		}
	}
	return result
}
