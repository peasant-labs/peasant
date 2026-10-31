package ingest

import (
	"encoding/json"
	"strings"

	"github.com/peasant-labs/schema"
)

// CommandInvocationFromEntry projects the command an entry recorded at index
// time onto the wire type. The indexers record it in the entry's Extra, which
// every stored representation carries. The recorded name gets the leading
// slash the wire requires when the harness recorded it bare. It returns nil
// when the entry recorded no command and when the recorded name cannot form a
// valid invocation, which is also how a built-in harness command stays off the
// wire, because schema.NewCommandInvocation refuses one.
func CommandInvocationFromEntry(entry schema.SessionEntry) *schema.CommandInvocation {
	if entry.Extra == nil {
		return nil
	}
	var stored struct {
		Name string          `json:"command_name"`
		Args json.RawMessage `json:"command_args"`
	}
	if err := json.Unmarshal([]byte(*entry.Extra), &stored); err != nil || stored.Name == "" {
		return nil
	}
	name := stored.Name
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	invocation, err := schema.NewCommandInvocation(name, commandArgsFromRaw(stored.Args))
	if err != nil {
		return nil
	}
	return &invocation
}

// commandArgsFromRaw reads a stored command_args value tolerantly: a JSON
// string becomes the args, and anything else (a number, an object, an array, a
// boolean, null, or the field being absent) is treated as empty args, so a
// non-string args value never drops the invocation.
func commandArgsFromRaw(raw json.RawMessage) string {
	var args string
	if len(raw) == 0 {
		return args
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return ""
	}
	return args
}
