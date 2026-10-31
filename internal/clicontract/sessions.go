package clicontract

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// SessionsFromFlag is the flag that names a file of session ids, or "-" for
// standard input.
const SessionsFromFlag = "sessions-from"

// sessionsFromStdin is the --sessions-from value that reads standard input.
const sessionsFromStdin = "-"

// conflictingSelectors are the other ways a command can be told which sessions
// to act on. --sessions-from replaces them; it never combines with one, so a
// caller cannot widen or narrow a list by accident.
var conflictingSelectors = []string{"session", "all"}

// ErrNoSessionIDs is returned when an input names no session at all. A
// selector that selects nothing must never widen to "all sessions".
var ErrNoSessionIDs = errors.New("the input names no session ids")

// RegisterSessionsFrom adds --sessions-from <file|-> to cmd.
func RegisterSessionsFrom(cmd *cobra.Command) {
	cmd.Flags().String(SessionsFromFlag, "",
		"Read session ids from a file, or from standard input when the value is -. "+
			"Accepts one id per line, a JSON array of ids, the JSON array that a --json listing prints, "+
			"or a sessions review --json document, which gives only its clean sessions")
}

// AliasSessionsFrom makes alias a hidden spelling of --sessions-from on cmd.
// The alias resolves to the same flag, so it is not listed in help and cannot
// be given a different value.
func AliasSessionsFrom(cmd *cobra.Command, alias string) {
	flags := cmd.Flags()
	previous := flags.GetNormalizeFunc()
	flags.SetNormalizeFunc(func(f *pflag.FlagSet, name string) pflag.NormalizedName {
		if name == alias {
			name = SessionsFromFlag
		}
		return previous(f, name)
	})
}

// ResolveSessionsFrom returns the ids named by --sessions-from. set is false
// when the flag was not given. A value of "-" reads cmd.InOrStdin(). It
// refuses an empty list, and it refuses --sessions-from together with
// --session or --all. Every refusal is a *UsageError.
//
// The result is a SessionList rather than bare ids so that every receiving
// command has the dropped sessions in hand and reports them.
func ResolveSessionsFrom(cmd *cobra.Command) (list SessionList, set bool, err error) {
	flag := cmd.Flags().Lookup(SessionsFromFlag)
	if flag == nil || !flag.Changed {
		return SessionList{}, false, nil
	}
	for _, other := range conflictingSelectors {
		if o := cmd.Flags().Lookup(other); o != nil && o.Changed {
			return SessionList{}, true, NewUsageError(fmt.Sprintf(
				"%s: conflicting flags --%s and --%s\n"+
					"What went wrong: both --%s and --%s were provided.\n"+
					"Why: exactly one source of session ids is allowed, so a list can never be widened or narrowed by a second flag.\n"+
					"Fix: pass the ids through --%s alone, or drop --%s and use --%s.",
				cmd.CommandPath(), SessionsFromFlag, other,
				SessionsFromFlag, other,
				SessionsFromFlag, SessionsFromFlag, other))
		}
	}

	return ReadSessionsFrom(cmd)
}

// ReadSessionsFrom is ResolveSessionsFrom without the conflict check: it reads
// the ids --sessions-from names and refuses an unreadable or empty list. It is
// for a command whose own rules let --sessions-from combine with another
// selector, such as village push, where --session adds to the list.
func ReadSessionsFrom(cmd *cobra.Command) (list SessionList, set bool, err error) {
	flag := cmd.Flags().Lookup(SessionsFromFlag)
	if flag == nil || !flag.Changed {
		return SessionList{}, false, nil
	}

	source := flag.Value.String()
	var r io.Reader
	name := source
	if source == sessionsFromStdin {
		r = cmd.InOrStdin()
		name = "standard input"
	} else {
		f, openErr := os.Open(source)
		if openErr != nil {
			return SessionList{}, true, NewUsageError(fmt.Sprintf(
				"%s: open --%s %q: %v\n"+
					"What went wrong: the file %q could not be opened.\n"+
					"Why: the file may not exist, or the process may lack read permission.\n"+
					"Fix: verify the file path exists and is readable, or pass - to read the ids from standard input.",
				cmd.CommandPath(), SessionsFromFlag, source, openErr, source))
		}
		defer f.Close()
		r = f
	}

	list, err = ReadSessionList(r)
	var noClean *NoCleanSessionsError
	if errors.As(err, &noClean) {
		return SessionList{}, true, NewUsageError(fmt.Sprintf(
			"%s: --%s %s names no clean session\n"+
				"What went wrong: the review document left out every session it holds: %s.\n"+
				"Why: a session with findings or an error must not flow into another command, and a list that selects nothing must not widen to every session.\n"+
				"Fix: resolve the findings and review again, or name a session with --session after reading what the review reported.",
			cmd.CommandPath(), SessionsFromFlag, name, describeDropped(noClean.Dropped)))
	}
	if errors.Is(err, ErrNoSessionIDs) {
		return SessionList{}, true, NewUsageError(fmt.Sprintf(
			"%s: --%s %s names no session ids\n"+
				"What went wrong: the input holds no session id, only blank or comment lines or an empty array.\n"+
				"Why: a list that selects nothing must not widen to every session.\n"+
				"Fix: give at least one session id, for example from `peasant sessions list --json`.",
			cmd.CommandPath(), SessionsFromFlag, name))
	}
	if err != nil {
		return SessionList{}, true, NewUsageError(fmt.Sprintf(
			"%s: read --%s %s: %v\n"+
				"What went wrong: the input is not a list of session ids.\n"+
				"Why: --%s accepts one id per line, a JSON array of ids, a JSON array of objects carrying \"id\", \"session_id\" or \"sessionId\", or a `sessions review --json` document.\n"+
				"Fix: correct the input at the position named above, then retry.",
			cmd.CommandPath(), SessionsFromFlag, name, err, SessionsFromFlag))
	}
	return list, true, nil
}

