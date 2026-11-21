package ingest

import (
	"context"
	"testing"
)

// RetainedNoMaterializationEncodings is the closed set of stored encodings the
// allocation proof covers, single-sourced here so the internal fixture
// validator and the external allocation proof cannot drift apart: adding a
// sixth stored encoding updates this list, the noMaterializationCases
// fixture, and the proof dispatch together, and the proof fails until all
// three agree.
var RetainedNoMaterializationEncodings = []string{"payloadText", "rawPayload", "escapedKind", "escapedKey", "escapedRootKey"}

// IsRetainedNoMaterializationEncoding reports whether encoding belongs to the
// closed allocation-proof set above.
func IsRetainedNoMaterializationEncoding(encoding string) bool {
	for _, known := range RetainedNoMaterializationEncodings {
		if known == encoding {
			return true
		}
	}
	return false
}

// PairInstallFiles is the in-memory pair one install writes, as the external
// pair-install fixture supplies it.
type PairInstallFiles struct {
	TranscriptName string
	Transcript     []byte
	Debug          map[string][]byte
	SourceCapture  []byte
	MetadataName   string
	Metadata       []byte
}

// InstallManagedPair drives the production pair install with no writer lane,
// so the external fixture can count its filesystem and database work.
func (p *Pipeline) InstallManagedPair(ctx context.Context, sessionDir string, session DiscoveredSession, files PairInstallFiles) (transcriptReplaced, metadataReplaced bool, err error) {
	result, err := p.installManagedPair(ctx, sessionDir, string(session.SessionID), installedPair{
		transcriptName: files.TranscriptName,
		transcript:     files.Transcript,
		debug:          files.Debug,
		sourceCapture:  files.SourceCapture,
		metadataName:   files.MetadataName,
		metadata:       files.Metadata,
	}, session, nil)
	return result.TranscriptReplaced, result.MetadataReplaced, err
}

// FileCaptureEvidenceName names the private source-capture marker of a session.
func FileCaptureEvidenceName(sid SessionID) string { return fileCaptureEvidenceName(sid) }

// RefreshStoredMetadata drives the stored-copy metadata refresh of one session
// with no writer lane, so the external fixture can count its file reads. It
// reports whether the installed transcript file was kept.
func (p *Pipeline) RefreshStoredMetadata(ctx context.Context, session DiscoveredSession, metadataPath string) (transcriptKept bool, err error) {
	result := p.processRetainedSession(ctx, session, metadataPath, nil)
	return result.transcriptKept, result.result.Error
}

// IndexBindWithoutReparseReason is the index-log reason of a session settled
// by the rule "bind without a re-parse", so the external harvest fixture can
// count binds apart from parses.
const IndexBindWithoutReparseReason = indexBindWithoutReparseReason

// StoredDecisionForTest asks the stored-row decision for one session, so the
// external fixture can prove the next run settles to none. loc is the stored
// location the fixture read; no file is opened.
func StoredDecisionForTest(p *Pipeline, session DiscoveredSession, loc SessionLocation) RefreshDecision {
	return p.refreshDecisionForLocation(session, loc, nil)
}

// SetClaudeMetadataHalfMutationForTest installs a mutation of the Claude
// metadata half's output for the rest of the test and removes it on cleanup.
// Only BuildGenerationMetadata applies it. A test that sets it must not run
// in parallel with another test that builds a metadata candidate.
func SetClaudeMetadataHalfMutationForTest(t testing.TB, mutate func(GenerationBuildInput, *UnifiedMetadata)) {
	t.Helper()
	previous := claudeMetadataHalfMutationForTest
	claudeMetadataHalfMutationForTest = mutate
	t.Cleanup(func() { claudeMetadataHalfMutationForTest = previous })
}

// WithParseInputContextFieldsForTest replaces the per-harness list of
// captured metadata fields the parsed rows depend on.
// This function is only available in test builds.
func WithParseInputContextFieldsForTest(fields func(Harness) []string) PipelineOption {
	return func(p *Pipeline) { p.parseInputContextFieldsOverride = fields }
}
