package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/schema"
)

type stubOpenCodeSourceAccess struct {
	calls atomic.Int64
	fn    func(ctx context.Context, locator string) (OpenCodeHistorySnapshot, error)
}

func (s *stubOpenCodeSourceAccess) Snapshot(ctx context.Context, locator string) (OpenCodeHistorySnapshot, error) {
	s.calls.Add(1)
	return s.fn(ctx, locator)
}

func TestMemoizedOpenCodeSnapshotAccess(t *testing.T) {
	t.Run("caches-same-locator", func(t *testing.T) {
		base := &stubOpenCodeSourceAccess{
			fn: func(_ context.Context, locator string) (OpenCodeHistorySnapshot, error) {
				return OpenCodeHistorySnapshot{SessionID: "ses_1", SourceEvidenceDigest: "digest-" + locator}, nil
			},
		}
		memo := &memoizedOpenCodeSnapshotAccess{base: base}

		first, err := memo.Snapshot(context.Background(), "/db/one.sqlite")
		if err != nil {
			t.Fatalf("first Snapshot: %v", err)
		}
		second, err := memo.Snapshot(context.Background(), "/db/one.sqlite")
		if err != nil {
			t.Fatalf("second Snapshot: %v", err)
		}
		if base.calls.Load() != 1 {
			t.Fatalf("base.Snapshot calls = %d, want 1", base.calls.Load())
		}
		if first.SourceEvidenceDigest != second.SourceEvidenceDigest {
			t.Fatalf("cached digest = %q, want %q", second.SourceEvidenceDigest, first.SourceEvidenceDigest)
		}
	})

	t.Run("locator-mismatch-reloads", func(t *testing.T) {
		base := &stubOpenCodeSourceAccess{
			fn: func(_ context.Context, locator string) (OpenCodeHistorySnapshot, error) {
				return OpenCodeHistorySnapshot{SessionID: "ses_1", SourceEvidenceDigest: "digest-" + locator}, nil
			},
		}
		memo := &memoizedOpenCodeSnapshotAccess{base: base}

		first, err := memo.Snapshot(context.Background(), "/db/one.sqlite")
		if err != nil {
			t.Fatalf("first Snapshot: %v", err)
		}
		second, err := memo.Snapshot(context.Background(), "/db/two.sqlite")
		if err != nil {
			t.Fatalf("second Snapshot: %v", err)
		}
		if base.calls.Load() != 2 {
			t.Fatalf("base.Snapshot calls = %d, want 2", base.calls.Load())
		}
		if first.SourceEvidenceDigest == second.SourceEvidenceDigest {
			t.Fatalf("expected different digest after locator mismatch, got %q", second.SourceEvidenceDigest)
		}
	})

	t.Run("ctx-cancelled-never-returns-cached", func(t *testing.T) {
		base := &stubOpenCodeSourceAccess{
			fn: func(_ context.Context, locator string) (OpenCodeHistorySnapshot, error) {
				return OpenCodeHistorySnapshot{SessionID: "ses_1", SourceEvidenceDigest: "digest-" + locator}, nil
			},
		}
		memo := &memoizedOpenCodeSnapshotAccess{base: base}

		if _, err := memo.Snapshot(context.Background(), "/db/one.sqlite"); err != nil {
			t.Fatalf("first Snapshot: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		snap, err := memo.Snapshot(ctx, "/db/one.sqlite")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Snapshot err = %v, want context.Canceled", err)
		}
		if snap.SessionID != "" || snap.SourceEvidenceDigest != "" {
			t.Fatalf("cancelled Snapshot returned non-zero snapshot: %+v", snap)
		}
		if base.calls.Load() != 1 {
			t.Fatalf("base.Snapshot calls = %d, want 1", base.calls.Load())
		}
	})

	t.Run("error-never-cached", func(t *testing.T) {
		sentinel := errors.New("db locked")
		var attempt atomic.Int64
		base := &stubOpenCodeSourceAccess{
			fn: func(_ context.Context, locator string) (OpenCodeHistorySnapshot, error) {
				if attempt.Add(1) == 1 {
					return OpenCodeHistorySnapshot{}, sentinel
				}
				return OpenCodeHistorySnapshot{SessionID: "ses_1", SourceEvidenceDigest: "digest-" + locator}, nil
			},
		}
		memo := &memoizedOpenCodeSnapshotAccess{base: base}

		if _, err := memo.Snapshot(context.Background(), "/db/one.sqlite"); !errors.Is(err, sentinel) {
			t.Fatalf("first Snapshot err = %v, want %v", err, sentinel)
		}
		if memo.loaded {
			t.Fatalf("memo.loaded = true after error, want false")
		}
		snap, err := memo.Snapshot(context.Background(), "/db/one.sqlite")
		if err != nil {
			t.Fatalf("second Snapshot: %v", err)
		}
		if base.calls.Load() != 2 {
			t.Fatalf("base.Snapshot calls = %d, want 2", base.calls.Load())
		}
		if snap.SourceEvidenceDigest != "digest-/db/one.sqlite" {
			t.Fatalf("second Snapshot digest = %q, want %q", snap.SourceEvidenceDigest, "digest-/db/one.sqlite")
		}
	})
}

