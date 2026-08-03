package githooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// Lifecycle plans, reports, installs, and removes the village upload hooks of
// one repository at a time. Every call resolves the repository afresh through
// its GitResolver, so a report always describes the path Git would really run.
type Lifecycle struct {
	git GitResolver
}

// New returns a Lifecycle backed by git. Production callers pass NewExecGit.
func New(git GitResolver) *Lifecycle {
	return &Lifecycle{git: git}
}

// intent is the operation a plan is being built for. The same slot on disk has
// to be explained differently to someone installing, someone inspecting, and
// someone removing: install wording ("add this section by hand") is wrong and
// confusing when the user is trying to take a hook away.
type intent string

const (
	intentInstall   intent = "install"
	intentStatus    intent = "status"
	intentUninstall intent = "uninstall"
)

// Status reports the current state of the requested events, or of every managed
// event when none are named. It is read-only, and it explains what it finds in
// inspection terms rather than restating what an install would have said.
func (l *Lifecycle) Status(ctx context.Context, req Request) (PlanReport, error) {
	return l.plan(ctx, req, intentStatus, false)
}

// Install writes the village upload hook for each requested event. Events are
// independent: a refusal on one never suppresses the real outcome of another,
// and every event reports what actually happened to its own file.
func (l *Lifecycle) Install(ctx context.Context, req Request) (ChangeReport, error) {
	report, err := l.plan(ctx, req, intentInstall, true)
	if err != nil {
		return ChangeReport{}, err
	}
	results := make([]Result, 0, len(report.Plans))
	for _, plan := range report.Plans {
		results = append(results, l.installOne(plan, report.Repository, req.Binding))
	}
	return ChangeReport{Repository: report.Repository, Results: results}, nil
}

// Uninstall removes the village upload hook for each requested event, or for
// every managed event when none are named. It only ever deletes an intact
// Peasant-generated file; anything else is left exactly as it was.
func (l *Lifecycle) Uninstall(ctx context.Context, req Request) (ChangeReport, error) {
	report, err := l.plan(ctx, req, intentUninstall, false)
	if err != nil {
		return ChangeReport{}, err
	}
	results := make([]Result, 0, len(report.Plans))
	for _, plan := range report.Plans {
		results = append(results, uninstallOne(plan, report.Repository, req.Binding))
	}
	return ChangeReport{Repository: report.Repository, Results: results}, nil
}

// plan resolves the repository and builds one Plan per event.
func (l *Lifecycle) plan(ctx context.Context, req Request, op intent, requireExplicit bool) (PlanReport, error) {
	events, err := normalizeEvents(req.Events, requireExplicit)
	if err != nil {
		return PlanReport{}, err
	}
	requested, err := absoluteDir(req.Dir, "githooks.Lifecycle")
	if err != nil {
		return PlanReport{}, err
	}
	root, err := l.git.Root(ctx, requested)
	if err != nil {
		return PlanReport{}, err
	}
	gitDir, err := l.git.GitDir(ctx, requested)
	if err != nil {
		return PlanReport{}, err
	}
	repo := Repository{RequestedDir: requested, Root: root, GitDir: gitDir}

	plans := make([]Plan, 0, len(events))
	for _, event := range events {
		path, err := l.git.HookPath(ctx, requested, event)
		if err != nil {
			return PlanReport{}, err
		}
		slot, err := inspect(event, path)
		if err != nil {
			return PlanReport{}, err
		}
		// Only a slot that is about to be refused as shared needs an owner, and
		// finding one costs git calls, so the question is asked exactly there.
		owner := ""
		if !privateHookPath(slot, repo) {
			owner = l.hooksDirectoryOwner(ctx, slot)
		}
		plan, err := planFor(slot, repo, req.Binding, op, owner)
		if err != nil {
			return PlanReport{}, err
		}
		plans = append(plans, plan)
	}
	return PlanReport{Repository: repo, Plans: plans}, nil
}

