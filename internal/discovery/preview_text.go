package discovery

import (
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// The words of a preview. The terminal pane and the setup page describe the
// same rows, so the sentences they say are written here and read by both. A
// surface decides how a line is drawn and where a value is cleaned; it does not
// write its own sentence for a project, a branch, or a cut transcript.

// PreviewNoTurnsNote is what a preview says for a discovered session whose
// harness transcript holds no readable turns.
const PreviewNoTurnsNote = "no readable turns are in this session's transcript yet."

// HarnessLabel is the name of a harness in the lowercase chrome of a preview.
func HarnessLabel(harness string) string {
	return strings.ToLower(schema.HarnessDisplayName(defaults.Harness(harness)))
}

// CleanPreviewValue makes a value from the machine safe to put on one line of a
// preview: control characters are dropped and every run of white space becomes
// one space. A terminal removes whole escape sequences before it calls its own
// cleaner; this one serves a surface that is not a terminal.
func CleanPreviewValue(value string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return -1
		default:
			return r
		}
	}, value)
	return strings.Join(strings.Fields(cleaned), " ")
}

// RepositoryPreviewLines is what a project or a branch row previews: the
// project, the branch when the row is one, the harnesses, remotes, git
// directories, worktrees and branches the row covers, and how many sessions it
// holds. clean makes each value from the machine safe for the surface that draws
// it. The lines carry no markup and no trailing white space.
func RepositoryPreviewLines(context PreviewContext, clean func(string) string) []string {
	lines := []string{"project: " + clean(context.Project)}
	if context.Kind == PreviewBranch && context.Branch != "" {
		lines = append(lines, "branch: "+clean(context.Branch))
	}
	if len(context.Harnesses) > 0 {
		names := make([]string, 0, len(context.Harnesses))
		for _, harness := range context.Harnesses {
			names = append(names, HarnessLabel(harness))
		}
		label := "harness: "
		if len(names) > 1 {
			label = "harnesses: "
		}
		lines = append(lines, label+strings.Join(names, ", "))
	}
	if len(context.Remotes) > 0 {
		label := "remote: "
		if len(context.Remotes) > 1 {
			label = "remotes: "
		}
		values := make([]string, 0, len(context.Remotes))
		for _, remote := range context.Remotes {
			values = append(values, clean(remote))
		}
		lines = append(lines, label+strings.Join(values, ", "))
	}
	if len(context.GitDirectories) == 1 {
		lines = append(lines, "git directory: "+clean(context.GitDirectories[0]))
	} else if len(context.GitDirectories) > 1 {
		lines = appendPreviewList(lines, "git directories", context.GitDirectories, clean)
	}
	lines = appendPreviewList(lines, "worktrees", context.ClonePaths, clean)
	if context.Kind == PreviewProject {
		lines = appendPreviewList(lines, "branches", context.Branches, clean)
	}
	return append(lines, fmt.Sprintf("sessions: %d", context.SessionCount))
}

func appendPreviewList(lines []string, label string, values []string, clean func(string) string) []string {
	if len(values) == 0 {
		return lines
	}
	lines = append(lines, fmt.Sprintf("%s: %d", label, len(values)))
	for _, value := range values {
		lines = append(lines, "  "+clean(value))
	}
	return lines
}

// PreviewByteSize writes a byte count the way a preview note reads it aloud:
// whole mebibytes past a mebibyte, kibibytes below that.
func PreviewByteSize(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%d MiB", size/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%d KiB", size/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", size)
	}
}

// PreviewCutNotice says how much of a session a preview shows when the session
// continues past it: what is loaded, what the whole session holds, and that the
// full session ingests normally, because a cut-off transcript otherwise reads as
// data loss. canLoadMore adds how to load more, for a surface that can. A
// session with nothing more behind it gets no sentence, and neither does one
// whose size is unknown.
func PreviewCutNotice(consumed, total int64, more, canLoadMore bool) string {
	if !more || total <= 0 {
		return ""
	}
	loadMore := ""
	if canLoadMore {
		loadMore = " scroll to the bottom to load more."
	}
	return fmt.Sprintf("showing the first %s of this %s session.%s the full session ingests normally.",
		PreviewByteSize(consumed), PreviewByteSize(total), loadMore)
}