type memoCleanupStubAdapter struct {
	SourceAdapter
	calls atomic.Int64
}

func (a *memoCleanupStubAdapter) SnapshotOpenCodeProvenance(_ context.Context, dbPath string, sessionID string, _ OpenCodeSnapshotOptions) (OpenCodeHistorySnapshot, error) {
	a.calls.Add(1)
	return OpenCodeHistorySnapshot{SessionID: sessionID, SourceEvidenceDigest: "digest-" + dbPath}, nil
}

type memoCleanupStubIndexer struct {
	TranscriptIndexer
	buildErr error
}

func (i *memoCleanupStubIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }

func (i *memoCleanupStubIndexer) BuildGenerationMetadata(ctx context.Context, in GenerationBuildInput) (GenerationMetadataCandidate, error) {
	snap, err := in.OpenCode.Snapshot(ctx, "/tmp/opencode.sqlite")
	if err != nil {
		return GenerationMetadataCandidate{}, err
	}
	return GenerationMetadataCandidate{
		Metadata:             *in.RecordedMetadata,
		Completeness:         indexformat.GenerationCompletenessComplete,
		SourceEvidenceDigest: snap.SourceEvidenceDigest,
	}, nil
}

func (i *memoCleanupStubIndexer) BuildGeneration(ctx context.Context, in GenerationBuildInput) (GenerationCandidate, error) {
	if _, err := in.OpenCode.Snapshot(ctx, "/tmp/opencode.sqlite"); err != nil {
		return GenerationCandidate{}, err
	}
	return GenerationCandidate{}, i.buildErr
}

type memoCleanupStubPlan struct {
	sessionID SessionID
}

func (p memoCleanupStubPlan) SessionID() SessionID       { return p.sessionID }
func (p memoCleanupStubPlan) SourceGenerationID() string { return "gen-old" }
func (p memoCleanupStubPlan) OwnerGenerationID() string  { return "gen-old" }

type memoCleanupStubStore struct {
	MetricsStore
	state          *SessionIndexState
	proof          UnchangedProof
	validateResult ManagedGenerationValidationResult
	settleErr      error
}

func (s *memoCleanupStubStore) SupportsIndexFormat(v int) bool { return v == 2 }
func (s *memoCleanupStubStore) ReadIndexState(context.Context, SessionID) (*SessionIndexState, error) {
	return s.state, nil
}
func (s *memoCleanupStubStore) ProveCapturedGenerationUnchanged(context.Context, ManagedGenerationUnchangedProof) (UnchangedProof, error) {
	return s.proof, nil
}
func (s *memoCleanupStubStore) ValidateManagedGenerationUnchanged(context.Context, ManagedGenerationValidationRequest) (ManagedGenerationValidationResult, error) {
	return s.validateResult, nil
}
func (s *memoCleanupStubStore) SettleCapturedGenerationUnchanged(_ context.Context, req ManagedGenerationUnchangedProof) (ManagedGenerationUnchangedSettleOutcome, error) {
	if s.settleErr != nil {
		return ManagedGenerationUnchangedSettleOutcome{}, s.settleErr
	}
	return ManagedGenerationUnchangedSettleOutcome{GenerationID: req.Expected.ActiveGenerationID}, nil
}
func (s *memoCleanupStubStore) PlanManagedGenerationReference(_ context.Context, _ ManagedGenerationReferenceRequest) (ManagedGenerationReferencePlan, error) {
	return memoCleanupStubPlan{sessionID: s.state.SessionID}, nil
}
func (s *memoCleanupStubStore) InstallManagedGenerationReference(context.Context, ManagedGenerationReferencePlan) (ManagedGenerationReferenceOutcome, error) {
	return ManagedGenerationReferenceOutcome{GenerationID: "gen-ref", OwnerGenerationID: "gen-old"}, nil
}