// hooksDirectoryOwner names a repository from which 'peasant village hooks'
// would act on exactly the file this slot describes, or "" when no such
// repository exists.
//
// The refusal for a shared hook path offers "run the command from the
// repository that owns the hooks directory" as one of its options, and that
// option is only an option when such a repository is there. With a global
// core.hooksPath - the ordinary way people share one hooks directory across
// their own repositories - it points at a plain directory owned by nobody, and
// following it answers "<dir> is not inside a Git worktree". It IS correct for a
// linked worktree, whose hooks live in the main worktree's Git directory.
//
// Git cannot be asked directly: run inside a .git directory it reports "this
// operation must be run in a work tree", so the nearest enclosing worktree is
// found by walking up. The candidate is then PROVEN rather than assumed - Git is
// asked, from there, which file it would run for this event. A worktree that
// merely contains the directory but resolves its own hooks elsewhere is not the
// owner, and naming it would send the user somewhere that changes a different
// file.
func (l *Lifecycle) hooksDirectoryOwner(ctx context.Context, slot Slot) string {
	dir := filepath.Dir(slot.Path)
	for {
		if root, err := l.git.Root(ctx, dir); err == nil {
			owned, hookErr := l.git.HookPath(ctx, root, slot.Event)
			if hookErr == nil && sameHookFile(owned, slot.Path) {
				return root
			}
			// The nearest worktree runs a different file for this event, so no
			// repository owns this path; walking further up would only find
			// repositories nested even less closely.
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// sameHookFile reports whether two hook paths name the same file, comparing
// through symlinks so an indirected hooks directory is not mistaken for a
// different one.
func sameHookFile(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b) || resolveExisting(a) == resolveExisting(b)
}

// inspect reads the current state of one hook slot.
//
// A symlink, and any other non-regular file, is foreign: Peasant never rewrites
// or deletes something that is not a plain file it wrote. But foreign is a
// verdict about MUTATION, not an excuse to stop reading. Git executes whatever
// a link points at, so the two questions that decide the advice - what
// interpreter runs this slot, and whether a village upload already runs from it
// - are answered from the TARGET's bytes. Returning early on the link instead
// left both unanswered: an unclassified slot was treated as a shell, so a
// symlinked python hook was offered a POSIX shell section that turns it into a
// syntax error and blocks every push; and a symlinked generated hook was
// reported as inert while it uploaded on every commit.
//
// When those bytes cannot be read at all - a dangling link, a link to a
// directory, a file Peasant may not open - nothing is classified and the slot
// is marked unreadable, which fails every content-dependent offer closed.
func inspect(event Event, path string) (Slot, error) {
	slot := Slot{Event: event, Path: path, Ownership: OwnershipAbsent}

	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return slot, nil
	case err != nil:
		return Slot{}, fmt.Errorf(
			"githooks: cannot inspect the %s hook at %s\n"+
				"What went wrong: %v\n"+
				"Why: the path could not be stat'd, usually a permission problem on the hooks directory.\n"+
				"Where: githooks.inspect.\n"+
				"When: while reading the current hook state, before anything was written.\n"+
				"Impact: nothing was planned, installed, or removed for this event.\n"+
				"Fix: make the hooks directory readable, then retry.",
			event, path, err,
		)
	}

	slot.Mode = info.Mode()
	slot.Size = info.Size()

	if info.Mode()&os.ModeSymlink != 0 {
		slot.Ownership = OwnershipForeign
		slot.LinkTarget = linkTarget(path)
		// Open first and then stat the opened descriptor. A separate Stat followed
		// by ReadFile leaves a race in which the link can be repointed to a FIFO or
		// device between the two calls and make inspection block forever. A
		// dangling link, a directory, a device, and an unreadable target all mean
		// the same thing here: nothing was classified, so every content-dependent
		// offer fails closed.
		content, targetInfo, readable := readRegularTarget(path)
		if !readable {
			slot.Unreadable = true
			return slot, nil
		}
		// Git follows the link before deciding whether the hook is executable.
		// Keeping the link's synthetic lrwxrwxrwx mode here would report a
		// non-executable target as actively uploading.
		slot.Mode = targetInfo.Mode()
		slot.Size = targetInfo.Size()
		classifyForeign(&slot, content)
		return slot, nil
	}
	if !info.Mode().IsRegular() {
		// A fifo, socket, or device. There are no bytes to classify, so every
		// content-dependent offer fails closed rather than guessing.
		slot.Ownership = OwnershipForeign
		slot.Unreadable = true
		return slot, nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return Slot{}, fmt.Errorf(
			"githooks: cannot read the existing %s hook at %s\n"+
				"What went wrong: %v\n"+
				"Why: ownership is decided by the file's contents, so an unreadable file cannot be classified.\n"+
				"Where: githooks.inspect.\n"+
				"When: while reading the current hook state, before anything was written.\n"+
				"Impact: nothing was planned, installed, or removed for this event; the file is untouched.\n"+
				"Fix: make the file readable, or inspect and remove it yourself, then retry.",
			event, path, err,
		)
	}
	if IsManaged(content) {
		slot.Ownership = OwnershipPeasant
		slot.EmbeddedRoot = EmbeddedRepository(content)
		slot.EmbeddedCommand = EmbeddedCommand(content)
		return slot, nil
	}
	slot.Ownership = OwnershipForeign
	classifyForeign(&slot, content)
	return slot, nil
}

// readRegularTarget reads through a symlink only after proving the opened object
// is a regular file. It is read-only by construction and closes the descriptor
// on every path.
//
// It FOLLOWS a symlink deliberately, because its caller is inspection, whose
// question is "what will Git run" - and Git follows the link before it executes
// anything. The re-check that runs before a write or a delete asks the opposite
// question and uses readRegularSlot instead.
func readRegularTarget(path string) ([]byte, fs.FileInfo, bool) {
	return readRegularOpened(path, 0)
}

// readRegularSlot reads path only when path ITSELF is a regular file, refusing a
// symlink rather than reading what it points at.
//
// The distinction is not cosmetic. Ownership is permission to rewrite and
// delete, and Peasant never follows a link to do either: a slot swapped to a
// symlink whose target happens to be a byte-perfect generated hook is still a
// file the user made, and acting on it destroys their link. Following the link
// there also makes the ownership test answer about the wrong file entirely.
func readRegularSlot(path string) ([]byte, bool) {
	content, _, readable := readRegularOpened(path, readSlotNoFollowFlag)
	return content, readable
}

// readRegularOpened opens path with extra flags, proves the OPENED object is a
// regular file before reading a byte of it, and closes the descriptor on every
// path.
//
// Proving regularity on the descriptor rather than on a prior stat is what makes
// it safe to use in a race: a separate Stat followed by a read leaves a window in
// which the path can be repointed at a FIFO or a device between the two calls.
func readRegularOpened(path string, extraFlags int) ([]byte, fs.FileInfo, bool) {
	// O_NONBLOCK matters before the descriptor can be inspected: opening a FIFO
	// read-only otherwise waits forever for a writer. It has no effect on regular
	// files, which are the only objects read below.
	file, err := os.OpenFile(path, os.O_RDONLY|readTargetNonblockFlag|extraFlags, 0)
	if err != nil {
		return nil, nil, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil, false
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, false
	}
	return content, info, true
}

// classifyForeign records what Git runs a foreign slot AS, and whether a village
// upload already runs from it. It is applied to a plain foreign file and to a
// symlink's target alike, because Git executes the same bytes either way.
//
// A symlinked slot is never promoted to OwnershipPeasant even when its target is
// a byte-perfect generated hook: ownership is permission to rewrite and delete,
// and Peasant does not follow a link to do either. It is recorded as a generated
// remnant instead, which is what makes status and uninstall say - truthfully -
// that an upload runs from a file only the user can take away.
func classifyForeign(slot *Slot, content []byte) {
	// What Git runs this file AS decides whether the by-hand section can be
	// offered for it at all. The bytes are already in hand, so the shebang
	// costs nothing to read - and getting this wrong destroys a working hook
	// rather than merely failing to help.
	slot.Language, slot.Interpreter = ClassifyHookLanguage(content)
	slot.HandAdded = ContainsManualSection(content)
	// A generated hook whose framing was broken is foreign - Peasant will not
	// rewrite or delete it - but its upload line is still in the file and still
	// runs, so it must never be reported as nothing being there.
	slot.GeneratedRemnant = ContainsGeneratedSection(content)
}

// linkTarget names the file a symlinked slot resolves to, absolute, following
// the whole chain when it can. A link that resolves to nothing - dangling, or a
// cycle - still has a first hop worth naming, because that is the path the user
// has to look at; if even that cannot be read the slot path is the best
// available answer and the caller has already marked it unreadable.
func linkTarget(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	hop, err := os.Readlink(path)
	if err != nil {
		return path
	}
	if !filepath.IsAbs(hop) {
		hop = filepath.Join(filepath.Dir(path), hop)
	}
	return filepath.Clean(hop)
}

// planFor decides what the requested operation would do with a slot, and
// explains it in that operation's own words. owner is the repository that runs
// a shared hook path, or "" when none does; it is ignored for a private path.
func planFor(slot Slot, repo Repository, binding Binding, op intent, owner string) (Plan, error) {
	plan := Plan{Slot: slot}
	if !privateHookPath(slot, repo) {
		plan.Action = ActionRefuse
		plan.Refusal = RefusalSharedPath
		plan.Reason = sharedPathReason(slot, repo, binding, op, owner)
		return plan, nil
	}
	switch slot.Ownership {
	case OwnershipForeign:
		plan.Action = ActionRefuse
		plan.Refusal = RefusalForeignFile
		plan.Reason = foreignFileReason(slot, repo, binding, op)
		// The by-hand section is offered only where pasting it is actually
		// correct: a slot inside this repository, run by this repository alone,
		// that is not already carrying any Peasant upload section,
		// and that git runs as a shell script at all. Uninstall never offers it
		// - the user is removing an upload, not adding one - and neither does a
		// slot whose broken generated block already uploads, where a second
		// section is the last thing anyone needs. A hook run by another
		// interpreter is refused the section outright: pasting POSIX shell into
		// a python or ruby hook does not fail to help, it makes that hook a
		// syntax error and blocks every push. So is a slot whose contents could
		// not be read, and a symlink into a file outside the hooks directory,
		// which is not this repository's to have an upload pinned into it.
		//
		// The section is addressed to the file git really executes: appending
		// to a symlinked slot appends to its target, so naming the link would
		// tell the user to edit a file that is not the one that changes.
		if op != intentUninstall && !slot.CarriesUploadSection() && slot.AcceptsShellSection() {
			manual, err := ManualSnippet(slot.Event, repo.Root, slot.FileToEdit(), binding)
			if err != nil {
				return Plan{}, err
			}
			plan.Manual = manual
		}
		return plan, nil
	case OwnershipPeasant:
		plan.Action = ActionReplace
	default:
		plan.Action = ActionCreate
	}
	// Gathered BEFORE the reason, because the reason has to agree with them: a
	// hook that lost its executable bit is present and inert, and saying "git
	// runs it" beside a warning that git skips it makes the report contradict
	// itself in the same paragraph.
	plan.Warnings = slotWarnings(slot, repo, binding)
	plan.Reason = manageableReason(slot, repo, binding, op, plan.Warnings)
	script, err := Script(slot.Event, repo.Root, slot.Path, binding)
	if err != nil {
		return Plan{}, err
	}
	plan.Script = script
	return plan, nil
}

// privateHookPath reports whether the file git runs for this slot belongs to
// this repository alone.
//
// The question is about the DIRECTORY git runs hooks from, not about where the
// slot file itself points. A symlink in an ordinary .git/hooks is content
// Peasant did not write - foreign - and answering it with "your hooks directory
// is outside the worktree" both misdiagnoses it and withholds the by-hand
// snippet, which is exactly the remedy that applies.
//
// Two directories qualify, and BOTH are needed. The repository's own Git
// directory is private by construction: nothing but this repository runs the
// hooks in it. The worktree covers a repository-local custom hooks path, which
// is committable but still this repository's. Testing only the worktree refused
// every submodule, because git runs a submodule's hooks from
// <parent>/.git/modules/<sub>/hooks - outside the submodule's worktree, yet that
// submodule's own private Git directory - and the refusal then told the user to
// run the command from the repository that owns it, which is the repository they
// were already standing in. A linked worktree is the genuinely shared case and
// still fails both tests: its own Git directory is <main>/.git/worktrees/<name>,
// while the hooks it runs live in <main>/.git/hooks. A core.hooksPath outside
// the repository fails both too.
func privateHookPath(slot Slot, repo Repository) bool {
	hooksDir := filepath.Dir(slot.Path)
	if repo.GitDir != "" && pathWithin(repo.GitDir, hooksDir) {
		return true
	}
	return pathWithin(repo.Root, hooksDir)
}

// pathWithin reports whether path lies inside root once BOTH sides are resolved
// through symlinks.
//
// Comparing the unresolved strings is wrong in both directions. Root() returns
// Git's physical top level while HookPath() stays anchored to the directory the
// caller asked about, so a repository reached through a symlinked path would be
// refused for a hook that is genuinely inside it. And a symlinked hooks
// directory would place the write outside the repository this check exists to
// keep it inside.
func pathWithin(root, path string) bool {
	resolvedRoot := resolveExisting(root)
	resolvedPath := resolveExisting(path)
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveExisting resolves the longest existing prefix of p through symlinks and
// re-attaches the part that does not exist yet. A hook file - and sometimes the
// whole hooks directory - has not been created at the time of the check, so
// EvalSymlinks cannot be applied to p as a whole.
func resolveExisting(p string) string {
	current := filepath.Clean(p)
	pending := ""
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, pending)
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Nothing along the path exists; the lexical form is the best
			// available answer and keeps the comparison total.
			return filepath.Clean(p)
		}
		pending = filepath.Join(filepath.Base(current), pending)
		current = parent
	}
}

