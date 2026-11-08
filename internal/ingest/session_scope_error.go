package ingest

import (
	"fmt"
	"strings"
)

// MissReason says why one selected session id could not be used.
type MissReason uint8

const (
	missReasonUnset MissReason = iota // invalid
	// MissNotFound means there is no stored row and no lookup hit. A
	// SearchFailed entry in the miss's Searched says a lookup could not run.
	MissNotFound
	// MissHarnessFiltered means the session is stored under a harness that
	// --harness excludes.
	MissHarnessFiltered
	// MissSinceFiltered means the session was found, but --since excludes it.
	MissSinceFiltered
)

// IsValid reports whether the reason is one of the declared values.
func (r MissReason) IsValid() bool {
	switch r {
	case MissNotFound, MissHarnessFiltered, MissSinceFiltered:
		return true
	default:
		return false
	}
}

// StoreAttachment says whether the run that produced a scope miss had a store
// attached. The fix line differs when none was.
type StoreAttachment uint8

const (
	storeAttachmentUnset StoreAttachment = iota // invalid
	// StoreAttached means the run reads and writes the analytics database.
	StoreAttached
	// StoreNotAttached means a logs-only run, or a store-free dry run.
	StoreNotAttached
)

// IsValid reports whether the attachment is one of the declared values.
func (s StoreAttachment) IsValid() bool {
	switch s {
	case StoreAttached, StoreNotAttached:
		return true
	default:
		return false
	}
}

// ScopeMiss is one selected session id that could not be used.
type ScopeMiss struct {
	ID     SessionID
	Reason MissReason
	// StoredHarness is set for MissHarnessFiltered: the harness the session is
	// recorded under.
	StoredHarness Harness
	// Searched is set for MissNotFound. It includes SearchHarnessDisabled and
	// SearchFailed entries, so the message can name an unsearched harness and a
	// failed lookup.
	Searched []SearchedSource
}

// SessionScopeError is returned by Pipeline.Run (and Reindex) when one or more
// SELECTED ids cannot be used: the row is absent and no source holds it, the
// user's own --harness or --since excludes it, or the selection is unusable for
// another stated reason. It is returned after LOCATE and before the
// harvest-start sweep, before the deferred writes, and before the AUDIT row.
//
// A stored session whose source is merely unreadable is NOT a ScopeMiss: it is
// a LocateStoredUnreadable member, and the run continues.
//
// At least one miss is required: a value with none describes no refusal, so it
// renders that fact instead of posing as one.
type SessionScopeError struct {
	Misses []ScopeMiss
	// Command is the words the user typed: "harvest", "harvest index", or
	// "harvest logs". `open` sets "harvest", because it re-enters harvest.
	Command string
	// Prefix is the command that reaches this store, for example
	// "peasant --data-dir '/srv/peasant'". It is used only inside suggested
	// commands; the empty value renders a bare "peasant".
	Prefix string
	// Store says whether the run had a store attached. The fix line differs
	// when none was. The zero value states no attachment, and every renderer
	// says so rather than guessing.
	Store StoreAttachment
}

// Error returns the full labelled block for the screen. Its first line is the
// bare "peasant:" label with the command and headline; its last line is the
// fix. The searched detail appears once, and the fix appears once.
func (e *SessionScopeError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "peasant: %s failed: %s\n", e.commandName(), e.headline())
	b.WriteString(labelBlock("what:", e.whatLine(), false))
	for _, line := range e.whyLines() {
		b.WriteString(labelBlock("why:", line, false))
	}
	b.WriteString(labelBlock("where:", "session lookup (ingest scoped locate)", false))
	b.WriteString(labelBlock("when:", "before any session was swept, recorded, or indexed", false))
	if e.anyNotFound() {
		b.WriteString("  searched:\n")
		for _, line := range e.searchedLines() {
			fmt.Fprintf(&b, "    %-14s%s\n", line.name, line.text)
		}
	}
	b.WriteString(labelBlock("impact:", blockImpactText, true))
	b.WriteString(labelBlock("fix:", e.FixLine(), true))
	return strings.TrimRight(b.String(), "\n")
}

// Reason returns ONE line for the one-line renderers (`open`): the headline
// followed, when any miss is not-found, by the searched clause. It carries no
// label, no "fix:", and no trailing period.
func (e *SessionScopeError) Reason() string {
	reason := e.headline()
	if e.anyNotFound() {
		reason += "; " + e.searchedClause()
	}
	return reason
}

