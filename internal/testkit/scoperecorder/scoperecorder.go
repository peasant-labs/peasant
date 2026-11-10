// Package scoperecorder records the scope every scope-bearing store call a
// test double observed.
//
// It is a standard-library-only leaf package, like internal/testkit/fsdecorator
// and for the same reason: the shared doubles in internal/testutil and the
// white-box doubles in package ingest both need it, and internal/testutil
// imports ingest, so a recorder kept there cannot be imported by the package
// that owns seven of the nine scope-bearing passes. The recorder therefore
// carries only neutral strings; each caller converts its own scope value.
//
// The shape is operation-keyed: one generic Call slice, never an enumerated
// field per method. Adding a scope-bearing store read adds one method and one
// Record call, and no fixture has to grow an entry for it.
package scoperecorder

import "sync"

// Kind names which scope value a call carried.
type Kind uint8

const (
	// KindUnset is the zero value: no call has been recorded with it.
	KindUnset Kind = iota
	// KindStore is a call that took a session StoreScope.
	KindStore
	// KindProjects is a call that took a ProjectScope.
	KindProjects
)

// Call is one recorded scope-bearing call.
type Call struct {
	// Operation is the store method the double implemented.
	Operation string
	// Kind selects which of IDs and ProjectHashes is meaningful.
	Kind Kind
	// All reports the full-store scope. When true, IDs and ProjectHashes are
	// empty by construction, because an all-store value names nothing.
	All bool
	// IDs holds the narrowed session ids, in caller order.
	IDs []string
	// ProjectHashes holds the narrowed project hashes, in caller order.
	ProjectHashes []string
}

// Recorder records scope-bearing calls. Its zero value is ready to use and
// safe for concurrent recording and reading.
type Recorder struct {
	mu    sync.Mutex
	calls []Call
}

// RecordStore records one call that took a session scope. ids must be the
// narrowed ids (empty when all is true); all reports the full-store value.
func (r *Recorder) RecordStore(operation string, ids []string, all bool) {
	call := Call{Operation: operation, Kind: KindStore, All: all}
	if !all {
		call.IDs = append([]string(nil), ids...)
	}
	r.append(call)
}

// RecordProjects records one call that took a project scope.
func (r *Recorder) RecordProjects(operation string, hashes []string, all bool) {
	call := Call{Operation: operation, Kind: KindProjects, All: all}
	if !all {
		call.ProjectHashes = append([]string(nil), hashes...)
	}
	r.append(call)
}

func (r *Recorder) append(call Call) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
}

// Calls returns a copy of the recorded calls, in call order.
func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Call, len(r.calls))
	for index, call := range r.calls {
		out[index] = call
		out[index].IDs = append([]string(nil), call.IDs...)
		out[index].ProjectHashes = append([]string(nil), call.ProjectHashes...)
	}
	return out
}

// Reset drops every recorded call.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}