// sharedPathReason explains a slot Git runs from outside the resolved
// repository. That file belongs to more than this repository - a linked
// worktree shares its main worktree's hooks directory, and an outside
// core.hooksPath is shared by every repository configured to use it - so
// repository-specific consent cannot be honored there and no by-hand snippet is
// offered for it: pasting an upload pinned to one repository into a file every
// other one also runs is exactly the leak this refusal exists to prevent.
func sharedPathReason(slot Slot, repo Repository, binding Binding, op intent, owner string) string {
	shared := fmt.Sprintf(
		"What went wrong: git runs %s for the %s event of repository %s, and its hooks directory %s is outside %s.\n"+
			"Why: a hooks directory outside the worktree is shared - every worktree and every repository whose configuration resolves to it runs that same file - so a choice made for one repository cannot be confined to it.\n"+
			"Where: githooks.planFor, checking the effective hook path git itself reported.\n",
		slot.Path, slot.Event, repo.Root, sharedHooksDirectory(slot), repo.Root)
	switch op {
	case intentStatus:
		return shared + fmt.Sprintf(
			"When: while inspecting the current state; nothing was read from or written to that file beyond its contents.\n"+
				"Impact: %s\n"+
				"Fix: %s Peasant never sets, unsets, or changes core.hooksPath, so pointing git at a hooks directory inside this worktree is your decision to make; after that, 'peasant village hooks install' can manage it here.",
			sharedPathState(slot), ownerInspectFix(slot, owner, binding))
	case intentUninstall:
		return shared + fmt.Sprintf(
			"When: while uninstalling, before anything was removed.\n"+
				"Impact: nothing was removed and %s\n"+
				"Fix: %s Removing it there removes it for every worktree and repository that runs it.",
			sharedPathState(slot), ownerRemoveFix(slot, owner, binding))
	default:
		return shared + fmt.Sprintf(
			"When: before any hook file was created, rewritten, or removed.\n"+
				"Impact: nothing was installed and the effective path is untouched. Do not paste an upload section into %s: it is pinned to %s, so it would run for every worktree and repository that executes that file and would still upload only %s.\n"+
				"Fix: pick one - run the upload yourself when you want it (cd %s && %s);%s or point git at a hooks directory inside this worktree yourself and re-run this command. Peasant never sets, unsets, or changes core.hooksPath.",
			slot.Path, repo.Root, repo.Root,
			shellQuote(repo.Root), ManualCommand(repo.Root, binding), ownerInstallOption(slot, owner, binding))
	}
}

// ownerInstallOption is the "do it from the repository that owns the file"
// option, offered ONLY when such a repository exists.
//
// A global core.hooksPath pointing at a plain directory has no owner, and that
// is the ordinary way people share hooks between their own repositories. Listing
// the option there put a command in a menu of three that answers
// "<dir> is not inside a Git worktree" - a wrong entry beside two that work.
func ownerInstallOption(slot Slot, owner string, binding Binding) string {
	if owner == "" {
		return ""
	}
	return fmt.Sprintf(
		" or run %s, which is the repository git really runs that file for, so the consent matches it;",
		InstallCommandWithBinding(slot.Event, owner, binding))
}

// ownerInspectFix is the status counterpart: inspect the shared file from the
// repository that runs it, when one exists.
func ownerInspectFix(slot Slot, owner string, binding Binding) string {
	if owner == "" {
		return fmt.Sprintf(
			"no repository owns %s - it is a plain directory git was pointed at - so there is nothing to inspect it from; open that file yourself to see what runs.",
			filepath.Dir(slot.Path))
	}
	return fmt.Sprintf("run %s to inspect or change it there.", StatusCommandWithBinding(owner, binding))
}

// ownerRemoveFix is the uninstall counterpart.
func ownerRemoveFix(slot Slot, owner string, binding Binding) string {
	if owner == "" {
		return fmt.Sprintf(
			"no repository owns %s - it is a plain directory git was pointed at - so no Peasant command reaches it; delete that file yourself.",
			filepath.Dir(slot.Path))
	}
	return fmt.Sprintf("run %s, or delete that file yourself.", UninstallCommandWithBinding(slot.Event, owner, binding))
}

// sharedHooksDirectory names the directory the refusal was actually decided on,
// and says where it resolves to when that differs.
//
// The decision is made on the resolved path, so printing only the unresolved one
// produces a message claiming that a file lexically inside the repository is
// outside it - a self-contradiction that hides the symlink or gitdir indirection
// which is the real cause.
func sharedHooksDirectory(slot Slot) string {
	dir := filepath.Clean(filepath.Dir(slot.Path))
	resolved := resolveExisting(dir)
	if resolved == dir {
		return dir
	}
	return fmt.Sprintf("%s (which resolves to %s)", dir, resolved)
}

// sharedPathState states, in one clause, what is actually in a shared slot, so
// status and uninstall report the file rather than an installation verdict.
func sharedPathState(slot Slot) string {
	switch slot.Ownership {
	case OwnershipPeasant:
		if !executableByGit(slot.Mode) {
			return fmt.Sprintf(
				"that file is a Peasant-generated hook, installed from %s, but git does not run it because it is not executable. It would run for every worktree and repository that executes this shared slot if its executable mode were restored.",
				embeddedRootOrUnknown(slot))
		}
		return fmt.Sprintf(
			"that file is a Peasant-generated hook, installed from %s; it runs for every worktree and repository that executes it.",
			embeddedRootOrUnknown(slot))
	case OwnershipForeign:
		if slot.UploadsFromForeignFile() {
			return "that file was not written by Peasant - or is no longer intact - but it still carries a village upload section, so uploads run from it for every worktree and repository that executes it. It is left exactly as it is."
		}
		if slot.CarriesUploadSection() {
			return "that file was not written by Peasant - or is no longer intact - and it carries a village upload section, but git does not currently run it because it is not executable. It is left exactly as it is; restoring its executable mode would make the upload run for every worktree and repository that executes this shared slot."
		}
		return "that file was not written by Peasant and is left exactly as it is."
	default:
		return "no file exists there, so nothing runs for this event."
	}
}