// ReadSessionIDs is ReadSessionList for a caller that needs only the ids.
func ReadSessionIDs(r io.Reader) ([]string, error) {
	list, err := ReadSessionList(r)
	if err != nil {
		return nil, err
	}
	return list.IDs, nil
}

// ReadSessionList parses a session list from r. It accepts four shapes and
// detects them from the first non-space byte: newline text (blank lines
// skipped, '#' lines skipped), a JSON array of strings, a JSON array of
// objects, or a `sessions review --json` document. An array object contributes
// its "id" key, else its "session_id" key, else its "sessionId" key. Any other
// shape is an error naming the line or index. Duplicates collapse, first
// occurrence wins. An input that names no id returns ErrNoSessionIDs.
//
// From review entries, the document's or those of a `jq .sessions` of it, only
// status clean is taken. The rest are kept in Dropped so that the receiving
// command can say so, and when nothing is left the error is a
// *NoCleanSessionsError, which is also ErrNoSessionIDs.
func ReadSessionList(r io.Reader) (SessionList, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return SessionList{}, fmt.Errorf("read session ids: %w", err)
	}
	trimmed := bytes.TrimLeftFunc(data, unicode.IsSpace)
	var parsed []parsedSession
	switch {
	case len(trimmed) > 0 && trimmed[0] == '[':
		parsed, err = readJSONSessionIDs(data)
	case len(trimmed) > 0 && trimmed[0] == '{':
		parsed, err = readReviewDocument(data)
	default:
		parsed, err = readTextSessionIDs(data)
	}
	if err != nil {
		return SessionList{}, err
	}
	list := splitByStatus(parsed)
	if len(list.IDs) > 0 {
		return list, nil
	}
	if len(list.Dropped) > 0 {
		return SessionList{}, &NoCleanSessionsError{Dropped: list.Dropped}
	}
	return SessionList{}, ErrNoSessionIDs
}

// splitByStatus applies the clean-only rule. An id listed as both clean and
// not clean counts as not clean.
func splitByStatus(parsed []parsedSession) SessionList {
	var list SessionList
	dropped := map[string]bool{}
	for _, session := range parsed {
		if session.Status == "" || session.Status == ReviewClean || dropped[session.ID] {
			continue
		}
		dropped[session.ID] = true
		list.Dropped = append(list.Dropped, DroppedSession{ID: session.ID, Status: session.Status})
	}
	for _, session := range parsed {
		if !dropped[session.ID] {
			list.IDs = append(list.IDs, session.ID)
		}
	}
	list.IDs = dedupe(list.IDs)
	return list
}

func readTextSessionIDs(data []byte) ([]parsedSession, error) {
	var ids []parsedSession
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), len(data)+1)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if strings.ContainsFunc(text, unicode.IsSpace) {
			return nil, fmt.Errorf("line %d: %q is not one session id (it contains whitespace)", line, text)
		}
		if strings.ContainsAny(text[:1], "[]{}\"") {
			return nil, fmt.Errorf("line %d: %q looks like JSON inside a newline list; give either one id per line or one JSON array", line, text)
		}
		ids = append(ids, parsedSession{ID: text})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read session ids: %w", err)
	}
	return ids, nil
}

