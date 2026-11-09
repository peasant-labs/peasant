package ingest

import (
	"fmt"
	"strings"
)

// The stored-but-unreadable warning is rendered here, once, because three
// surfaces print it: `harvest` and `harvest index` print one block per
// unreadable selected id on stderr, `open` prints one line for the selected id,
// and the dashboard logs that same line. A caller that rendered its own copy
// would drift from the others the first time a clause changed.

// UnreadableWarnings returns one labelled block per unreadable SELECTED member,
// in the report's member order. An unreadable child or subagent of a selected
// session is named on its parent's `also:` line instead of getting a block of
// its own: a full harvest prints nothing for an unreadable child, so a scoped
// run says only that the stored copies were kept. An unreadable child of a
// readable selected session prints nothing at all.
//
// command is the words the user typed ("harvest", "harvest index"); prefix is
// the shell-quoted command that reaches the same store, and only commands the
// message suggests carry it.
func (r ScopeReport) UnreadableWarnings(command, prefix string) []string {
	var blocks []string
	for _, member := range r.Members {
		if member.Role != CohortSelected || member.Outcome != LocateStoredUnreadable {
			continue
		}
		blocks = append(blocks, r.unreadableBlock(member, command, prefix))
	}
	return blocks
}

// unreadableBlock renders the labelled block for one unreadable selected id.
func (r ScopeReport) unreadableBlock(member LocatedMember, command, prefix string) string {
	id := warningShortID(member.ID)
	var b strings.Builder
	fmt.Fprintf(&b, "peasant: session %s was not read again, so its stored transcript was kept\n", id)
	b.WriteString(labelBlock("what:", fmt.Sprintf("this session-scoped %s kept the stored copy of session %s and did not re-read its source", warningCommandName(command), id), false))
	why, path := unreadableWhy(member)
	b.WriteString(labelBlock("why:", why, false))
	if path != "" {
		b.WriteString(strings.Repeat(" ", blockLabelColumn))
		b.WriteString(path)
		b.WriteString("\n")
	}
	if member.Unreadable == UnreadableSourceGone {
		b.WriteString(strings.Repeat(" ", blockLabelColumn))
		b.WriteString("(Claude Code removes old transcripts after its retention period; peasant keeps the stored copy)\n")
	}
	b.WriteString(labelBlock("where:", "session lookup (ingest scoped locate)", false))
	b.WriteString(labelBlock("when:", "before the session was diffed; its transcript was not re-read or re-indexed", false))
	if also := r.unreadableChildrenLine(member); also != "" {
		b.WriteString(labelBlock("also:", also, false))
	}
	b.WriteString(labelBlock("impact:", blockUnreadableImpactText, true))
	b.WriteString(labelBlock("fix:", unreadableFix(member, prefix), true))
	return strings.TrimRight(b.String(), "\n")
}

// unreadableChildrenLine names how many recorded children of an unreadable
// selected session were also kept as stored. It is empty when none were.
func (r ScopeReport) unreadableChildrenLine(parent LocatedMember) string {
	count := 0
	for _, member := range r.Members {
		if member.Role != CohortChild || member.Outcome != LocateStoredUnreadable {
			continue
		}
		if member.Harness == parent.Harness {
			count++
		}
	}
	switch count {
	case 0:
		return ""
	case 1:
		return "1 recorded subagent of this session was not read again either (kept as stored)"
	default:
		return fmt.Sprintf("%d recorded subagents of this session were not read again either (kept as stored)", count)
	}
}

// unreadableWhy returns the reason line and, when the message names a path, the
// path to print under it.
func unreadableWhy(member LocatedMember) (why, path string) {
	switch member.Unreadable {
	case UnreadableSourceGone:
		return "the source path recorded for this session no longer holds it:", string(member.StoredPath)
	case UnreadableHarnessDisabled:
		return fmt.Sprintf("the harness that recorded this session (%s) is not enabled in config.yaml, so its source cannot be read again", harnessName(member.Harness)), ""
	case UnreadableSourceFailed:
		detail := member.Detail
		if detail == "" {
			detail = "the recorded source could not be read"
		}
		return "the source recorded for this session could not be read: " + detail, string(member.StoredPath)
	default:
		return "this session is recorded, but its source cannot be read again for an unstated reason", string(member.StoredPath)
	}
}

// unreadableFix is the fix clause for one unreadable selected session.
func unreadableFix(member LocatedMember, prefix string) string {
	command := warningCommand(prefix)
	switch member.Unreadable {
	case UnreadableHarnessDisabled:
		return fmt.Sprintf("enable it with `%s kickstart`, or keep using the stored copy", command)
	case UnreadableSourceFailed:
		path := string(member.StoredPath)
		if path == "" {
			path = "the recorded source path"
		}
		return fmt.Sprintf("nothing is needed to keep using the stored copy; to record it again, repair the source at %s, then run `%s harvest --session %s` again", path, command, member.ID)
	case UnreadableSourceGone:
		return fmt.Sprintf("nothing is needed to keep using the stored copy. To record this session again, restore a transcript at that path (or re-enable its harness), then run `%s harvest --session %s` again", command, member.ID)
	default:
		return "nothing is needed to keep using the stored copy"
	}
}

// UnreadableOpenLine is the one stderr line `open` prints, and the line the
// dashboard logs, for a selected session whose source could not be read again.
// The empty string means the member is readable, so there is nothing to say.
func (m LocatedMember) UnreadableOpenLine() string {
	if m.Outcome != LocateStoredUnreadable {
		return ""
	}
	id := warningShortID(m.ID)
	clause := "the source no longer holds it"
	switch m.Unreadable {
	case UnreadableHarnessDisabled:
		clause = fmt.Sprintf("its harness, %s, is not enabled", harnessName(m.Harness))
	case UnreadableSourceFailed:
		detail := m.Detail
		if detail == "" {
			detail = "the recorded source could not be read"
		}
		clause = "its source could not be read: " + detail
	}
	return fmt.Sprintf("peasant: session %s was not read again; showing its stored transcript (%s)", id, clause)
}

// SelectedMember returns the report member for one selected id, and whether the
// id is in the report. A miss has no member: it is a SessionScopeError instead.
func (r ScopeReport) SelectedMember(id SessionID) (LocatedMember, bool) {
	for _, member := range r.Members {
		if member.ID == id && member.Role == CohortSelected {
			return member, true
		}
	}
	return LocatedMember{}, false
}

const blockUnreadableImpactText = "the stored transcript, its index and its links were not recomputed, and `peasant open` shows the stored " +
	"copy. The bookkeeping a full harvest does for a recorded session (its origin verdict, its metrics, and " +
	"stored-copy maintenance) ran for it as in a full harvest. Start-up maintenance may also have run."

// warningCommandName returns the words the user typed, defaulting to "harvest"
// the way SessionScopeError does.
func warningCommandName(command string) string {
	if command == "" {
		return "harvest"
	}
	return command
}

// warningCommand renders the run's command prefix, defaulting to a bare
// "peasant" for a pipeline-only caller or a test.
func warningCommand(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "peasant"
	}
	return prefix
}

// harnessName names a harness in prose, saying so when the store recorded none.
func harnessName(harness Harness) string {
	if harness == "" {
		return "an unrecorded harness"
	}
	return string(harness)
}

// warningShortID abbreviates an id for prose, keeping the four characters the
// miss messages pin. A suggested command always carries the full id.
func warningShortID(id SessionID) string {
	raw := string(id)
	if len(raw) <= 4 {
		return raw
	}
	return raw[:4] + "..."
}