// embeddedRootOrUnknown names the repository a managed hook was written for.
func embeddedRootOrUnknown(slot Slot) string {
	if slot.EmbeddedRoot == "" {
		return "a repository the file does not name"
	}
	return slot.EmbeddedRoot
}

// foreignFileReason explains, in the six dimensions, why a file Peasant did not
// write is left alone, phrased for the operation that found it.
func foreignFileReason(slot Slot, repo Repository, binding Binding, op intent) string {
	active := "no upload hook is active for this event"
	switch {
	case slot.CarriesUploadSection() && !slot.UploadsFromForeignFile():
		active = fmt.Sprintf(
			"an upload section is present in %s, but no upload is active because git does not run that file without an executable mode; restoring that mode would activate it",
			slot.FileToEdit())
	case slot.GeneratedRemnant:
		active = fmt.Sprintf(
			"a Peasant-generated upload section is still in %s with its framing no longer intact, so uploads are running from it",
			slot.FileToEdit())
	case slot.HandAdded:
		active = fmt.Sprintf(
			"the by-hand upload section is already in %s, so uploads are running from it",
			slot.FileToEdit())
	case slot.Unreadable:
		// Nothing was classified, so nothing may be claimed about what runs.
		active = "Peasant could not read what git executes here, so it cannot say whether an upload runs from it"
	}
	var when, fix string
	switch op {
	case intentStatus:
		when = "while inspecting the current state, without changing anything"
		switch {
		case slot.GeneratedRemnant:
			fix = removeGeneratedSectionFix(slot, repo, binding, "to stop those uploads")
		case slot.HandAdded:
			fix = fmt.Sprintf("nothing to do: %s already carries the by-hand upload section", slot.FileToEdit())
		case !slot.AcceptsShellSection():
			fix = "nothing to do unless you want an upload here, and " + withheldSectionFix(slot, repo, binding)
		default:
			fix = fmt.Sprintf("nothing to do unless you want an upload here: the section below is what you would add to %s by hand", slot.FileToEdit())
		}
	case intentUninstall:
		when = "while uninstalling, before anything was removed"
		fix = fmt.Sprintf("nothing of Peasant's is there to remove; leave %s as it is, or edit it yourself", slot.FileToEdit())
	default:
		when = fmt.Sprintf("while planning the %s hook, before anything was written", slot.Event)
		switch {
		case slot.GeneratedRemnant:
			fix = removeGeneratedSectionFix(slot, repo, binding, "do not add a second upload section")
		case slot.HandAdded:
			fix = fmt.Sprintf("nothing to add: %s already carries the by-hand upload section", slot.FileToEdit())
		case !slot.AcceptsShellSection():
			fix = withheldSectionFix(slot, repo, binding)
		default:
			fix = fmt.Sprintf("add the section below to %s by hand, or move that file aside yourself and re-run install", slot.FileToEdit())
		}
	}
	return fmt.Sprintf(
		"What went wrong: %s.\n"+
			"Why: Peasant only rewrites files it generated, and this one is not framed as one.\n"+
			"Where: repository %s.\n"+
			"When: %s.\n"+
			"Impact: the existing file is untouched - not edited, wrapped, renamed, chmod'd, or deleted - and %s.\n"+
			"Fix: %s.",
		foreignSlotDescription(slot), repo.Root, when, active, fix,
	)
}

// foreignSlotDescription says what is actually in the slot, naming the file git
// really executes when a link stands between the two. A user told only about
// '.git/hooks/pre-push' when that is a symlink into their dotfiles has been
// pointed at the wrong file for every instruction that follows.
func foreignSlotDescription(slot Slot) string {
	if slot.LinkTarget != "" {
		return fmt.Sprintf(
			"the %s hook at %s is a symlink to %s, which Peasant did not write",
			slot.Event, slot.Path, slot.LinkTarget)
	}
	return fmt.Sprintf(
		"a %s hook that Peasant did not write is already at %s", slot.Event, slot.Path)
}

// withheldSectionFix is the advice for every slot the by-hand POSIX shell
// section is NOT offered for, and it names which of the reasons applies.
//
// It leads with what must not happen. The section is POSIX shell, and the widely
// used pre-commit framework installs a '#!/usr/bin/env python3' hook; pasting
// the section into that file does not merely fail to work, it makes the file a
// syntax error - 'SyntaxError: invalid decimal literal' on every pre-push, which
// blocks the push outright, and a silent death on every post-commit. So the
// section is never offered here, and the things that do work are named instead:
// take the file out of the way, or add the upload in that hook's own language,
// which is a single argument list with no shell syntax in it.
func withheldSectionFix(slot Slot, repo Repository, binding Binding) string {
	target := slot.FileToEdit()
	switch {
	case slot.Unreadable:
		if slot.LinkTarget != "" {
			return fmt.Sprintf(
				"Peasant could not read what git runs for this event through %s, so it cannot tell which interpreter that file needs and will NOT offer its POSIX shell section - offering one for a hook another interpreter runs turns that hook into a syntax error and blocks every %s. "+
					"Inspect the link and its target yourself (%s -> %s). If the link is broken or unwanted, delete only the link with: rm %s; then run %s for a hook Peasant manages. If the target is a hook you want to keep, make it readable and re-run status before adding anything",
				slot.Path, slot.Event, slot.Path, target, shellQuote(slot.Path), InstallCommandWithBinding(slot.Event, repo.Root, binding))
		}
		return fmt.Sprintf(
			"Peasant could not read what git runs for this event through %s, so it cannot tell which interpreter that file needs and will NOT offer its POSIX shell section - offering one for a hook another interpreter runs turns that hook into a syntax error and blocks every %s. "+
				"Open %s yourself to see what is there: if it is broken or unwanted, remove it and run %s for a hook Peasant manages; if it is a hook you want to keep, add the upload in that file's own language, as an argument list with no shell quoting: %s",
			slot.Path, slot.Event, target, InstallCommandWithBinding(slot.Event, repo.Root, binding), ArgumentList(RepositoryArgv(repo.Root, binding)))
	case slot.Language == HookLanguageBinary:
		if slot.LinkTarget != "" {
			return fmt.Sprintf(
				"git reaches this event through %s, which points at %s. The target is not a text script, so no upload section or interpreter argument list can be added to it. "+
					"Leave the target untouched; if you want a hook Peasant manages for this repository, delete only the link with: rm %s; then run %s",
				slot.Path, target, shellQuote(slot.Path), InstallCommandWithBinding(slot.Event, repo.Root, binding))
		}
		return fmt.Sprintf(
			"%s is not a text script, so no upload section can be added to it at all - move that file aside yourself and re-run %s, which then writes a hook Peasant manages",
			target, InstallCommandWithBinding(slot.Event, repo.Root, binding))
	case slot.LinkLeavesHooksDirectory():
		invocation := fmt.Sprintf(
			"start the upload as an argument list with no shell quoting: %s",
			ArgumentList(RepositoryArgv(repo.Root, binding)))
		if slot.Language == HookLanguagePOSIXShell {
			invocation = fmt.Sprintf(
				"run this POSIX shell command: %s",
				RepositoryCommand(repo.Root, binding))
		}
		return fmt.Sprintf(
			"git reaches this event through a symlink that leaves the hooks directory: %s points at %s. "+
				"Peasant will NOT offer its by-hand section for that file - it is not this repository's hooks directory content, and an upload section pinned to %s would run for everyone who adopts it, from wherever they adopt it. "+
				"Pick one: replace the link with a hook of this repository's own by deleting %s and running %s; or, if %s really is meant to serve this repository alone, add the upload to it yourself in the language it is written in: %s",
			slot.Path, target, repo.Root,
			shellQuote(slot.Path), InstallCommandWithBinding(slot.Event, repo.Root, binding), target, invocation)
	}
	interpreter := slot.Interpreter
	if interpreter == "" {
		interpreter = "an interpreter that is not a shell"
	}
	return fmt.Sprintf(
		"git runs %s with %s, not a POSIX shell, so Peasant's by-hand shell section is NOT offered for it and must not be pasted in - that would leave the file a syntax error and every %s would fail. "+
			"Pick one instead: move that file aside yourself and re-run %s, which then writes a hook Peasant manages; or, from inside that %s hook, start the upload the way %s starts a program - as an argument list, with no shell quoting on any element: %s",
		hookRunPhrase(slot), interpreter, slot.Event,
		InstallCommandWithBinding(slot.Event, repo.Root, binding), interpreter, interpreter, ArgumentList(RepositoryArgv(repo.Root, binding)))
}