// FixLine returns the fix clause only, with no "fix:" label. Every command it
// suggests carries the run's Prefix. It is self-contained: `open` prints it on
// one line with nothing above it.
func (e *SessionScopeError) FixLine() string {
	if fix := e.lookupFailedFix(); fix != "" {
		return fix
	}
	if len(e.Misses) == 0 {
		return "build the error with the misses the session lookup recorded, then report it"
	}
	notFound := e.countReason(MissNotFound)
	fixes := make([]string, 0, 3)
	if notFound > 0 {
		idWord := "id"
		if notFound > 1 {
			idWord = "ids"
		}
		switch e.Store {
		case StoreNotAttached:
			fixes = append(fixes, fmt.Sprintf("no store is attached to this run, so a pi or legacy-JSON OpenCode session can be found only by running `%s` once without --session",
				e.command("harvest", "logs")))
		case StoreAttached:
			fixes = append(fixes, fmt.Sprintf("check the %s with `%s`, or run `%s` once without --session",
				idWord, e.command("sessions", "list"), e.command(e.commandName())))
		default:
			fixes = append(fixes, fmt.Sprintf("check the %s with `%s` if this run has a store, or run `%s` once without --session",
				idWord, e.command("sessions", "list"), e.command(e.commandName())))
		}
	}
	if e.countReason(MissHarnessFiltered) > 0 {
		if harnesses := e.filteredHarnesses(); len(harnesses) == 1 {
			fixes = append(fixes, "drop --harness, or pass --harness "+string(harnesses[0]))
		} else {
			fixes = append(fixes, "drop --harness, or pass the harness each session was recorded under (--harness takes one, so run once per harness)")
		}
	}
	if e.countReason(MissSinceFiltered) > 0 {
		fixes = append(fixes, "drop --since, or use an earlier --since")
	}
	if len(fixes) == 0 {
		return "check the id and the run's filters, then retry"
	}
	if len(fixes) == 1 {
		if notFound > 0 {
			return fixes[0] + ", then retry"
		}
		return fixes[0]
	}
	return strings.Join(fixes, "; ") + "; then retry"
}

// headline is the one-sentence summary of what the run refused to do.
func (e *SessionScopeError) headline() string {
	switch len(e.Misses) {
	case 0:
		return "a scope error was raised without a miss, so it cannot say which session was refused"
	case 1:
		return "session " + e.missClause(e.Misses[0]) + ", so nothing was recorded"
	}
	clauses := make([]string, 0, len(e.Misses))
	for _, miss := range e.Misses {
		clauses = append(clauses, e.missClause(miss))
	}
	return fmt.Sprintf("%d sessions could not be used, so nothing was recorded (%s)",
		len(e.Misses), strings.Join(clauses, "; "))
}

// missClause is one miss stated in a sentence fragment, without the leading
// "session" word the single-miss headline adds.
func (e *SessionScopeError) missClause(miss ScopeMiss) string {
	id := e.shortID(miss.ID)
	switch miss.Reason {
	case MissNotFound:
		if failedLookup(miss.Searched) != nil {
			return id + " could not be looked up in all sources"
		}
		return id + " was not found"
	case MissHarnessFiltered:
		return fmt.Sprintf("%s is recorded under %s, which --harness excludes", id, miss.StoredHarness)
	case MissSinceFiltered:
		return id + " was found but --since excludes it"
	default:
		return id + " could not be used for an unstated reason"
	}
}

// whatLine names the kind of run and what it looked for. It never rebuilds a
// command, so it cannot drop the user's other flags.
func (e *SessionScopeError) whatLine() string {
	if len(e.Misses) == 0 {
		return "this scope error carries no miss, so it cannot say what the run looked for"
	}
	if len(e.Misses) != 1 {
		return fmt.Sprintf("this session-scoped %s looked for %d sessions", e.commandName(), len(e.Misses))
	}
	miss := e.Misses[0]
	base := fmt.Sprintf("this session-scoped %s looked for session %s", e.commandName(), e.shortID(miss.ID))
	switch miss.Reason {
	case MissNotFound:
		if failed := failedLookup(miss.Searched); failed != nil {
			return fmt.Sprintf("%s; no source that was searched holds it, and the lookup in %s failed", base, failed.Harness)
		}
		return base + " and no enabled source holds it"
	case MissHarnessFiltered:
		return fmt.Sprintf("%s and found it recorded under %s, which this run's --harness excludes", base, miss.StoredHarness)
	case MissSinceFiltered:
		return base + " and found it, but this run's --since excludes it"
	default:
		return base
	}
}

