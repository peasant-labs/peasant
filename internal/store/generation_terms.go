package store

import (
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// unchangedTermMode selects how the unchanged terms decide the two context
// terms (metadata-context and redaction-context). Every other term is
// evaluated the same way, in the same order, in every mode.
type unchangedTermMode int

const (
	// termModeStoredDigests is the cheap proof: the captured metadata must
	// hash to the stored metadata_hash, and the captured redaction context
	// must equal the stored one.
	termModeStoredDigests unchangedTermMode = iota
	// termModeValidateContext skips the two digest terms and, once every
	// other term holds, compares the validated candidate's context with the
	// stored context rows.
	termModeValidateContext
	// termModeSettleValidated is termModeValidateContext under the settle's
	// lock: the comparison runs again, with the same candidate, inside the
	// settle's write transaction.
	termModeSettleValidated
)

// contextModes reports whether the mode replaces the digest terms with the
// candidate comparison.
func (m unchangedTermMode) contextMode() bool {
	return m == termModeValidateContext || m == termModeSettleValidated
}

func nonzeroDigest(s string) bool { return validIndexInputHash(s) && strings.Trim(s, "0") != "" }

// evaluateUnchangedTermsOnConn evaluates the unchanged terms of one captured
// input against the catalog and index state on conn, in a fixed order; the
// first term that fails names the mismatch. It reads the session row, the
// active generation row and the index state; in a context mode it also reads
// the stored context rows of the active generation. It never reads a mapping
// row, a body, a blob or a stat.
//
// A context mode requires p.Validated: without a candidate the context terms
// cannot hold, so the result is not proven with metadata-context.
func evaluateUnchangedTermsOnConn(conn *sqlite.Conn, p ManagedGenerationUnchangedProof, mode unchangedTermMode) (ingest.UnchangedProof, error) {
	mismatch := func(reason ingest.UnchangedProofMismatch) (ingest.UnchangedProof, error) {
		return ingest.UnchangedProof{Mismatch: reason}, nil
	}
	var active, attempt, content, metadataHash, harness, format, completeness, evidence, seal, level, redactionHash string
	var indexFormat, applied int
	var adapter, derived *int64
	var activated bool
	found := false
	err := sqlitex.Execute(conn, `SELECT COALESCE(s.active_generation_id,''), COALESCE(s.latest_attempt_generation_id,''),
 g.content_hash,g.metadata_hash,g.harness,g.source_format,g.completeness,g.source_evidence_digest,
 g.candidate_digest,g.index_format_version,g.redaction_applied,COALESCE(g.redaction_level,''),
 COALESCE(g.redaction_content_hash_at_redact,''),g.adapter_version,g.derived_at,g.activated_at_ms
 FROM sessions s LEFT JOIN session_generations g ON g.session_id=s.session_id AND g.generation_id=s.active_generation_id
 WHERE s.session_id=?`, &sqlitex.ExecOptions{Args: []any{string(p.SessionID)}, ResultFunc: func(st *sqlite.Stmt) error {
		found = true
		active = st.ColumnText(0)
		attempt = st.ColumnText(1)
		content = st.ColumnText(2)
		metadataHash = st.ColumnText(3)
		harness = st.ColumnText(4)
		format = st.ColumnText(5)
		completeness = st.ColumnText(6)
		evidence = st.ColumnText(7)
		seal = st.ColumnText(8)
		indexFormat = st.ColumnInt(9)
		applied = st.ColumnInt(10)
		level = st.ColumnText(11)
		redactionHash = st.ColumnText(12)
		adapter = nullableInt64Col(st, 13)
		derived = nullableInt64Col(st, 14)
		activated = st.ColumnType(15) != sqlite.TypeNull
		return nil
	}})
	if err != nil {
		return ingest.UnchangedProof{}, fmt.Errorf("store: read generation for unchanged proof of %s: %w; retry with database access restored", p.SessionID, err)
	}
	if !found || active == "" || active != p.GenerationID || p.Metadata.SessionID != p.SessionID {
		return mismatch(ingest.UnchangedNotProvenActive)
	}
	if mode.contextMode() && (p.Validated == nil || p.Validated.GenerationID != active) {
		// A validated answer binds one generation; it never carries over to
		// another, even one with the same content.
		return mismatch(ingest.UnchangedNotProvenActive)
	}
	if indexFormat != 2 || !activated {
		return mismatch(ingest.UnchangedNotProvenUnsupported)
	}
	if attempt != "" {
		return mismatch(ingest.UnchangedNotProvenLatestAttempt)
	}
	if !nonzeroDigest(content) || content != p.Metadata.ContentHash {
		return mismatch(ingest.UnchangedNotProvenContent)
	}
	if format != string(p.Metadata.Source.Format) {
		return mismatch(ingest.UnchangedNotProvenSourceFormat)
	}
	if harness != string(p.Metadata.ModelHarness) {
		return mismatch(ingest.UnchangedNotProvenHarness)
	}
	if completeness != string(indexformat.GenerationCompletenessComplete) {
		return mismatch(ingest.UnchangedNotProvenCompleteness)
	}
	if !nonzeroDigest(evidence) {
		return mismatch(ingest.UnchangedNotProvenSourceEvidence)
	}
	if !mode.contextMode() {
		meta := p.Metadata
		meta.AdapterVersion = nil
		if adapter != nil {
			v := int(*adapter)
			meta.AdapterVersion = &v
		}
		if !nonzeroDigest(metadataHash) || statsExcludedMetadataHash(meta) != metadataHash || (meta.DerivedAt != nil && !sameIndexValue(meta.DerivedAt, derived)) {
			return mismatch(ingest.UnchangedNotProvenMetadata)
		}
		if (applied == 1) != meta.Redaction.Applied || level != meta.Redaction.Level || redactionHash != meta.Redaction.ContentHashAtRedact {
			return mismatch(ingest.UnchangedNotProvenRedaction)
		}
	}
	state, err := readIndexStateOnConn(conn, p.SessionID)
	if err != nil {
		return ingest.UnchangedProof{}, err
	}
	if state == nil || p.IndexerTarget <= 0 || p.FormatTarget != 2 || state.IndexerVersion != p.IndexerTarget || state.IndexVersion == nil || *state.IndexVersion != p.FormatTarget || state.IndexedAt == nil {
		return mismatch(ingest.UnchangedNotProvenIndexer)
	}
	if !nonzeroDigest(p.InputDigest) || state.IndexedInputHash == nil || *state.IndexedInputHash != p.InputDigest {
		return mismatch(ingest.UnchangedNotProvenInputProof)
	}
	if !nonzeroDigest(seal) {
		return mismatch(ingest.UnchangedNotProvenSeal)
	}
	if p.Expected == nil || !sameIndexState(p.Expected, state) || !sameIndexValue(p.Expected.LatestAttemptArtifactHash, state.LatestAttemptArtifactHash) || !sameIndexValue(p.Expected.LatestAttemptIndexerVersion, state.LatestAttemptIndexerVersion) || !sameIndexValue(p.Expected.LatestAttemptAdapterVersion, state.LatestAttemptAdapterVersion) || p.Expected.ContentFailureCode != state.ContentFailureCode {
		return mismatch(ingest.UnchangedNotProvenInputProof)
	}
	if mode.contextMode() {
		equal, err := candidateContextMatchesOnConn(conn, p.SessionID, active, p.Validated.Candidate)
		if err != nil {
			return ingest.UnchangedProof{}, err
		}
		if !equal {
			return mismatch(ingest.UnchangedNotProvenMetadata)
		}
	}
	return ingest.UnchangedProof{Proven: true}, nil
}
