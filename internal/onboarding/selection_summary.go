package onboarding

import (
	"fmt"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// selectionSummaryLine is the review's line about what was chosen, in plain
// words, written from the selection the save would write and nothing else, so
// the terminal and the web say the same thing:
//
//	selection: 1 project, 1 branch, 2 sessions you chose
//
// It counts what the selection names. A project is counted once however many
// clones or tools name it, a branch once per project, and a session when it is
// named by its id (a branch the selection names covers its sessions without
// listing them). A tool the selection names with no project and no session
// stands for every session it has, and the line says so. A selection that names
// nothing says "nothing chosen".
func selectionSummaryLine(selection config.SelectionConfig) string {
	projects := map[string]struct{}{}
	branches := map[string]struct{}{}
	sessions := map[string]struct{}{}
	var wholeTools []string
	for tool, harness := range selection.Harnesses {
		if len(harness.Projects) == 0 && len(harness.Sessions) == 0 {
			wholeTools = append(wholeTools, tool)
			continue
		}
		for _, project := range harness.Projects {
			identity := selectedProjectIdentity(project)
			if identity == "" {
				continue
			}
			projects[identity] = struct{}{}
			for _, branch := range project.Branches {
				branches[identity+"\x00"+branch] = struct{}{}
			}
		}
		for _, id := range harness.Sessions {
			sessions[id] = struct{}{}
		}
	}
	sort.Strings(wholeTools)
	var parts []string
	for _, tool := range wholeTools {
		parts = append(parts, "every "+tool+" session")
	}
	if n := len(projects); n > 0 {
		parts = append(parts, countOf(n, "project", "projects"))
	}
	if n := len(branches); n > 0 {
		parts = append(parts, countOf(n, "branch", "branches"))
	}
	if n := len(sessions); n > 0 {
		parts = append(parts, countOf(n, "session", "sessions"))
	}
	if len(parts) == 0 {
		return "selection: nothing chosen"
	}
	return "selection: " + strings.Join(parts, ", ") + " you chose"
}

func countOf(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// selectedProjectIdentity says which project an entry names, so two entries for
// one project (two clones of it, or the same repository under two tools) count
// once and two projects that share a name do not: the repository when it has
// one, else the clones it names, else its name.
func selectedProjectIdentity(project config.ProjectSelection) string {
	if remote := ingest.NormalizeRemoteForMatch(project.GitRemote); remote != "" {
		return "remote:" + remote
	}
	if len(project.ClonePaths) > 0 {
		paths := append([]string(nil), project.ClonePaths...)
		sort.Strings(paths)
		return "path:" + strings.Join(paths, "\x00")
	}
	if name := ingest.NormalizeProjectNameForMatch(project.Name); name != "" {
		return "name:" + name
	}
	return ""
}

// retentionSummaryLine is the review's line about Claude Code's retention
// period. The never-expire period is stored as a large number of days, which is
// a way of writing "never", so the line says never.
func retentionSummaryLine(days int) string {
	if NeverExpires(days) {
		return "claude code source retention: never expire"
	}
	return fmt.Sprintf("claude code source retention: %d days", days)
}
