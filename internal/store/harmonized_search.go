package store

import (
	"context"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
)

// SearchState is the store-global search index health (design §3.5): exactly
// one row. It is set when a delete used untrustworthy FTS values; search
// refuses while it is set; the whole-index rebuild clears it.
type SearchState struct {
	NeedsRebuild bool
}

// SearchStateRead reports the store-global search index health. Stub: returns
// ErrHarmonizedNotImplemented until the search health tracking lands.
func SearchStateRead(ctx context.Context) (SearchState, error) {
	return SearchState{}, ErrHarmonizedNotImplemented
}

// SearchStateSetNeedsRebuild flags the store-global search index for a
// whole-index rebuild on the caller's connection: a deleting transaction
// calls it when the deleted row failed the serializeEntry digest check, so
// its indexed values cannot be trusted for the BEFORE DELETE un-indexing.
// (The contract sketch names tx *sqlitex.Tx; the vendored sqlite fork has no
// Tx type — every store helper takes *sqlite.Conn — so the seam takes the
// connection.) Stub: returns ErrHarmonizedNotImplemented until the search
// health tracking lands.
func SearchStateSetNeedsRebuild(ctx context.Context, conn *sqlite.Conn) error {
	return ErrHarmonizedNotImplemented
}