// hookRunPhrase names the file git executes for a slot, disclosing the link when
// there is one so "git runs X with python3" is not said about a symlink whose
// own bytes are a path.
func hookRunPhrase(slot Slot) string {
	if slot.LinkTarget != "" {
		return fmt.Sprintf("%s (a symlink to %s)", slot.Path, slot.LinkTarget)
	}
	return slot.Path
}

// removeGeneratedSectionFix is the one recovery for a generated block whose
// framing was broken: only the user can take it out, so name the exact lines,
// the file, and what to do with whatever is left.
//
// The two outcomes of that deletion need different endings, and collapsing them
// closed a loop. "Delete the section, then run install" is right only when the
// file is gone; when the file still holds the user's own hook, install refuses
// it again - correctly, it is still a foreign file - and hands back the by-hand
// section, which is where the advice should have pointed in the first place.
func removeGeneratedSectionFix(slot Slot, repo Repository, binding Binding, lead string) string {
	target := slot.FileToEdit()
	ending := fmt.Sprintf(
		"If that empties the file, delete it too and run %s for a hook Peasant manages. "+
			"If the file still holds hooks of your own, keep it and re-run %s: with the generated section gone it refuses the file again - it is not Peasant's to rewrite - and prints the by-hand section to paste into what remains",
		InstallCommandWithBinding(slot.Event, repo.Root, binding), StatusCommandWithBinding(repo.Root, binding))
	if !slot.AcceptsShellSection() {
		ending = fmt.Sprintf(
			"If that empties the file, delete it too and run %s for a hook Peasant manages. "+
				"If the file still holds hooks of your own, keep it and add the upload the way that hook's own interpreter starts a program - as an argument list, with no shell quoting on any element: %s",
			InstallCommandWithBinding(slot.Event, repo.Root, binding), ArgumentList(RepositoryArgv(repo.Root, binding)))
	}
	// The file to open is the one git executes. Deleting the section out of a
	// symlink means editing its target; deleting the link itself only removes
	// the pointer and leaves the upload in place for anything else that runs
	// that file.
	linkNote := ""
	if slot.LinkTarget != "" {
		if slot.UploadsFromForeignFile() {
			linkNote = fmt.Sprintf(
				" (%s is a symlink to it; removing the link alone leaves the upload section in %s, where anything else pointing at it still runs)",
				slot.Path, target)
		} else {
			linkNote = fmt.Sprintf(
				" (%s is a symlink to it; removing the link alone leaves the dormant upload section in %s, where it will run if that file's executable mode is restored)",
				slot.Path, target)
		}
	}
	return fmt.Sprintf(
		"%s - open %s%s and delete the lines from '%s' to '%s' yourself. %s",
		lead, target, linkNote, ScriptMarkerBegin, ScriptMarkerEnd, ending)
}

// manageableReason explains a slot Peasant may act on, in the words of the
// operation that is looking at it. Status describes what git runs today;
// install describes what it is about to do.
//
// It is told the warnings that were already gathered, because the primary
// sentence must not contradict them. A hook that lost its executable bit is
// present and INERT - git skips it and says so in a hint - so reporting "git
// runs the Peasant-generated hook here" beside a warning that git skips it
// makes the same paragraph assert both.
func manageableReason(slot Slot, repo Repository, binding Binding, op intent, warnings []Warning) string {
	managed := slot.Ownership == OwnershipPeasant
	if op == intentStatus {
		if managed {
			if hasWarning(warnings, WarningHookNotExecutable) {
				return fmt.Sprintf(
					"A Peasant-generated %s hook is at %s, but git does not run it: see the warning below.",
					slot.Event, slot.Path)
			}
			return fmt.Sprintf(
				"Git runs the Peasant-generated %s hook at %s for this repository.", slot.Event, slot.Path)
		}
		return fmt.Sprintf(
			"No file exists at %s, so git runs no %s hook for this repository. Install one with: %s",
			slot.Path, slot.Event, InstallCommandWithBinding(slot.Event, repo.Root, binding))
	}
	if managed {
		return fmt.Sprintf("Peasant wrote the file at %s, so it can rewrite it in place.", slot.Path)
	}
	return fmt.Sprintf("No file exists at %s, so Peasant can install its hook there.", slot.Path)
}

// hasWarning reports whether a disclosure of this kind was gathered.
func hasWarning(warnings []Warning, kind WarningKind) bool {
	for _, warning := range warnings {
		if warning.Kind == kind {
			return true
		}
	}
	return false
}

// slotWarnings collects the non-blocking disclosures that apply to a slot
// Peasant can manage. They are attached to plans and to successful results,
// because both are moments where the user is deciding whether to keep the hook.
func slotWarnings(slot Slot, repo Repository, binding Binding) []Warning {
	var warnings []Warning
	// Only a slot that actually holds a hook is shared with anything. Saying
	// "git runs THIS same hook file for all of them" about an empty slot
	// describes a file that is not there.
	if shared := linkedWorktrees(repo); slot.Ownership == OwnershipPeasant && len(shared) > 0 {
		warnings = append(warnings, Warning{
			Kind: WarningSharedWithLinkedWorktrees,
			Detail: fmt.Sprintf(
				"%s also has %d linked worktree(s) (%s), and git runs THIS same hook file for all of them. "+
					"Committing in any of them triggers an upload, and every one of those uploads is the one pinned here: "+
					"only %s's own recorded sessions are ever sent, never another worktree's. "+
					"List them with 'git worktree list'; remove the hook for all of them at once with: %s",
				repo.Root, len(shared), strings.Join(shared, ", "), repo.Root,
				UninstallCommandWithBinding(slot.Event, repo.Root, binding)),
		})
	}
	if repo.GitDir != "" && !pathWithin(repo.GitDir, slot.Path) {
		warnings = append(warnings, Warning{
			Kind: WarningCommittableHook,
			Detail: fmt.Sprintf(
				"%s is inside the working tree, not inside %s, so git can track it. "+
					"Committing it publishes an upload hook that runs for everyone who checks the repository out and adopts that hooks directory, "+
					"and it carries this machine's absolute paths. "+
					"Add it to .gitignore or .git/info/exclude if you meant this hook to stay yours.",
				slot.Path, repo.GitDir),
		})
	}
	// Whether git runs this file at all is settled once, here, so the two
	// disclosures below cannot disagree with each other about it.
	runs := executableByGit(slot.Mode)
	if slot.Ownership == OwnershipPeasant && slot.EmbeddedRoot != "" && slot.EmbeddedRoot != repo.Root {
		consequence := fmt.Sprintf(
			"The hook still runs, and its upload fails on every %s because the repository it names is not there.", slot.Event)
		if !runs {
			// Saying "the hook still runs" next to the not-executable warning
			// below, which says git skips it, would make one report assert both.
			consequence = fmt.Sprintf(
				"git is not running this file at all right now (see the mode warning below), so nothing fails and nothing uploads; if the mode is restored without refreshing the hook, its upload then fails on every %s because the repository it names is not there.", slot.Event)
		}
		warnings = append(warnings, Warning{
			Kind: WarningRepositoryMoved,
			Detail: fmt.Sprintf(
				"the installed %s hook was written for %s, but git resolves this repository as %s. "+
					"%s "+
					"Refresh it with: %s",
				slot.Event, slot.EmbeddedRoot, repo.Root, consequence, InstallCommandWithBinding(slot.Event, repo.Root, binding)),
		})
	}
	// Git refuses to run a hook without the executable bit and says so in a
	// hint. Reporting the file as installed and active while git is skipping it
	// describes uploads that are not happening, so the one fact that decides it
	// - a mode already read during inspection - is checked rather than assumed.
	if slot.Ownership == OwnershipPeasant && !runs {
		warnings = append(warnings, Warning{
			Kind: WarningHookNotExecutable,
			Detail: fmt.Sprintf(
				"%s has mode %04o, which carries no executable bit, so git skips it with "+
					"\"hint: The '%s' hook was ignored because it's not set as executable\" and nothing is uploaded. "+
					"Restore it with: chmod +x %s   (or re-run %s, which rewrites the file with the mode git needs)",
				slot.Path, slot.Mode.Perm(), slot.Event, shellQuote(slot.Path), InstallCommandWithBinding(slot.Event, repo.Root, binding)),
		})
	}
	return warnings
}