func TestOpenCodeSnapshotMemoClearedOnAllParseIndexMetaExits(t *testing.T) {
	sid := SessionID("ses_opencodememocleanup1")
	transcriptBytes, err := json.Marshal(openCodeCurrentProjection{
		Format:    openCodeCurrentProjectionFormat,
		Version:   openCodeCurrentProjectionVersion,
		SessionID: string(sid),
		Messages: []openCodeLegacyProjectionMessage{
			{
				ID:        "m1",
				SessionID: string(sid),
				Data:      json.RawMessage(`{"id":"m1","role":"user"}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal projection: %v", err)
	}
	contentHash := schema.ComputeTranscriptHash(transcriptBytes)
	meta := UnifiedMetadata{
		SchemaVersion: CurrentSchemaVersion,
		SessionID:     sid,
		ModelHarness:  HarnessOpenCode,
		HostSlug:      "host-a",
		ContentHash:   contentHash,
		Source:        SourceInfo{Format: SourceFormatJSON, FilePath: "/tmp/opencode.sqlite"},
	}
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	artifact, err := newIngestArtifact(&meta, metaJSON, transcriptBytes)
	if err != nil {
		t.Fatalf("newIngestArtifact: %v", err)
	}
	indexVersion := 2
	state := &SessionIndexState{
		SessionID:          sid,
		Harness:            HarnessOpenCode,
		IndexerVersion:     2,
		IndexVersion:       &indexVersion,
		ArtifactHash:       &artifact.ArtifactHash,
		ActiveGenerationID: "gen-old",
	}

	cases := []struct {
		name           string
		proof          UnchangedProof
		validateResult ManagedGenerationValidationResult
		settleErr      error
		buildErr       error
		wantSnapshot   int64
	}{
		{
			name:           "reference-install",
			proof:          UnchangedProof{Mismatch: UnchangedNotProvenMetadata},
			validateResult: ManagedGenerationValidationResult{Outcome: ValidationNotProven, Mismatch: UnchangedNotProvenMetadata},
			wantSnapshot:   1,
		},
		{
			name:           "validated-settle",
			proof:          UnchangedProof{Proven: true},
			validateResult: ManagedGenerationValidationResult{Outcome: ValidationProven},
			wantSnapshot:   1,
		},
		{
			name:           "fallback-parse-error",
			proof:          UnchangedProof{Proven: true},
			validateResult: ManagedGenerationValidationResult{Outcome: ValidationProven},
			settleErr:      &StaleIndexWorkError{SessionID: sid},
			buildErr:       errors.New("synthetic build failure"),
			wantSnapshot:   1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &memoCleanupStubAdapter{}
			indexer := &memoCleanupStubIndexer{
				TranscriptIndexer: NewOpenCodeIndexer(&OSFileSystem{}),
				buildErr:          tc.buildErr,
			}
			st := &memoCleanupStubStore{
				state:          state,
				proof:          tc.proof,
				validateResult: tc.validateResult,
				settleErr:      tc.settleErr,
			}
			p := &Pipeline{
				adapters: map[Harness]AdapterFactory{
					HarnessOpenCode: func(FileSystem, GitResolver, salt.Salt) SourceAdapter { return adapter },
				},
				indexers: map[Harness]TranscriptIndexer{
					HarnessOpenCode: indexer,
				},
				metricsStore: st,
				resolvedVersions: map[Harness]HarvesterVersions{
					HarnessOpenCode: {AdapterVersion: 5, IndexerVersion: 2, IndexVersion: 2},
				},
			}

			im := indexedMeta{
				session: DiscoveredSession{
					SessionID:        sid,
					Harness:          HarnessOpenCode,
					TranscriptOrigin: TranscriptOriginOpenCodeCurrentSQLite,
					OriginalRoot:     "/tmp/opencode.sqlite",
				},
				outputTranscriptPath:     "/tmp/out/transcript.json",
				metadataData:             metaJSON,
				transcriptData:           transcriptBytes,
				managedGenerationRefresh: true,
			}
			var activeParses, maxActiveParses atomic.Int64
			res := p.parseIndexMeta(context.Background(), im, &activeParses, &maxActiveParses, "memo-cleanup-test", nil)
			if res.input == nil {
				t.Fatalf("expected non-nil res.input")
			}
			if res.input.openCodeSnapshot != nil {
				t.Fatalf("res.input.openCodeSnapshot != nil after %s exit", tc.name)
			}
			if adapter.calls.Load() != tc.wantSnapshot {
				reason := ""
				if res.logEntry.Reason != nil {
					reason = *res.logEntry.Reason
				}
				t.Fatalf("SnapshotOpenCodeProvenance calls = %d, want %d (outcome=%s reason=%q err=%v)", adapter.calls.Load(), tc.wantSnapshot, res.logEntry.Outcome, reason, res.logEntry.ErrorMessage)
			}
		})
	}
}
