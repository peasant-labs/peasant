package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/schema"
)

// ManagedArtifact is a captured metadata/transcript pair. Its hash identifies
// retained semantic input, not an indexer's input or proof of computation.
// Public values are revalidated at persistence boundaries before use.
type ManagedArtifact struct {
	Metadata     UnifiedMetadata
	MetadataJSON []byte
	Transcript   []byte
	ArtifactHash string
}

// NewManagedArtifact validates a captured pair without changing either file.
// Missing historical checksums remain missing; computing current identity does
// not invent a historical parser run or a previously verified checksum.
func NewManagedArtifact(metadataJSON, transcript []byte) (*ManagedArtifact, error) {
	meta, err := decodeManagedMetadata(metadataJSON, "captured artifact")
	if err != nil {
		return nil, fmt.Errorf("capture managed artifact before use: %w", err)
	}
	if meta.SchemaVersion < 1 {
		return nil, fmt.Errorf("capture managed artifact for session %s: missing positive schema version; no files or database rows were changed; restore valid managed metadata before retrying", meta.SessionID)
	}
	if _, err := NewSessionID(string(meta.SessionID)); err != nil {
		return nil, err
	}
	if meta.ParentUUID != nil {
		if _, err := NewSessionID(string(*meta.ParentUUID)); err != nil {
			return nil, err
		}
		if *meta.ParentUUID == meta.SessionID {
			return nil, fmt.Errorf("capture managed artifact for session %s: session cannot be its own parent; restore the recorded parent before retrying", meta.SessionID)
		}
	}
	if _, err := NewHostSlug(string(meta.HostSlug)); err != nil {
		return nil, err
	}
	if _, ok := HarvesterVersionRegistry[meta.ModelHarness]; !ok {
		return nil, fmt.Errorf("capture managed artifact for session %s: harness %q has no supported reader; preserve the files and use a compatible Peasant build", meta.SessionID, meta.ModelHarness)
	}
	if meta.Source.Format != SourceFormatJSON && meta.Source.Format != SourceFormatJSONL {
		return nil, fmt.Errorf("capture managed artifact for session %s: unsupported transcript format %q; no files were changed; restore its JSON or JSONL metadata or use a compatible build", meta.SessionID, meta.Source.Format)
	}
	contentHash := schema.ComputeTranscriptHash(transcript)
	if meta.ContentHash != "" && meta.ContentHash != contentHash {
		return nil, fmt.Errorf("capture managed artifact for session %s: transcript checksum does not match committed metadata; the pair cannot be used safely; run harvest to recover or restore matching retained files", meta.SessionID)
	}
	if meta.MetadataHash != "" && meta.MetadataHash != schema.ComputeMetadataHash(meta) {
		return nil, fmt.Errorf("capture managed artifact for session %s: metadata checksum is inconsistent; no producer evidence was changed; restore valid committed metadata before retrying", meta.SessionID)
	}
	semantic, err := artifactSemanticJSON(metadataJSON, contentHash)
	if err != nil {
		return nil, err
	}
	return &ManagedArtifact{Metadata: *meta, MetadataJSON: bytes.Clone(metadataJSON), Transcript: bytes.Clone(transcript), ArtifactHash: schema.ComputeTranscriptHash(semantic)}, nil
}

// Validate refuses mutated or fabricated snapshots before mirroring them.
func (a *ManagedArtifact) Validate() error {
	if a == nil {
		return fmt.Errorf("validate managed artifact before persistence: no captured pair was supplied; capture matching committed metadata and transcript before retrying")
	}
	checked, err := NewManagedArtifact(a.MetadataJSON, a.Transcript)
	if err != nil {
		return err
	}
	if checked.ArtifactHash != a.ArtifactHash || !reflect.DeepEqual(checked.Metadata, a.Metadata) {
		return fmt.Errorf("validate managed artifact for session %s before persistence: captured metadata or hash was changed after validation; no database rows were changed; capture the committed pair again", a.Metadata.SessionID)
	}
	return nil
}

// MetricSeed distinguishes absent historical statistics from an explicit
// retained zero. Callers validate the captured pair before using its seed.
func (a *ManagedArtifact) MetricSeed() *StatsInfo {
	var fields map[string]json.RawMessage
	if a == nil || json.Unmarshal(a.MetadataJSON, &fields) != nil {
		return nil
	}
	raw, present := fields["stats"]
	if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	stats := a.Metadata.Stats
	return &stats
}

func artifactSemanticJSON(data []byte, contentHash string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	delete(fields, "derivedAt")
	delete(fields, "metadataHash")
	fields["contentHash"], _ = json.Marshal(contentHash)
	var timestamps map[string]json.RawMessage
	if err := json.Unmarshal(fields["timestamp"], &timestamps); err != nil {
		return nil, fmt.Errorf("capture managed artifact semantic timestamp: %w; restore valid metadata before retrying", err)
	}
	delete(timestamps, "ingested")
	fields["timestamp"], _ = json.Marshal(timestamps)
	if raw := fields["redaction"]; len(raw) != 0 {
		var redaction map[string]json.RawMessage
		if err := json.Unmarshal(raw, &redaction); err != nil {
			return nil, err
		}
		delete(redaction, "redacted_at_ms")
		fields["redaction"], _ = json.Marshal(redaction)
	}
	return json.Marshal(fields)
}

// ArtifactMirrorRequest carries only native evidence actually acquired for this
// artifact. Nil cursor/origin preserves the stored value; zero is valid evidence.
type ArtifactMirrorRequest struct {
	Artifact *ManagedArtifact
	EventSeq *int64
	Origin   *sessionorigin.Origin
}

// ArtifactMirrorResult reports committed success for one session, including
// explicit refusal when its parent or compatibility evidence is unavailable.
type ArtifactMirrorResult struct {
	SessionID SessionID
	Mirrored  bool
	Err       error
}

// ArtifactMirrorStore mirrors a committed file snapshot, seeds, commit bindings
// and acquired native evidence transactionally. Callers hold session ownership
// and verify the supplied pair still matches the committed files.
type ArtifactMirrorStore interface {
	MirrorArtifacts(context.Context, []ArtifactMirrorRequest) []ArtifactMirrorResult
}
