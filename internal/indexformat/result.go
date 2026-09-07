// Package indexformat defines concrete local index results. It does not own
// harness parsing, SQLite persistence, or the public transcript contract.
package indexformat

import "github.com/peasant-labs/schema"

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
