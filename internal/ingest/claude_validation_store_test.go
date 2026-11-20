package ingest_test

import (
	"context"
	"encoding/json"
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

// halfValidatingStore observes the real store under a refresh run. The
// pipeline builds the metadata candidate once and asks the store's validator;
// this wrapper only records each answer and settle, checks that the pipeline
// supplied the candidate and carried a proven answer into the settle, and
// offers two seams: forceValidation turns a proven cheap proof into a
// metadata-context mismatch so the validator answers for every session, and
// afterValidation runs between the validator's answer and the settle.
type halfValidatingStore struct {
	*store.Store
	forceValidation bool
	afterValidation func(sid ingest.SessionID)

	mu          sync.Mutex
	outcomes    map[ingest.SessionID]ingest.ManagedGenerationValidationResult
	cheapProven map[ingest.SessionID]bool
	proven      map[ingest.SessionID]bool
	settled     map[ingest.SessionID]error
	// wiring records a pipeline that asked without a candidate or settled a
	// proven answer without carrying it.
	wiring []string
}

var _ ingest.ManagedGenerationValidator = (*halfValidatingStore)(nil)

func newHalfValidatingStore(db *store.Store) *halfValidatingStore {
	return &halfValidatingStore{
		Store:       db,
		outcomes:    map[ingest.SessionID]ingest.ManagedGenerationValidationResult{},
		cheapProven: map[ingest.SessionID]bool{},
		proven:      map[ingest.SessionID]bool{},
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
	result, err := s.Store.ValidateManagedGenerationUnchanged(ctx, request)
	s.mu.Lock()
	if request.Candidate == nil {
		s.wiring = append(s.wiring, "the pipeline asked the validator for "+string(request.SessionID)+" without a candidate")
	}
	if err == nil {
		s.outcomes[request.SessionID] = result
		s.proven[request.SessionID] = result.Outcome == ingest.ValidationProven
	}
	s.mu.Unlock()
	if s.afterValidation != nil {
		s.afterValidation(request.SessionID)
	}
	return result, err
}

func (s *halfValidatingStore) SettleCapturedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationUnchangedProof) (ingest.ManagedGenerationUnchangedSettleOutcome, error) {
	s.mu.Lock()
	if s.proven[request.SessionID] && (request.Validated == nil || request.Validated.Candidate.Metadata.SessionID != request.SessionID) {
		s.wiring = append(s.wiring, "the pipeline settled "+string(request.SessionID)+" without the validated answer")
	}
	s.mu.Unlock()
	outcome, err := s.Store.SettleCapturedGenerationUnchanged(ctx, request)
	s.mu.Lock()
	s.settled[request.SessionID] = err
	s.mu.Unlock()
	return outcome, err
}

func (s *halfValidatingStore) outcome(sid ingest.SessionID) (ingest.ManagedGenerationValidationResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, ok := s.outcomes[sid]
	return result, ok
}

// assertWiring fails the test when the pipeline did not hand the candidate
// or the validated answer through.
func (s *halfValidatingStore) assertWiring(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, problem := range s.wiring {
		t.Error(problem)
	}
}

// halfRecorder records, per session, the refusal of the metadata half the
// pipeline ran, so a test can tell "no candidate" from "never asked".
type halfRecorder struct {
	mu      sync.Mutex
	refused map[ingest.SessionID]error
	built   map[ingest.SessionID]int
}

func newHalfRecorder() *halfRecorder {
	return &halfRecorder{refused: map[ingest.SessionID]error{}, built: map[ingest.SessionID]int{}}
}

func (r *halfRecorder) record(sid ingest.SessionID, err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.built[sid]++
	if err != nil {
		r.refused[sid] = err
	}
}

func (r *halfRecorder) refusal(sid ingest.SessionID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refused[sid]
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
