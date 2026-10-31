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
			"Accepts one id per line, a JSON array of ids, or the JSON array that a --json listing prints")
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
func ResolveSessionsFrom(cmd *cobra.Command) (ids []string, set bool, err error) {
	flag := cmd.Flags().Lookup(SessionsFromFlag)
	if flag == nil || !flag.Changed {
		return nil, false, nil
	}
	for _, other := range conflictingSelectors {
		if o := cmd.Flags().Lookup(other); o != nil && o.Changed {
			return nil, true, NewUsageError(fmt.Sprintf(
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
func ReadSessionsFrom(cmd *cobra.Command) (ids []string, set bool, err error) {
	flag := cmd.Flags().Lookup(SessionsFromFlag)
	if flag == nil || !flag.Changed {
		return nil, false, nil
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
			return nil, true, NewUsageError(fmt.Sprintf(
				"%s: open --%s %q: %v\n"+
					"What went wrong: the file %q could not be opened.\n"+
					"Why: the file may not exist, or the process may lack read permission.\n"+
					"Fix: verify the file path exists and is readable, or pass - to read the ids from standard input.",
				cmd.CommandPath(), SessionsFromFlag, source, openErr, source))
		}
		defer f.Close()
		r = f
	}

	ids, err = ReadSessionIDs(r)
	if errors.Is(err, ErrNoSessionIDs) {
		return nil, true, NewUsageError(fmt.Sprintf(
			"%s: --%s %s names no session ids\n"+
				"What went wrong: the input holds no session id, only blank or comment lines or an empty array.\n"+
				"Why: a list that selects nothing must not widen to every session.\n"+
				"Fix: give at least one session id, for example from `peasant sessions list --json`.",
			cmd.CommandPath(), SessionsFromFlag, name))
	}
	if err != nil {
		return nil, true, NewUsageError(fmt.Sprintf(
			"%s: read --%s %s: %v\n"+
				"What went wrong: the input is not a list of session ids.\n"+
				"Why: --%s accepts one id per line, a JSON array of ids, or a JSON array of objects carrying \"id\" or \"session_id\".\n"+
				"Fix: correct the input at the position named above, then retry.",
			cmd.CommandPath(), SessionsFromFlag, name, err, SessionsFromFlag))
	}
	return ids, true, nil
}

// ReadSessionIDs parses ids from r. It accepts three shapes and detects them
// from the first non-space byte: newline text (blank lines skipped, '#' lines
// skipped), a JSON array of strings, or a JSON array of objects. An object
// contributes its "id" key, else its "session_id" key. Any other shape is an
// error naming the line or index. Duplicates collapse, first occurrence wins.
// An input that names no id returns ErrNoSessionIDs.
func ReadSessionIDs(r io.Reader) ([]string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read session ids: %w", err)
	}
	trimmed := bytes.TrimLeftFunc(data, unicode.IsSpace)
	var ids []string
	if len(trimmed) > 0 && trimmed[0] == '[' {
		ids, err = readJSONSessionIDs(data)
	} else {
		ids, err = readTextSessionIDs(data)
	}
	if err != nil {
		return nil, err
	}
	ids = dedupe(ids)
	if len(ids) == 0 {
		return nil, ErrNoSessionIDs
	}
	return ids, nil
}

func readTextSessionIDs(data []byte) ([]string, error) {
	var ids []string
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
		ids = append(ids, text)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read session ids: %w", err)
	}
	return ids, nil
}

func readJSONSessionIDs(data []byte) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var elements []json.RawMessage
	if err := decoder.Decode(&elements); err != nil {
		return nil, fmt.Errorf("JSON array: %w", err)
	}
	rest := data[decoder.InputOffset():]
	if extra := bytes.TrimSpace(rest); len(extra) > 0 {
		offset := int(decoder.InputOffset()) + bytes.Index(rest, extra[:1])
		return nil, fmt.Errorf("line %d: unexpected content after the JSON array; give either one JSON array or one id per line",
			bytes.Count(data[:offset], []byte("\n"))+1)
	}
	ids := make([]string, 0, len(elements))
	for index, raw := range elements {
		id, err := sessionIDFromElement(raw)
		if err != nil {
			return nil, fmt.Errorf("index %d: %w", index, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func sessionIDFromElement(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", errors.New("empty element")
	}
	switch trimmed[0] {
	case '"':
		var id string
		if err := json.Unmarshal(trimmed, &id); err != nil {
			return "", err
		}
		return nonEmptyID(id)
	case '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return "", err
		}
		for _, key := range []string{"id", "session_id"} {
			value, ok := object[key]
			if !ok {
				continue
			}
			var id string
			if err := json.Unmarshal(value, &id); err != nil {
				return "", fmt.Errorf("%q is not a string", key)
			}
			return nonEmptyID(id)
		}
		return "", errors.New(`object has neither an "id" nor a "session_id" key`)
	default:
		return "", fmt.Errorf("%s is neither a session id string nor an object", trimmed)
	}
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