// linkedWorktreesShown caps how many linked worktrees a disclosure names before
// it stops listing them. The count is the fact that matters; a handful of names
// is enough to recognise which ones, and a repository can have dozens.
const linkedWorktreesShown = 3

// linkedWorktrees names the worktrees, other than this one, that share this
// repository's hooks directory and therefore run the hook being written.
//
// Installing FROM a linked worktree is refused with a full explanation, while
// installing from the main worktree arms every linked worktree silently - the
// asymmetry the disclosure closes. It is read from the Git directory rather than
// from 'git worktree list' because a name-only listing is one cheap directory
// read on a path that is otherwise pure, and because a hook is being written for
// THIS repository: the question is only whether other worktrees exist.
func linkedWorktrees(repo Repository) []string {
	if repo.GitDir == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(repo.GitDir, "worktrees"))
	if err != nil {
		// No linked worktrees, or a Git directory that cannot be read. Neither
		// is a failure of the install, and neither may invent a disclosure.
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if len(names) == linkedWorktreesShown {
			names = append(names, fmt.Sprintf("and %d more", len(entries)-linkedWorktreesShown))
			break
		}
		names = append(names, entry.Name())
	}
	return names
}

// executableByGit reports whether git would run a file with this mode.
//
// Windows does not carry POSIX permission bits - the ones os.Stat reports there
// are synthesized - and Git for Windows runs hooks through its own shell without
// consulting them, so the question is not asked there and no warning is invented.
func executableByGit(mode fs.FileMode) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	return mode.Perm()&0o111 != 0
}

// installOne performs one event's mutation, re-verifying ownership immediately
// before touching the file.
func (l *Lifecycle) installOne(plan Plan, repo Repository, binding Binding) Result {
	result := Result{Slot: plan.Slot, Reason: plan.Reason}
	switch plan.Action {
	case ActionRefuse:
		result.Outcome = OutcomeRefused
		result.Refusal = plan.Refusal
		result.Manual = plan.Manual
		return result

	case ActionCreate:
		if err := createHook(plan.Path, plan.Script, repo.Root, binding); err != nil {
			result.Outcome = OutcomeFailed
			result.Err = err
			result.Reason = err.Error()
			return result
		}
		result.Outcome = OutcomeCreated
		result.Reason = installSuccessReason("Installed", plan, repo, binding)
		// A fresh write names the repository Git resolves now, so only the
		// committable-hook disclosure can still apply.
		result.Warnings = writtenHookWarnings(plan.Slot, repo, binding)
		return result

	case ActionReplace:
		if err := replaceHook(plan.Path, plan.Script); err != nil {
			result.Outcome = OutcomeFailed
			result.Err = err
			result.Reason = err.Error()
			return result
		}
		result.Outcome = OutcomeReplaced
		result.Reason = installSuccessReason("Rewrote the Peasant-generated", plan, repo, binding)
		result.Warnings = writtenHookWarnings(plan.Slot, repo, binding)
		return result

	default:
		result.Outcome = OutcomeFailed
		result.Err = fmt.Errorf("githooks: unknown planned action %q", plan.Action)
		result.Reason = result.Err.Error()
		return result
	}
}

// writtenHookWarnings are the disclosures that survive a successful write.
//
// Two cannot. The repository-moved warning: the file Peasant just wrote names
// the root Git resolves right now. And the not-executable warning: the observed
// slot describes the file as it was BEFORE the write - mode zero when the slot
// was empty - while every write path sets ScriptMode and fails loudly if it
// cannot.
func writtenHookWarnings(slot Slot, repo Repository, binding Binding) []Warning {
	written := slot
	written.EmbeddedRoot = repo.Root
	written.Ownership = OwnershipPeasant
	written.Mode = ScriptMode
	return slotWarnings(written, repo, binding)
}

func installSuccessReason(action string, plan Plan, repo Repository, binding Binding) string {
	return fmt.Sprintf(
		"%s %s hook at %s. It runs %s and never blocks git.",
		action, plan.Event, plan.Path, RepositoryCommand(repo.Root, binding))
}

// uninstallOne removes one event's hook when, and only when, Peasant still owns
// an intact copy of it.
//
// A slot holding an unrelated hook is not a failure: Peasant has nothing there
// to remove, and it says so without touching the file. The cases that DO fail
// are the ones where a village upload is still running from a file Peasant may
// not edit - the by-hand section, or a generated hook whose framing was broken
// by an edit - because reporting "nothing of Peasant's to remove, no upload runs
// from it" there is false and the uploads carry on regardless.
func uninstallOne(plan Plan, repo Repository, binding Binding) Result {
	result := Result{Slot: plan.Slot}
	if plan.Refusal == RefusalSharedPath && plan.Ownership == OwnershipPeasant {
		// The file is a Peasant hook, but it is not this repository's to remove:
		// every worktree and repository resolving to that path runs it.
		result.Outcome = OutcomeRefused
		result.Refusal = plan.Refusal
		result.Reason = plan.Reason
		return result
	}
	switch plan.Ownership {
	case OwnershipAbsent:
		result.Outcome = OutcomeNotPresent
		result.Reason = fmt.Sprintf(
			"Nothing to remove: no file exists at %s.", plan.Path)
		return result

	case OwnershipForeign:
		if plan.Unreadable {
			// Nothing was classified, so "no upload runs from it" would be a
			// claim Peasant has no evidence for - and the file it cannot read
			// is exactly the one that could be uploading.
			result.Outcome = OutcomeRefused
			result.Refusal = RefusalForeignFile
			result.Reason = unreadableUninstallRefusal(plan, repo, binding)
			return result
		}
		if !plan.CarriesUploadSection() {
			result.Outcome = OutcomeNotPresent
			result.Reason = fmt.Sprintf(
				"Nothing of Peasant's to remove: the %s hook file for this repository is %s%s, "+
					"which Peasant did not write, so it was left exactly as it is and no upload runs from it.",
				plan.Event, plan.FileToEdit(), viaLinkPhrase(plan.Slot))
			return result
		}
		result.Outcome = OutcomeRefused
		result.Refusal = RefusalForeignFile
		result.Reason = foreignUploadRefusal(plan, repo, binding)
		return result

	default:
		if err := removeHook(plan.Path); err != nil {
			result.Outcome = OutcomeFailed
			result.Err = err
			result.Reason = err.Error()
			return result
		}
		result.Outcome = OutcomeRemoved
		result.Reason = fmt.Sprintf(
			"Removed the Peasant-generated %s hook at %s.", plan.Event, plan.Path)
		return result
	}
}