// whyLines explains each miss. With several misses each line names its id.
func (e *SessionScopeError) whyLines() []string {
	lines := make([]string, 0, len(e.Misses))
	for _, miss := range e.Misses {
		text := e.whyText(miss)
		if len(e.Misses) > 1 {
			text = "session " + e.shortID(miss.ID) + ": " + text
		}
		lines = append(lines, text)
	}
	return lines
}

func (e *SessionScopeError) whyText(miss ScopeMiss) string {
	switch miss.Reason {
	case MissNotFound:
		var storeClause string
		switch e.Store {
		case StoreAttached:
			storeClause = "the store has no row for this id"
		case StoreNotAttached:
			storeClause = "no store is attached to this run"
		default:
			storeClause = "this error does not state whether a store was attached"
		}
		if failed := failedLookup(miss.Searched); failed != nil {
			return fmt.Sprintf("%s, and the %s lookup could not run, so the session may be there", storeClause, failed.Harness)
		}
		return storeClause + ", and no harness that can search by id has a file or row with it"
	case MissHarnessFiltered:
		return fmt.Sprintf("this session is recorded under %s, which --harness excludes", miss.StoredHarness)
	case MissSinceFiltered:
		return "the session was found, but --since excludes it"
	default:
		return "the session could not be used for an unstated reason"
	}
}

// searchedLine is one rendered row of the searched block.
type searchedLine struct {
	name string
	text string
}

// searchedLines renders the searched block: the store row first, then every
// harness search in the order it was recorded. The store row is synthesized
// from Store, because no harness searched it.
func (e *SessionScopeError) searchedLines() []searchedLine {
	lines := []searchedLine{{name: "store", text: e.storeLineText()}}
	for _, searched := range e.uniqueSearched() {
		name := string(searched.Harness)
		switch {
		case searched.Method.searchable():
			lines = append(lines, searchedLine{name: name, text: searched.Location})
		case searched.Method == SearchUnsupported:
			lines = append(lines, searchedLine{name: name, text: unsupportedSearchText(searched.Reason)})
		case searched.Method == SearchHarnessDisabled:
			lines = append(lines, searchedLine{name: name, text: fmt.Sprintf(
				"not enabled (config.yaml excludes it); enable it with `%s`", e.command("kickstart"))})
		case searched.Method == SearchFailed:
			lines = append(lines, searchedLine{name: name, text: "lookup failed: " + firstLineOr(searched.Detail, "no error detail")})
		default:
			// A method this build does not know still names its harness: dropping
			// the row would silently hide a source from the miss message.
			lines = append(lines, searchedLine{name: name, text: "search state unstated (unknown method)"})
		}
	}
	return lines
}

// searchedClause is the one-clause summary of the same values the searched
// block renders, grouped by what the search could do.
func (e *SessionScopeError) searchedClause() string {
	var searchedNames []string
	if name := storeClauseName(e.Store); name != "" {
		searchedNames = append(searchedNames, name)
	}
	var unsupported, disabled, failed, unstated []string
	for _, searched := range e.uniqueSearched() {
		name := string(searched.Harness)
		switch searched.Method {
		case SearchStoredPath, SearchNamePattern:
			searchedNames = appendUniqueString(searchedNames, name)
		case SearchSQLiteRow:
			searchedNames = appendUniqueString(searchedNames, name+" (SQLite)")
		case SearchUnsupported:
			label := name
			if searched.Reason == IDOnlyInFileBody {
				label += " (legacy JSON)"
			}
			unsupported = appendUniqueString(unsupported, label)
		case SearchHarnessDisabled:
			disabled = appendUniqueString(disabled, name)
		case SearchFailed:
			failed = appendUniqueString(failed, fmt.Sprintf("%s (%s)", name, firstLineOr(searched.Detail, "no error detail")))
		default:
			unstated = appendUniqueString(unstated, name)
		}
	}

	parts := make([]string, 0, 6)
	if len(searchedNames) > 0 {
		parts = append(parts, "searched "+strings.Join(searchedNames, ", "))
	}
	if !e.Store.IsValid() {
		// A value this build does not know says nothing about the store. It is
		// never listed as a harness that searched, because that would claim a
		// search that may not have run.
		parts = append(parts, "store state unstated")
	}
	if len(unsupported) > 0 {
		parts = append(parts, "cannot search by id: "+strings.Join(unsupported, ", "))
	}
	if len(disabled) > 0 {
		parts = append(parts, "not enabled: "+strings.Join(disabled, ", "))
	}
	if len(failed) > 0 {
		parts = append(parts, "lookup failed: "+strings.Join(failed, ", "))
	}
	if len(unstated) > 0 {
		parts = append(parts, "search state unstated: "+strings.Join(unstated, ", "))
	}
	if len(parts) == 0 {
		return "no source state was recorded"
	}
	return strings.Join(parts, "; ")
}

