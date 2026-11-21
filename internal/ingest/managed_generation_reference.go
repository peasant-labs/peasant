package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

// A context change after an adapter bump leaves the transcript bytes, and so
// the parsed rows, as they were: only the captured context (project, git,
// host, cwd, purpose, parent, redaction context) moved. Instead of parsing
// the transcript again, the store can install a new generation that carries
// the new context and reads the parsed rows of the stored generation by
// reference. The plan runs read-only on a reader connection; the install runs
// on the write lane and moves the active generation. Any refusal or error
// sends the session to the ordinary parse.

// ManagedGenerationReferenceRequest asks the store to plan a new generation
// that references the parsed rows of the active generation.
type ManagedGenerationReferenceRequest struct {
	// Proof is the captured input the cheap proof and the validator saw.
	Proof ManagedGenerationUnchangedProof
	// Candidate is the builder's metadata half the validator compared: the
	// new generation's context comes from it and from nothing else.
	Candidate GenerationMetadataCandidate
	// GenerationID is the fresh identity of the new generation, from the
	// same source a parse would use.
	GenerationID string
	// ParseInputContextFields lists the dotted JSON paths of UnifiedMetadata
	// fields the parsed rows depend on. A difference in any of them between
	// the candidate and the stored generation refuses the reference.
	ParseInputContextFields []string
}

// ManagedGenerationReferencePlan is the store's opaque plan. Only the store
// that made it can install it.
type ManagedGenerationReferencePlan interface {
	SessionID() SessionID
	// SourceGenerationID is the active generation the plan read.
	SourceGenerationID() string
	// OwnerGenerationID is the generation whose parsed rows the new
	// generation reads.
	OwnerGenerationID() string
}

// ManagedGenerationReferenceOutcome reports an installed reference.
type ManagedGenerationReferenceOutcome struct {
	// GenerationID is the new active generation.
	GenerationID string
	// OwnerGenerationID is the generation whose parsed rows it reads.
	OwnerGenerationID string
}

// ManagedGenerationReferencer is the optional store capability that installs
// a context change as a new generation without a parse.
type ManagedGenerationReferencer interface {
	PlanManagedGenerationReference(context.Context, ManagedGenerationReferenceRequest) (ManagedGenerationReferencePlan, error)
	InstallManagedGenerationReference(context.Context, ManagedGenerationReferencePlan) (ManagedGenerationReferenceOutcome, error)
}

// ReferenceRefusalReason names why the store refused to plan or install a
// reference. Every refusal writes nothing, and the session parses.
type ReferenceRefusalReason string

const (
	// ReferenceRefusalNotContextOnly: a term other than the context terms
	// failed, or the difference is not a context difference.
	ReferenceRefusalNotContextOnly ReferenceRefusalReason = "not-context-only"
	// ReferenceRefusalContentIdentity: the candidate's harness, content hash,
	// source format, completeness or source-evidence digest differs from the
	// stored generation's.
	ReferenceRefusalContentIdentity ReferenceRefusalReason = "content-identity-changed"
	// ReferenceRefusalContextUnchanged: the candidate's context equals the
	// stored context, so the validated settle applies instead.
	ReferenceRefusalContextUnchanged ReferenceRefusalReason = "context-unchanged"
	// ReferenceRefusalParseInputContext: a field the parsed rows depend on
	// differs between the candidate and the stored generation.
	ReferenceRefusalParseInputContext ReferenceRefusalReason = "parse-input-context-changed"
	// ReferenceRefusalStatsIncomplete: the stored parse counts are incomplete.
	ReferenceRefusalStatsIncomplete ReferenceRefusalReason = "stats-incomplete"
	// ReferenceRefusalOwnerSeal: the stored seal of the source does not
	// match its catalog row and the owner's parsed rows.
	ReferenceRefusalOwnerSeal ReferenceRefusalReason = "owner-seal-mismatch"
	// ReferenceRefusalLatestAttempt: the session holds a latest attempt.
	ReferenceRefusalLatestAttempt ReferenceRefusalReason = "latest-attempt-held"
	// ReferenceRefusalSourceMoved: at install, the active generation, the
	// index state, the source's or owner's seal, or the owner's ownership
	// changed after the plan.
	ReferenceRefusalSourceMoved ReferenceRefusalReason = "source-moved"
)

// AllReferenceRefusalReasons returns every refusal reason.
func AllReferenceRefusalReasons() []ReferenceRefusalReason {
	return []ReferenceRefusalReason{
		ReferenceRefusalNotContextOnly, ReferenceRefusalContentIdentity, ReferenceRefusalContextUnchanged,
		ReferenceRefusalParseInputContext, ReferenceRefusalStatsIncomplete, ReferenceRefusalOwnerSeal,
		ReferenceRefusalLatestAttempt, ReferenceRefusalSourceMoved,
	}
}

// NewReferenceRefusalReason validates a raw refusal reason.
func NewReferenceRefusalReason(raw string) (ReferenceRefusalReason, error) {
	reason := ReferenceRefusalReason(raw)
	if slices.Contains(AllReferenceRefusalReasons(), reason) {
		return reason, nil
	}
	return "", fmt.Errorf("ingest: read reference refusal reason %q: this build does not recognize it; no refusal was accepted; use a supported reason", raw)
}

