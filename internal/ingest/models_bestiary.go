package ingest

import (
	"time"

	"github.com/dayvidpham/bestiary"
)

// nilIfEmpty returns nil for an empty string, otherwise a pointer to a copy of s.
// It maps a bestiary value field (zero value == "absent") onto ingest.ModelInfo's
// optional pointer fields. Defined locally because the existing deref* helpers in
// entries_writer.go go the opposite direction (pointer -> any) and must not be reused.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nilIfZero returns nil for a zero int, otherwise a pointer to a copy of n.
// It maps a bestiary value field (0 == "absent") onto ingest.ModelInfo's optional
// pointer fields. See nilIfEmpty for why this is defined locally.
func nilIfZero(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

// ModelFromBestiary converts a single bestiary.ModelInfo into the local
// ingest.ModelInfo persisted in the models table.
//
// LastSynced follows a preserve-if-present rule: bestiary's live FetchModels
// leaves LastSynced empty (the caller owns the timestamp), whereas StaticModels
// carries the codegen snapshot timestamp. Preserving a present value means a
// static-fallback sync records the snapshot's true vintage instead of a faked
// "now"; an empty value is stamped with syncedAt (RFC3339 UTC), matching the
// behaviour of the retired models_client.go live path.
//
// Bestiary extras (Attachment, Temperature, StructuredOutput, Interleaved,
// OpenWeights, Knowledge, Modalities, Version/Variant) are outside this storage
// contract; only the 15 fields backing the models table are mapped.
func ModelFromBestiary(b bestiary.ModelInfo, syncedAt time.Time) ModelInfo {
	last := b.LastSynced
	if last == "" {
		last = syncedAt.UTC().Format(time.RFC3339)
	}
	return ModelInfo{
		ModelID: string(b.ID),
		// DO NOT TOUCH (TRAP): ProviderKey is the model-VENDOR credential, NOT the
		// coding-tool harness. The harness terminology change intentionally left
		// this credential key alone. Do not rename/flip it to a harness key —
		// guarded by ast-grep/no-trap-harness-flip.yml on the struct definition.
		ProviderKey:           b.Provider.String(),
		DisplayName:           b.DisplayName,
		Family:                nilIfEmpty(string(b.Family)),
		ContextWindow:         nilIfZero(b.ContextWindow),
		MaxOutput:             nilIfZero(b.MaxOutput),
		Reasoning:             b.Reasoning,
		ToolCall:              b.ToolCall,
		CostInputPerMTok:      b.CostInputPerMTok,
		CostOutputPerMTok:     b.CostOutputPerMTok,
		CostReasoningPerMTok:  b.CostReasoningPerMTok,
		CostCacheReadPerMTok:  b.CostCacheReadPerMTok,
		CostCacheWritePerMTok: b.CostCacheWritePerMTok,
		ReleaseDate:           nilIfEmpty(b.ReleaseDate),
		LastSynced:            last,
	}
}

// ModelsFromBestiary converts a slice of bestiary models, preserving order.
// A nil or empty input yields a nil output (no allocation).
func ModelsFromBestiary(bs []bestiary.ModelInfo, syncedAt time.Time) []ModelInfo {
	if len(bs) == 0 {
		return nil
	}
	out := make([]ModelInfo, len(bs))
	for i := range bs {
		out[i] = ModelFromBestiary(bs[i], syncedAt)
	}
	return out
}