// foreignUploadRefusal explains an uninstall that found a live village upload
// inside a file Peasant may not edit, and hands back the three remediation steps
// requires of an ownership mismatch: how to inspect it, how to remove it by
// hand, and how to reinstall a hook Peasant can manage.
func foreignUploadRefusal(plan Plan, repo Repository, binding Binding) string {
	var found, remove []string
	if plan.GeneratedRemnant {
		found = append(found, "it carries a Peasant-generated upload section")
		remove = append(remove, fmt.Sprintf("delete the lines from '%s' to '%s'", ScriptMarkerBegin, ScriptMarkerEnd))
	}
	if plan.HandAdded {
		found = append(found, "it carries the by-hand upload section")
		remove = append(remove, fmt.Sprintf("delete the lines from '%s' to the matching end line", ManualMarkerPrefix))
	}
	// Every instruction below has to name the file git really executes. Through
	// a symlink, "delete the file" would remove the pointer and leave the
	// upload section sitting in the target, still running for anything else
	// that points at it - so the link and the target are named separately, with
	// what each removal actually achieves.
	target := plan.FileToEdit()
	via, deleteNote := "", "or delete the file if it holds nothing else you want"
	if plan.LinkTarget != "" {
		via = fmt.Sprintf(", which git reaches through the symlink %s", plan.Path)
		deleteNote = fmt.Sprintf(
			"or, to stop this repository running it without touching a file you share, delete only the link with: rm %s", shellQuote(plan.Path))
	}
	foundState := strings.Join(found, ", and ") + ", so a village upload still runs from it"
	impact := fmt.Sprintf("nothing was removed. The file is exactly as it was, and it still uploads on every %s", plan.Event)
	if !plan.UploadsFromForeignFile() {
		foundState = strings.Join(found, ", and ") + ", but git does not currently run that file because it is not executable"
		impact = fmt.Sprintf(
			"nothing was removed. No upload runs on %s while the file is not executable, but the section remains and will run again if its executable mode is restored",
			plan.Event)
	}
	return fmt.Sprintf(
		"What went wrong: the %s hook slot for this repository resolves to %s%s, which is not a file Peasant may remove, yet %s.\n"+
			"Why: Peasant deletes only an intact file it generated, and never edits, follows a link to change, or deletes a hook someone else composed or appended to.\n"+
			"Where: repository %s.\n"+
			"When: while uninstalling, before anything was removed.\n"+
			"Impact: %s.\n"+
			"Fix: inspect it with %s; then open %s and %s yourself (%s); then, if you want a hook Peasant can manage again, run %s.",
		plan.Event, target, via, foundState, repo.Root, impact,
		StatusCommandWithBinding(repo.Root, binding), target, strings.Join(remove, ", and "), deleteNote, InstallCommandWithBinding(plan.Event, repo.Root, binding),
	)
}

// viaLinkPhrase discloses the symlink standing between the slot and the file
// git executes, or nothing at all when there is none.
func viaLinkPhrase(slot Slot) string {
	if slot.LinkTarget == "" {
		return ""
	}
	return fmt.Sprintf(" (reached through the symlink %s)", slot.Path)
}

// unreadableUninstallRefusal answers an uninstall that found a slot whose
// contents could not be read - a dangling link, a link to a directory, a device
// node, a file the process may not open.
//
// It refuses rather than reporting nothing to remove, because "no upload runs
// from it" is a claim about contents nobody read. The remedy is the one thing
// that is certain: this repository stops running whatever is there the moment
// the slot itself is gone, and removing the slot is the user's to do because
// Peasant never deletes a file it did not write.
func unreadableUninstallRefusal(plan Plan, repo Repository, binding Binding) string {
	what := fmt.Sprintf("the %s hook at %s could not be read", plan.Event, plan.Path)
	if plan.LinkTarget != "" {
		what = fmt.Sprintf(
			"the %s hook at %s is a symlink to %s, and those bytes could not be read - the link is broken, or points at something that is not a readable file",
			plan.Event, plan.Path, plan.LinkTarget)
	}
	return fmt.Sprintf(
		"What went wrong: %s.\n"+
			"Why: Peasant classifies a hook by its contents and removes only an intact file it generated, so a slot it cannot read is a slot it may neither claim nor delete.\n"+
			"Where: repository %s.\n"+
			"When: while uninstalling, before anything was removed.\n"+
			"Impact: nothing was removed, and Peasant cannot tell you whether an upload runs from this slot - it never read it.\n"+
			"Fix: look at %s yourself. To stop this repository running whatever is there, delete that path: rm %s. "+
			"Then, if you want a hook Peasant can manage, run %s.",
		what, repo.Root, plan.Path, shellQuote(plan.Path), InstallCommandWithBinding(plan.Event, repo.Root, binding),
	)
}

// createHook writes a hook where none exists. The create is exclusive, so a
// file that appeared since the plan was made is never clobbered.
func createHook(path, script, root string, binding Binding) error {
	if err := ensureHooksDir(filepath.Dir(path)); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, ScriptMode)
	if err != nil {
		// The remedy names the cause the error itself carries, because the two
		// causes have different fixes and only one of them is the user's to
		// make. Pointing at 'hooks status' instead sends the user to a report
		// that says "not installed - install one with <the command that just
		// failed>", which is a closed loop: the sibling remove path already
		// names the permission directly.
		return fmt.Errorf(
			"githooks: cannot create the hook at %s\n"+
				"What went wrong: %v\n"+
				"Why: the exclusive create failed, which means the file appeared after Peasant checked, or %s is not writable.\n"+
				"Where: githooks.createHook.\n"+
				"When: while writing the hook, after planning and before any content was written.\n"+
				"Impact: nothing was written and no existing file was replaced.\n"+
				"Fix: %s",
			path, err, filepath.Dir(path), createHookRecovery(path, root, binding, err),
		)
	}
	if _, err := file.WriteString(script); err != nil {
		file.Close()
		os.Remove(path)
		return fmt.Errorf(
			"githooks: cannot write the hook at %s\n"+
				"What went wrong: %v\n"+
				"Why: the write to the newly created file failed, most often a full or read-only filesystem.\n"+
				"Where: githooks.createHook.\n"+
				"When: while writing the hook body.\n"+
				"Impact: the partially written file Peasant had just created was removed; no pre-existing file was touched.\n"+
				"Fix: free space or fix the filesystem permissions, then retry.",
			path, err,
		)
	}
	// O_EXCL applies the umask to the requested mode, so set it explicitly:
	// git only runs a hook that is executable.
	if err := file.Chmod(ScriptMode); err != nil {
		file.Close()
		os.Remove(path)
		return fmt.Errorf(
			"githooks: cannot make the hook at %s executable\n"+
				"What went wrong: %v\n"+
				"Why: git only runs an executable hook, so a non-executable file would silently do nothing.\n"+
				"Where: githooks.createHook.\n"+
				"When: after writing the hook body.\n"+
				"Impact: the file Peasant had just created was removed; no pre-existing file was touched.\n"+
				"Fix: check the filesystem supports the executable bit, then retry.",
			path, err,
		)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf(
			"githooks: cannot finish writing the hook at %s\n"+
				"What went wrong: %v\n"+
				"Why: closing the file failed, so its contents are not trustworthy.\n"+
				"Where: githooks.createHook.\n"+
				"When: while closing the newly written hook.\n"+
				"Impact: the file Peasant had just created was removed; no pre-existing file was touched.\n"+
				"Fix: check the filesystem for errors, then retry.",
			path, err,
		)
	}
	return nil
}