func readJSONSessionIDs(data []byte) ([]parsedSession, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var elements []json.RawMessage
	if err := decoder.Decode(&elements); err != nil {
		return nil, fmt.Errorf("JSON array: %w", err)
	}
	if err := refuseTrailingContent(data, decoder, "array"); err != nil {
		return nil, err
	}
	ids := make([]parsedSession, 0, len(elements))
	for index, raw := range elements {
		session, err := sessionFromElement(raw)
		if err != nil {
			return nil, fmt.Errorf("index %d: %w", index, err)
		}
		ids = append(ids, session)
	}
	return ids, nil
}

// readReviewDocument reads the one object shape the reader accepts. Every
// element must carry sessionId and a status from the closed set, so a document
// that is not a review, or one that lost its statuses, is refused rather than
// taken as a list of clean sessions.
func readReviewDocument(data []byte) ([]parsedSession, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var document struct {
		Sessions *[]json.RawMessage `json:"sessions"`
	}
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("JSON object: %w", err)
	}
	if err := refuseTrailingContent(data, decoder, "object"); err != nil {
		return nil, err
	}
	if document.Sessions == nil {
		return nil, errors.New(`line 1: the JSON object has no "sessions" array; give a sessions review document, one JSON array, or one id per line`)
	}
	ids := make([]parsedSession, 0, len(*document.Sessions))
	for index, raw := range *document.Sessions {
		var entry struct {
			SessionID *string `json:"sessionId"`
			Status    *string `json:"status"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("index %d: %w", index, err)
		}
		if entry.SessionID == nil {
			return nil, fmt.Errorf(`index %d: object has no "sessionId" key`, index)
		}
		if entry.Status == nil {
			return nil, fmt.Errorf(`index %d: object has no "status" key`, index)
		}
		session, err := newReviewedSession(*entry.SessionID, *entry.Status)
		if err != nil {
			return nil, fmt.Errorf("index %d: %w", index, err)
		}
		ids = append(ids, session)
	}
	return ids, nil
}

func newReviewedSession(rawID, rawStatus string) (parsedSession, error) {
	id, err := nonEmptyID(rawID)
	if err != nil {
		return parsedSession{}, err
	}
	status, err := NewReviewStatus(rawStatus)
	if err != nil {
		return parsedSession{}, err
	}
	return parsedSession{ID: id, Status: status}, nil
}

func refuseTrailingContent(data []byte, decoder *json.Decoder, shape string) error {
	rest := data[decoder.InputOffset():]
	extra := bytes.TrimSpace(rest)
	if len(extra) == 0 {
		return nil
	}
	offset := int(decoder.InputOffset()) + bytes.Index(rest, extra[:1])
	return fmt.Errorf("line %d: unexpected content after the JSON %s; give either one JSON array or one id per line",
		bytes.Count(data[:offset], []byte("\n"))+1, shape)
}

func sessionFromElement(raw json.RawMessage) (parsedSession, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return parsedSession{}, errors.New("empty element")
	}
	switch trimmed[0] {
	case '"':
		var id string
		if err := json.Unmarshal(trimmed, &id); err != nil {
			return parsedSession{}, err
		}
		id, err := nonEmptyID(id)
		return parsedSession{ID: id}, err
	case '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return parsedSession{}, err
		}
		return sessionFromObject(object)
	default:
		return parsedSession{}, fmt.Errorf("%s is neither a session id string nor an object", trimmed)
	}
}

// sessionFromObject reads one array object. The sessionId key is the review
// spelling, so an object carrying it and a status is a review entry whatever
// other key names it: `jq .sessions` must not strip the clean-only rule.
func sessionFromObject(object map[string]json.RawMessage) (parsedSession, error) {
	for _, key := range []string{"id", "session_id", "sessionId"} {
		value, ok := object[key]
		if !ok {
			continue
		}
		var id string
		if err := json.Unmarshal(value, &id); err != nil {
			return parsedSession{}, fmt.Errorf("%q is not a string", key)
		}
		rawStatus, reviewed := object["status"]
		if _, spelled := object["sessionId"]; !spelled || !reviewed {
			id, err := nonEmptyID(id)
			return parsedSession{ID: id}, err
		}
		var status string
		if err := json.Unmarshal(rawStatus, &status); err != nil {
			return parsedSession{}, errors.New(`"status" is not a string`)
		}
		return newReviewedSession(id, status)
	}
	return parsedSession{}, errors.New(`object has neither an "id" nor a "session_id" nor a "sessionId" key`)
}

func nonEmptyID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("empty session id")
	}
	return id, nil
}

func dedupe(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
