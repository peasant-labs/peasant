package ingest_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// halfValidatingStore wraps a real store so a refresh run asks the real
// validator with the real Claude metadata half: when the pipeline's request
// carries no candidate, it builds one from the request's captured metadata and
// the managed transcript bytes, exactly as the builder's input would carry
// them, and remembers a proven answer so the settle carries it. A pipeline
// that already supplies the candidate and the validated answer passes through
// unchanged.
type halfValidatingStore struct {
	*store.Store
	indexer *ingest.ClaudeIndexer
	output  string
	// forceValidation turns a proven cheap proof into a metadata-context
	// mismatch, so the validator answers for every session.
	forceValidation bool
	// afterValidation runs between the validator's answer and the return to
	// the pipeline: the point before the settle where a filesystem race can
	// happen.
	afterValidation func(sid ingest.SessionID)

	mu          sync.Mutex
	outcomes    map[ingest.SessionID]ingest.ManagedGenerationValidationResult
	halfErrors  map[ingest.SessionID]error
	cheapProven map[ingest.SessionID]bool
	validated   map[ingest.SessionID]*ingest.ValidatedContext
	settled     map[ingest.SessionID]error
}

var _ ingest.ManagedGenerationValidator = (*halfValidatingStore)(nil)

func newHalfValidatingStore(db *store.Store, output string, fs ingest.FileSystem) *halfValidatingStore {
	return &halfValidatingStore{
		Store:       db,
		indexer:     ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true)),
		output:      output,
		outcomes:    map[ingest.SessionID]ingest.ManagedGenerationValidationResult{},
		halfErrors:  map[ingest.SessionID]error{},
		cheapProven: map[ingest.SessionID]bool{},
		validated:   map[ingest.SessionID]*ingest.ValidatedContext{},
		settled:     map[ingest.SessionID]error{},
	}
}

func (s *halfValidatingStore) ProveCapturedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationUnchangedProof) (ingest.UnchangedProof, error) {
	proof, err := s.Store.ProveCapturedGenerationUnchanged(ctx, request)
	s.mu.Lock()
	s.cheapProven[request.SessionID] = err == nil && proof.Proven
	s.mu.Unlock()
	if err == nil && proof.Proven && s.forceValidation {
		return ingest.UnchangedProof{Mismatch: ingest.UnchangedNotProvenMetadata}, nil
	}
	return proof, err
}

func (s *halfValidatingStore) ValidateManagedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationValidationRequest) (ingest.ManagedGenerationValidationResult, error) {
	if request.Candidate == nil {
		candidate, err := s.buildCandidate(ctx, request)
		if err != nil {
			s.mu.Lock()
			s.halfErrors[request.SessionID] = err
			s.mu.Unlock()
		} else {
			request.Candidate = &candidate
		}
	}
	result, err := s.Store.ValidateManagedGenerationUnchanged(ctx, request)
	s.mu.Lock()
	if err == nil {
		s.outcomes[request.SessionID] = result
	}
	if err == nil && result.Outcome == ingest.ValidationProven {
		s.validated[request.SessionID] = &ingest.ValidatedContext{GenerationID: request.GenerationID, Candidate: *request.Candidate}
	}
	s.mu.Unlock()
	if s.afterValidation != nil {
		s.afterValidation(request.SessionID)
	}
	return result, err
}

func (s *halfValidatingStore) SettleCapturedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationUnchangedProof) (ingest.ManagedGenerationUnchangedSettleOutcome, error) {
	s.mu.Lock()
	if request.Validated == nil {
		request.Validated = s.validated[request.SessionID]
	}
	s.mu.Unlock()
	outcome, err := s.Store.SettleCapturedGenerationUnchanged(ctx, request)
	s.mu.Lock()
	s.settled[request.SessionID] = err
	s.mu.Unlock()
	return outcome, err
}

// buildCandidate runs the production metadata half over the same inputs the
// builder receives: the captured metadata and the managed transcript bytes.
func (s *halfValidatingStore) buildCandidate(ctx context.Context, request ingest.ManagedGenerationValidationRequest) (ingest.GenerationMetadataCandidate, error) {
	transcript, err := managedTranscriptBytes(s.output, request.SessionID)
	if err != nil {
		return ingest.GenerationMetadataCandidate{}, err
	}
	recorded := request.Metadata
	return s.indexer.BuildGenerationMetadata(ctx, ingest.GenerationBuildInput{
		Session: ingest.DiscoveredSession{
			SessionID: request.SessionID, Harness: ingest.HarnessClaudeCode,
			SourceFormat: ingest.SourceFormatJSONL, SourcePath: ingest.ResolvedPath(recorded.Source.FilePath),
		},
		GenerationID:       "candidate-only",
		RecordedMetadata:   &recorded,
		CapturedTranscript: transcript,
	})
}

func (s *halfValidatingStore) outcome(sid ingest.SessionID) (ingest.ManagedGenerationValidationResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, ok := s.outcomes[sid]
	return result, ok
}

// managedTranscriptBytes reads the managed transcript of one session from the
// pipeline's output tree.
func managedTranscriptBytes(output string, sid ingest.SessionID) ([]byte, error) {
	name := string(sid) + "--transcript" + defaults.ExtJSONL.String()
	var found string
	err := filepath.WalkDir(output, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == name {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if found == "" {
		return nil, errors.New("no managed transcript for " + string(sid))
	}
	return os.ReadFile(found)
}

// rewriteManagedMetadata applies change to every managed metadata sidecar of
// the sessions named (all sessions when sids is empty), re-seals its
// metadata hash, and returns how many it rewrote.
func rewriteManagedMetadata(t *testing.T, output string, change func(sid string, metadata *schema.UnifiedMetadata), sids ...string) int {
	t.Helper()
	rewritten := 0
	err := filepath.WalkDir(output, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), defaults.MetadataSuffix) {
			return nil
		}
		sid := strings.TrimSuffix(entry.Name(), defaults.MetadataSuffix)
		if len(sids) != 0 && !contains(sids, sid) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var metadata schema.UnifiedMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			return err
		}
		change(sid, &metadata)
		metadata.MetadataHash = schema.ComputeMetadataHash(&metadata)
		encoded, err := json.Marshal(&metadata)
		if err != nil {
			return err
		}
		rewritten++
		return os.WriteFile(path, encoded, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return rewritten
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
