package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/peasant-labs/schema"
)

// openCodeProvenancePriorFormatVersion is the persisted prior document version.
// A reader refuses any other version rather than guessing at a different shape.
const openCodeProvenancePriorFormatVersion = 1

// openCodeProvenancePriorDocument is the durable, activation-owned form of
// OpenCodeProvenancePrior. It carries the alias and submission maps as ordered
// records so the encoding is deterministic, plus the retained captured-prefix
// rows the indexer needs when the live fork source no longer carries them.
type openCodeProvenancePriorDocument struct {
	Version           int                        `json:"version"`
	Entries           []openCodePriorEntryAlias  `json:"entries"`
	Submissions       []openCodePriorAcceptAlias `json:"submissions"`
	CapturedPrefix    []OpenCodeHistoryRow       `json:"capturedPrefix,omitempty"`
	HasCapturedPrefix bool                       `json:"hasCapturedPrefix"`
	HasComplete       bool                       `json:"hasCompleteGeneration"`
}

type openCodePriorEntryAlias struct {
	NativeKey string                `json:"nativeKey"`
	Ref       schema.SourceEntryRef `json:"ref"`
}

type openCodePriorAcceptAlias struct {
	NativeKey string               `json:"nativeKey"`
	Ref       schema.SubmissionRef `json:"ref"`
}

// EncodeOpenCodeProvenancePrior serializes the activation-owned prior evidence
// into a stable document the activation persists beside the managed generation.
// The indexer never generates this evidence; the encoding exists so a reopen
// can load the same aliases and retained captured-prefix rows from durable
// storage instead of from memory.
func EncodeOpenCodeProvenancePrior(prior OpenCodeProvenancePrior) ([]byte, error) {
	document := openCodeProvenancePriorDocument{
		Version:           openCodeProvenancePriorFormatVersion,
		HasCapturedPrefix: prior.HasCapturedPrefix,
		HasComplete:       prior.HasCompleteGeneration,
	}
	for nativeKey, ref := range prior.Aliases.Entries {
		document.Entries = append(document.Entries, openCodePriorEntryAlias{NativeKey: nativeKey, Ref: ref})
	}
	for nativeKey, ref := range prior.Aliases.Submissions {
		document.Submissions = append(document.Submissions, openCodePriorAcceptAlias{NativeKey: nativeKey, Ref: ref})
	}
	if prior.HasCapturedPrefix {
		document.CapturedPrefix = append([]OpenCodeHistoryRow(nil), prior.CapturedPrefix...)
	}
	sortOpenCodePriorAliases(document.Entries, document.Submissions)
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("ingest.EncodeOpenCodeProvenancePrior: encoding the prior alias and captured-prefix evidence failed; the prior cannot be persisted and a reopen would rekey unchanged source; retry the activation: %w", err)
	}
	return encoded, nil
}

// DecodeOpenCodeProvenancePrior reads a persisted prior document. The decode is
// strict: unknown fields and an unknown version are refused so a newer document
// never silently loses evidence.
func DecodeOpenCodeProvenancePrior(data []byte) (OpenCodeProvenancePrior, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document openCodeProvenancePriorDocument
	if err := decoder.Decode(&document); err != nil {
		return OpenCodeProvenancePrior{}, fmt.Errorf("ingest.DecodeOpenCodeProvenancePrior: the persisted prior evidence is not a supported document; the prior cannot be loaded and the candidate is refused; rewrite the prior from a valid generation: %w", err)
	}
	if err := ensureOpenCodePriorEOF(decoder); err != nil {
		return OpenCodeProvenancePrior{}, err
	}
	if document.Version != openCodeProvenancePriorFormatVersion {
		return OpenCodeProvenancePrior{}, fmt.Errorf("ingest.DecodeOpenCodeProvenancePrior: the persisted prior evidence declares version %d, but this build reads version %d; the prior cannot be interpreted; rewrite the prior from a valid generation", document.Version, openCodeProvenancePriorFormatVersion)
	}
	prior := OpenCodeProvenancePrior{
		Aliases:               NewProjectionPriorState(),
		HasCapturedPrefix:     document.HasCapturedPrefix,
		HasCompleteGeneration: document.HasComplete,
	}
	for _, alias := range document.Entries {
		prior.Aliases.Entries[alias.NativeKey] = alias.Ref
	}
	for _, alias := range document.Submissions {
		prior.Aliases.Submissions[alias.NativeKey] = alias.Ref
	}
	if document.HasCapturedPrefix {
		prior.CapturedPrefix = append([]OpenCodeHistoryRow(nil), document.CapturedPrefix...)
	}
	return prior, nil
}

// sortOpenCodePriorAliases orders the encoded alias and submission records by
// native key so the persisted bytes are deterministic across runs.
func sortOpenCodePriorAliases(entries []openCodePriorEntryAlias, submissions []openCodePriorAcceptAlias) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].NativeKey < entries[j].NativeKey })
	sort.Slice(submissions, func(i, j int) bool { return submissions[i].NativeKey < submissions[j].NativeKey })
}

func ensureOpenCodePriorEOF(decoder *json.Decoder) error {
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("ingest.DecodeOpenCodeProvenancePrior: the persisted prior document carries trailing content; the prior cannot be interpreted; rewrite the prior from a valid generation")
	}
	return nil
}