// ReferenceRefusal is an error a referencer returns when it refuses a
// reference. Its reason is one of AllReferenceRefusalReasons.
type ReferenceRefusal interface {
	error
	RefusalReason() ReferenceRefusalReason
}

// StageRefreshReference counts reference attempts in Total and installs in
// Done.
const StageRefreshReference Stage = "refresh_reference"

// stageRefreshReferenceRefusedPrefix prefixes the stage that counts the
// references refused for one reason.
const stageRefreshReferenceRefusedPrefix = "refresh_reference_refused:"

// StageRefreshReferenceError counts reference attempts that ended in an
// error that is not a refusal.
const StageRefreshReferenceError Stage = "refresh_reference_error"

// StageRefreshReferenceRefused is the stage that counts, in Total, the
// references refused for reason.
func StageRefreshReferenceRefused(reason ReferenceRefusalReason) Stage {
	return Stage(stageRefreshReferenceRefusedPrefix + string(reason))
}

// recordReferenceFailure counts one refused or failed reference.
func (p *Pipeline) recordReferenceFailure(err error) {
	var refusal ReferenceRefusal
	if errors.As(err, &refusal) {
		if reason, valid := NewReferenceRefusalReason(string(refusal.RefusalReason())); valid == nil {
			p.config.IndexProfiler.RecordStage(StageRefreshReferenceRefused(reason), 0, 0, 1)
			return
		}
	}
	p.config.IndexProfiler.RecordStage(StageRefreshReferenceError, 0, 0, 1)
}

// indexManagedReferenceReason is the index-log reason of a session whose
// context change was installed as a reference to the stored parse.
const indexManagedReferenceReason = "stored index already matches captured input; installed the changed context as a new generation that references the stored parse"

// ParseInputContextFields lists, per harness, the dotted JSON paths of the
// captured metadata that the parsed rows depend on. It is empty for every
// harness: the context-independence test shows that no captured context
// field changes a parsed row. A field that does must be listed here, so a
// change to it parses instead of referencing.
func ParseInputContextFields(Harness) []string { return nil }

// parseInputContextFields returns the pipeline's list for a harness: the
// production list, or a test's override.
func (p *Pipeline) parseInputContextFields(h Harness) []string {
	if p.parseInputContextFieldsOverride != nil {
		return p.parseInputContextFieldsOverride(h)
	}
	return ParseInputContextFields(h)
}

// referenceManagedGeneration plans and installs a reference for a session
// whose validator found only a context difference. It reports whether the
// reference was installed, and the install's time on the write lane. Every
// refusal or error reports false: the caller parses, and the store keeps the
// old generation active until that parse commits.
func (p *Pipeline) referenceManagedGeneration(ctx context.Context, input *CapturedIndexInput, lane *storeWriteLane, candidate *GenerationMetadataCandidate) (installed bool, writeDuration time.Duration) {
	referencer, ok := p.metricsStore.(ManagedGenerationReferencer)
	if !ok || candidate == nil || p.config.Force || p.config.Reindex || p.config.DisableRefreshSkips {
		return false, 0
	}
	start := time.Now()
	defer func() {
		done := 0
		if installed {
			done = 1
		}
		p.config.IndexProfiler.RecordStage(StageRefreshReference, time.Since(start), done, 1)
	}()
	proof, err := p.unchangedProofRequest(input)
	if err != nil {
		return false, 0
	}
	request := ManagedGenerationReferenceRequest{
		Proof:                   proof,
		Candidate:               p.referenceCandidate(input.session, *candidate),
		GenerationID:            p.generationIdentity(input.session),
		ParseInputContextFields: p.parseInputContextFields(input.session.Harness),
	}
	plan, err := referencer.PlanManagedGenerationReference(ctx, request)
	if err != nil {
		p.recordReferenceFailure(err)
		slog.Debug("ingest: reference plan refused; the session parses", "session_id", input.session.SessionID, "error", err)
		return false, 0
	}
	writeStart := time.Now()
	var outcome ManagedGenerationReferenceOutcome
	p.runStoreWrite(lane, StageIndex, func() {
		outcome, err = referencer.InstallManagedGenerationReference(ctx, plan)
	})
	writeDuration = time.Since(writeStart)
	if err != nil {
		p.recordReferenceFailure(err)
		slog.Debug("ingest: reference install refused; the session parses", "session_id", input.session.SessionID, "error", err)
		return false, writeDuration
	}
	slog.Debug("ingest: installed a context change as a reference", "session_id", input.session.SessionID, "generation_id", outcome.GenerationID, "owner_generation_id", outcome.OwnerGenerationID)
	return true, writeDuration
}

// referenceCandidate applies to the metadata half the one change the
// parse's activation applies to the builder's metadata: a missing adapter
// revision takes the harness's target revision. The reference's seal and
// catalog row then equal those a parse of the same input installs.
func (p *Pipeline) referenceCandidate(session DiscoveredSession, candidate GenerationMetadataCandidate) GenerationMetadataCandidate {
	target := p.versionTargets()[session.Harness]
	if candidate.Metadata.AdapterVersion == nil && target.AdapterVersion > 0 {
		version := target.AdapterVersion
		candidate.Metadata.AdapterVersion = &version
	}
	return candidate
}