// createHookRecovery names the step that actually clears a failed create.
//
// A permission failure needs the directory made writable; anything else means
// something appeared in the slot between the plan and the write, which is the
// one case 'hooks status' genuinely answers.
func createHookRecovery(path, root string, binding Binding, err error) string {
	dir := filepath.Dir(path)
	if errors.Is(err, os.ErrPermission) {
		return fmt.Sprintf(
			"make %s writable (chmod u+w %s), then re-run the install. If you cannot change it - a hooks directory owned by root, or a read-only filesystem - Peasant cannot manage a hook here at all; run the upload yourself when you want it instead.",
			dir, shellQuote(dir))
	}
	if errors.Is(err, os.ErrExist) {
		return fmt.Sprintf(
			"something was created at %s after Peasant checked the slot. Run %s to see what is there now, then re-run the install, or add the hook by hand.",
			path, StatusCommandWithBinding(root, binding))
	}
	return fmt.Sprintf(
		"check that %s exists and is writable, then re-run the install, or add the hook by hand.", dir)
}

// replaceHook rewrites a hook Peasant owns. Ownership is re-read immediately
// before the write, and the new content lands with a same-directory rename so
// git never sees a half-written hook.
func replaceHook(path, script string) error {
	if err := verifyOwned(path, "replace"); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".peasant-hook-*")
	if err != nil {
		return fmt.Errorf(
			"githooks: cannot stage a replacement for the hook at %s\n"+
				"What went wrong: %v\n"+
				"Why: a temporary file in %s is needed so the hook is swapped in atomically.\n"+
				"Where: githooks.replaceHook.\n"+
				"When: after confirming Peasant owns the existing hook, before it was changed.\n"+
				"Impact: the existing hook is unchanged; whether Git runs it is unchanged.\n"+
				"Fix: make %s writable, then retry.",
			path, err, dir, dir,
		)
	}
	tempPath := temp.Name()
	writeErr := func() error {
		if _, err := temp.WriteString(script); err != nil {
			return err
		}
		if err := temp.Chmod(ScriptMode); err != nil {
			return err
		}
		return temp.Close()
	}()
	if writeErr != nil {
		temp.Close()
		os.Remove(tempPath)
		return fmt.Errorf(
			"githooks: cannot stage a replacement for the hook at %s\n"+
				"What went wrong: %v\n"+
				"Why: writing the replacement into %s failed.\n"+
				"Where: githooks.replaceHook.\n"+
				"When: while staging the new hook body.\n"+
				"Impact: the staged file was removed and the existing hook is unchanged; whether Git runs it is unchanged.\n"+
				"Fix: free space or fix permissions on %s, then retry.",
			path, writeErr, tempPath, dir,
		)
	}
	if err := os.Rename(tempPath, path); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf(
			"githooks: cannot swap in the replacement hook at %s\n"+
				"What went wrong: %v\n"+
				"Why: the same-directory rename that installs the new hook failed.\n"+
				"Where: githooks.replaceHook.\n"+
				"When: after the replacement was fully written.\n"+
				"Impact: the staged file was removed and the existing hook is unchanged; whether Git runs it is unchanged.\n"+
				"Fix: check permissions on %s, then retry.",
			path, err, dir,
		)
	}
	return nil
}

// removeHook deletes a hook Peasant owns, re-reading it immediately before the
// delete so a file that changed since the plan is never destroyed.
func removeHook(path string) error {
	if err := verifyOwned(path, "remove"); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf(
			"githooks: cannot remove the hook at %s\n"+
				"What went wrong: %v\n"+
				"Why: the delete failed, usually a permission problem on the hooks directory.\n"+
				"Where: githooks.removeHook.\n"+
				"When: after confirming Peasant wrote the file, while deleting it.\n"+
				"Impact: the hook is still in place; whether Git runs it is unchanged.\n"+
				"Fix: make the hooks directory writable, or delete the file yourself, then retry.",
			path, err,
		)
	}
	return nil
}

// verifyOwned re-reads path and confirms Peasant still owns it. This runs
// immediately before every write and every delete, so a file that changed after
// the plan was made stops the operation instead of being overwritten.
func verifyOwned(path, operation string) error {
	content, readable := readRegularSlot(path)
	if !readable {
		return fmt.Errorf(
			"githooks: the hook at %s is no longer a plain file Peasant can re-read before the %s\n"+
				"What went wrong: the slot is now a symlink, a pipe, a device, or something else that could not be read as an ordinary file.\n"+
				"Why: Peasant re-reads a hook immediately before changing it, and it only ever rewrites or deletes a regular file it generated. It does not follow a symlink to do either: the link is a file you made, so removing or replacing it would destroy something Peasant does not own.\n"+
				"Where: githooks.verifyOwned.\n"+
				"When: between planning and the %s, so the slot changed shape in that window.\n"+
				"Impact: nothing was changed; the slot and anything it points at are exactly as you left them.\n"+
				"Fix: look at %s yourself. Run 'peasant village hooks status' to see what Peasant makes of it now, then retry.",
			path, operation, operation, path,
		)
	}
	if !IsManaged(content) {
		return fmt.Errorf(
			"githooks: the hook at %s is no longer the file Peasant planned to %s\n"+
				"What went wrong: its contents changed after Peasant read them and it is no longer framed as Peasant-generated.\n"+
				"Why: Peasant only rewrites or deletes an intact file it generated.\n"+
				"Where: githooks.verifyOwned.\n"+
				"When: immediately before the %s.\n"+
				"Impact: nothing was changed; the file on disk is exactly as you left it.\n"+
				"Fix: inspect the file, then either remove it yourself or re-run the command.",
			path, operation, operation,
		)
	}
	return nil
}

// ensureHooksDir makes sure the directory holding the hook exists. Git creates
// .git/hooks itself, so this only matters when it was deleted or when git is
// configured to run hooks from a directory that has not been created yet.
func ensureHooksDir(dir string) error {
	if err := os.MkdirAll(dir, defaults.PublicDirPerm); err != nil {
		return fmt.Errorf(
			"githooks: cannot create the hooks directory %s\n"+
				"What went wrong: %v\n"+
				"Why: git runs hooks from this directory and it does not exist yet.\n"+
				"Where: githooks.ensureHooksDir.\n"+
				"When: before writing the hook file.\n"+
				"Impact: nothing was written.\n"+
				"Fix: create %s yourself or fix the permissions above it, then retry.",
			dir, err, dir,
		)
	}
	return nil
}

// normalizeEvents validates the requested events and returns them deduplicated
// in report order. An empty request means every managed event unless the caller
// demands an explicit choice, which install and plan do so no hook is ever
// installed from an implied selection.
func normalizeEvents(events []Event, requireExplicit bool) ([]Event, error) {
	if len(events) == 0 {
		if requireExplicit {
			return nil, fmt.Errorf(
				"githooks: no hook event was requested\n"+
					"What went wrong: the request named zero events.\n"+
					"Why: a hook is only ever installed from an explicit choice, never an implied one.\n"+
					"Where: githooks.normalizeEvents.\n"+
					"When: while validating the request, before any repository was resolved.\n"+
					"Impact: nothing was planned or installed.\n"+
					"Fix: name at least one event with --event, one of %s.",
				EventList(AllEvents[:]),
			)
		}
		return append([]Event(nil), AllEvents[:]...), nil
	}

	requested := make(map[Event]bool, len(events))
	for _, event := range events {
		if err := event.Validate(); err != nil {
			return nil, err
		}
		requested[event] = true
	}
	ordered := make([]Event, 0, len(requested))
	for _, event := range AllEvents {
		if requested[event] {
			ordered = append(ordered, event)
		}
	}
	return ordered, nil
}