// lookupFailedFix is the self-contained fix a failed lookup needs: `open`
// prints it with nothing above it, so it names the source and its location and
// never refers to an earlier line.
func (e *SessionScopeError) lookupFailedFix() string {
	for _, miss := range e.Misses {
		for _, searched := range miss.Searched {
			if searched.Method != SearchFailed {
				continue
			}
			location := searched.Location
			if location == "" {
				location = "its configured location"
			}
			return fmt.Sprintf(
				"repair the %s database at %s (its lookup failed: %s), then run `%s` again, or check the id with `%s`",
				searched.Harness, location, firstLineOr(searched.Detail, "no error detail"),
				e.command(e.commandName(), "--session", string(miss.ID)),
				e.command("sessions", "list"))
		}
	}
	return ""
}

// uniqueSearched returns the recorded searches across every miss, in first-seen
// order and without repeats: several misses carry the one harness report, and a
// scoped run records the whole list on every not-found miss. An entry with no
// harness name is skipped because there is nothing to print; an entry whose
// method this build does not know is kept, and the renderers name its harness
// under a neutral arm.
func (e *SessionScopeError) uniqueSearched() []SearchedSource {
	var unique []SearchedSource
	seen := make(map[SearchedSource]struct{})
	for _, miss := range e.Misses {
		for _, searched := range miss.Searched {
			if searched.Harness == "" {
				continue
			}
			if _, duplicate := seen[searched]; duplicate {
				continue
			}
			seen[searched] = struct{}{}
			unique = append(unique, searched)
		}
	}
	return unique
}

func (e *SessionScopeError) storeLineText() string {
	switch e.Store {
	case StoreAttached:
		return "no recorded session with this id"
	case StoreNotAttached:
		return "not attached (logs-only run)"
	default:
		return "state unstated (this error does not carry whether a store was attached)"
	}
}

// storeClauseName is the store's name inside the searched clause. The zero
// value has no name there: it did not search, and it must not claim to.
func storeClauseName(store StoreAttachment) string {
	switch store {
	case StoreAttached:
		return "store"
	case StoreNotAttached:
		return "no store (logs-only)"
	default:
		return ""
	}
}

// anyNotFound reports whether any miss is MissNotFound, which is what decides
// whether the message carries a searched clause.
func (e *SessionScopeError) anyNotFound() bool {
	return e.countReason(MissNotFound) > 0
}

func (e *SessionScopeError) countReason(reason MissReason) int {
	count := 0
	for _, miss := range e.Misses {
		if miss.Reason == reason {
			count++
		}
	}
	return count
}

// filteredHarnesses returns the distinct stored harnesses the run's --harness
// excluded.
func (e *SessionScopeError) filteredHarnesses() []Harness {
	var harnesses []Harness
	for _, miss := range e.Misses {
		if miss.Reason != MissHarnessFiltered {
			continue
		}
		found := false
		for _, harness := range harnesses {
			if harness == miss.StoredHarness {
				found = true
				break
			}
		}
		if !found {
			harnesses = append(harnesses, miss.StoredHarness)
		}
	}
	return harnesses
}

// failedLookup returns the first search that could not run, or nil.
func failedLookup(searched []SearchedSource) *SearchedSource {
	for index := range searched {
		if searched[index].Method == SearchFailed {
			return &searched[index]
		}
	}
	return nil
}

