package ingest

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// RefAllocator mints the opaque public references the shared projection assigns
// to projected blocks and submissions. Production uses RandomRefAllocator; a
// deterministic replay or a fixture injects a sequential allocator through the
// same interface, so the production code path is the one under test.
type RefAllocator interface {
	NewEntryRef() (schema.SourceEntryRef, error)
	NewSubmissionRef() (schema.SubmissionRef, error)
}

// RandomRefAllocator mints a fresh 128-bit opaque reference for every call. A
// projected ref is random, never a hash of native content, so two byte-equal
// blocks keep distinct identities.
type RandomRefAllocator struct{}

// NewEntryRef returns one opaque block reference.
func (RandomRefAllocator) NewEntryRef() (schema.SourceEntryRef, error) {
	ref, err := randomOpaqueRef("e")
	if err != nil {
		return "", err
	}
	return schema.SourceEntryRef(ref), nil
}

// NewSubmissionRef returns one opaque submission reference.
func (RandomRefAllocator) NewSubmissionRef() (schema.SubmissionRef, error) {
	ref, err := randomOpaqueRef("s")
	if err != nil {
		return "", err
	}
	return schema.SubmissionRef(ref), nil
}

// randomOpaqueRef encodes 128 random bits with a one-letter kind prefix. The
// result is bounded well inside the 96-byte public-reference limit.
func randomOpaqueRef(kind string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("ingest.randomOpaqueRef: reading operating-system randomness failed while allocating a %q reference; the projection cannot mint a stable identity; retry the ingest run or verify the system entropy source: %w", kind, err)
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])
	return kind + "_" + encoded, nil
}

const (
	projectionBlockAliasPrefix  = "block:"
	projectionAcceptAliasPrefix = "accept:"
)

// ProjectionPriorState is the alias state a prior candidate or activation left
// behind. The projection consults it before allocating so a re-run, an append
// or a retry reuses every identity it already proved.
type ProjectionPriorState struct {
	// Entries maps a classified block native key to its allocated block ref.
	Entries map[string]schema.SourceEntryRef
	// Submissions maps a native acceptance key to its allocated submission ref.
	Submissions map[string]schema.SubmissionRef
}

// NewProjectionPriorState returns an empty alias state.
func NewProjectionPriorState() ProjectionPriorState {
	return ProjectionPriorState{
		Entries:     make(map[string]schema.SourceEntryRef),
		Submissions: make(map[string]schema.SubmissionRef),
	}
}

// clone returns a deep copy so a builder never mutates a caller's prior state.
func (s ProjectionPriorState) clone() ProjectionPriorState {
	out := NewProjectionPriorState()
	for key, ref := range s.Entries {
		out.Entries[key] = ref
	}
	for key, ref := range s.Submissions {
		out.Submissions[key] = ref
	}
	return out
}

// PriorStateFromGeneration rebuilds the alias state from a persisted managed
// generation so the next capture can reuse its identities. Block aliases are
// copied directly. An acceptance alias names the first submitted input block;
// its submission ref is recovered from that block's provenance, which is the
// single authority for the value.
func PriorStateFromGeneration(generation indexformat.Generation) (ProjectionPriorState, error) {
	state := NewProjectionPriorState()
	byRef := make(map[schema.SourceEntryRef]*schema.ContentProvenance)
	index := func(entries []schema.SessionEntry) {
		for i := range entries {
			entry := entries[i]
			if entry.SourceEntryRef != "" && entry.Provenance != nil {
				byRef[entry.SourceEntryRef] = entry.Provenance
			}
		}
	}
	index(generation.Main.Entries)
	for i := range generation.Earlier {
		index(generation.Earlier[i].Content.Entries)
	}
	for _, alias := range generation.Aliases {
		switch {
		case strings.HasPrefix(alias.NativeKey, projectionBlockAliasPrefix):
			state.Entries[strings.TrimPrefix(alias.NativeKey, projectionBlockAliasPrefix)] = alias.Ref
		case strings.HasPrefix(alias.NativeKey, projectionAcceptAliasPrefix):
			key := strings.TrimPrefix(alias.NativeKey, projectionAcceptAliasPrefix)
			if provenance, ok := byRef[alias.Ref]; ok && provenance != nil && provenance.SubmissionRef != "" {
				state.Submissions[key] = provenance.SubmissionRef
			}
		}
	}
	return state, nil
}

// encodeBlockAliasKey encodes a classified block's native key into the bounded
// local alias key persisted for it.
func encodeBlockAliasKey(nativeKey string) (string, error) {
	return encodeAliasKey(projectionBlockAliasPrefix, nativeKey, "block")
}

// encodeSubmissionAliasKey encodes a native acceptance key into the bounded
// local alias key persisted for the first submitted input block.
func encodeSubmissionAliasKey(submissionKey string) (string, error) {
	return encodeAliasKey(projectionAcceptAliasPrefix, submissionKey, "submission")
}

// encodeAliasKey bounds and validates one opaque alias key. The raw native key
// never carries a private path: it is an opaque local identity or coordinate
// the adapter already encoded.
func encodeAliasKey(prefix, raw, kind string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("ingest.encodeAliasKey: %s native key is empty; a later capture cannot match the identity; encode a non-empty opaque native key", kind)
	}
	key := prefix + raw
	if !utf8.ValidString(key) {
		return "", fmt.Errorf("ingest.encodeAliasKey: %s native key is not valid UTF-8; the persisted alias cannot be matched; encode a valid opaque native key", kind)
	}
	if len(key) > indexformat.MaxNativeAliasKeyBytes {
		return "", fmt.Errorf("ingest.encodeAliasKey: encoded %s alias key is %d bytes, over the %d-byte bound; a raw private path may be leaking into persisted identity; encode a shorter opaque native key", kind, len(key), indexformat.MaxNativeAliasKeyBytes)
	}
	return key, nil
}
