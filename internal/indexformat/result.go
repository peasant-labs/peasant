// Package indexformat defines concrete local index results. It does not own
// harness parsing, SQLite persistence, or the public transcript contract.
package indexformat

import (
	"fmt"
	"github.com/peasant-labs/schema"
	"reflect"
)

// Result identifies the representation actually returned by an indexer.
// A Store handler validates the concrete type before it changes any data.
type Result interface {
	IndexVersion() int
}

// V1 is the current relational representation, without a duplicate payload.
type V1 struct {
	Entries []schema.SessionEntry
}

func (V1) IndexVersion() int { return 1 }

var _ Result = V1{}

// VersionOf rejects absent and typed-nil results before calling their methods.
// Empty V1 entries are still a concrete successful result, not absent output.
func VersionOf(result Result) (int, error) {
	if result == nil {
		return 0, fmt.Errorf("indexer returned no concrete result; parsing was not recorded as successful; return a concrete empty result only when parsing completed")
	}
	value := reflect.ValueOf(result)
	switch value.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		if value.IsNil() {
			return 0, fmt.Errorf("indexer returned no concrete result (%T is nil); no index replacement was authorized; correct the parser result", result)
		}
	}
	version := result.IndexVersion()
	if version < 1 {
		return 0, fmt.Errorf("indexer returned invalid index format %d; no replacement was authorized; return a concrete result with a positive format version", version)
	}
	return version, nil
}