func unsupportedSearchText(reason UnsupportedReason) string {
	switch reason {
	case IDOnlyInFileBody:
		return "legacy JSON storage: cannot be searched by id (the id is only in the file body)"
	case IDOnlyInFileHeader:
		return "cannot be searched by id (the id is only in the file header)"
	default:
		return "cannot be searched by id"
	}
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// firstLineOr returns the first line of detail, or fallback when it is empty.
// A recorded error may be multi-line, and a message field stays on one line
// without a trailing period, because Reason() must end without one.
func firstLineOr(detail, fallback string) string {
	line := strings.SplitN(strings.TrimSpace(detail), "\n", 2)[0]
	line = strings.TrimRight(strings.TrimSpace(line), ".")
	if line == "" {
		return fallback
	}
	return line
}

// shortID abbreviates an id for prose. It keeps the pinned four characters
// unless another miss in this message shares them, in which case it lengthens
// the prefix until the ids are distinguishable: two identical clauses would
// tell the reader nothing. A suggested command always carries the full id,
// because an abbreviated one would not run.
func (e *SessionScopeError) shortID(id SessionID) string {
	raw := string(id)
	length := 4
	if len(raw) < length {
		return raw
	}
	for length < len(raw) {
		prefix := raw[:length]
		collides := false
		for _, other := range e.Misses {
			if other.ID == id {
				continue
			}
			otherRaw := string(other.ID)
			if len(otherRaw) >= length && otherRaw[:length] == prefix {
				collides = true
				break
			}
		}
		if !collides {
			break
		}
		length++
	}
	if length == len(raw) {
		return raw
	}
	return raw[:length] + "..."
}

func (e *SessionScopeError) commandName() string {
	if e.Command == "" {
		return "harvest"
	}
	return e.Command
}

// command renders one suggested command with the run's prefix. An empty prefix
// renders a bare "peasant", which is the zero value a pipeline-only caller and
// a test use.
func (e *SessionScopeError) command(args ...string) string {
	prefix := strings.TrimSpace(e.Prefix)
	if prefix == "" {
		prefix = "peasant"
	}
	return prefix + " " + strings.Join(args, " ")
}

// Label column and wrap rules for the block. The block is the method's pinned
// rendering, so these values are part of it: labels occupy the first 12
// columns, and the two fields that grow into paragraphs (impact and fix) wrap
// at 89 text columns with a hanging indent that lines up under the label. A
// backtick span is never split, because a command a user copies must stay whole.
const (
	blockLabelColumn   = 12
	blockTextWrapWidth = 89
)

const blockImpactText = "no session was recorded or changed. Start-up maintenance (recovery of " +
	"interrupted writes, store migration, search-index repair) may still have run. No full scan was run."

// labelBlock renders one labelled line. Only the two paragraph fields wrap; the
// other fields stay whole, because a cause or a searched path reads better on
// one line than split mid-phrase, and a reader copies a path as one unit.
func labelBlock(label, text string, wrap bool) string {
	lines := []string{text}
	if wrap {
		lines = wrapBlockText(text, blockTextWrapWidth)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  %-*s%s\n", blockLabelColumn-2, label, lines[0])
	for _, line := range lines[1:] {
		b.WriteString(strings.Repeat(" ", blockLabelColumn))
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// wrapBlockText greedily wraps text at width columns. A backtick span is one
// token even when it contains spaces, so a command is never split.
func wrapBlockText(text string, width int) []string {
	tokens := blockTokens(text)
	if len(tokens) == 0 {
		return []string{""}
	}
	lines := make([]string, 0, 2)
	current := tokens[0]
	for _, token := range tokens[1:] {
		if len(current)+1+len(token) <= width {
			current += " " + token
			continue
		}
		lines = append(lines, current)
		current = token
	}
	return append(lines, current)
}

// blockTokens splits text on spaces, keeping a backtick span whole.
func blockTokens(text string) []string {
	var tokens []string
	var current strings.Builder
	for index := 0; index < len(text); {
		switch text[index] {
		case '`':
			end := strings.IndexByte(text[index+1:], '`')
			if end < 0 {
				current.WriteByte(text[index])
				index++
				continue
			}
			current.WriteString(text[index : index+end+2])
			index += end + 2
		case ' ':
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
			index++
		default:
			current.WriteByte(text[index])
			index++
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}
