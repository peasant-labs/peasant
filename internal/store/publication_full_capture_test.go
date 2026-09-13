package store_test

import (
	"context"
	_ "embed"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
)

//go:embed testdata/publication_full_capture.yaml
var publicationFullCaptureYAML []byte

type publicationFullCaptureCase struct {
	Name             string                      `yaml:"name"`
	SQL              string                      `yaml:"sql"`
	Readiness        ingest.PublicationReadiness `yaml:"readiness"`
	IgnoreChecks     bool                        `yaml:"ignore_checks"`
	ProofError       bool                        `yaml:"proof_error"`
	BodyError        bool                        `yaml:"body_error"`
	Backfill         bool                        `yaml:"backfill"`
	ChangeShape      bool                        `yaml:"change_shape"`
	WriteError       bool                        `yaml:"write_error"`
	DisagreeRevision bool                        `yaml:"disagree_revision"`
}

func loadPublicationFullCaptureFixtures(t *testing.T) []publicationFullCaptureCase {
	t.Helper()
	var f struct {
		Cases []publicationFullCaptureCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(publicationFullCaptureYAML, &f); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, c := range f.Cases {
		if seen[c.Name] {
			t.Fatalf("duplicate fixture %s", c.Name)
		}
		seen[c.Name] = true
	}
	for _, name := range strings.Fields("complete-coherent-pool-one missing-full-capture incomplete-full-capture failed-full-capture unbound-full-capture stale-full-capture stale-index unsupported-metadata malformed-full-hash nonhex-full-hash missing-complete-hash unknown-status later-corrupt-chunk retained-backfill-binds-current-proof retained-backfill-keeps-unpaired-index-unready retained-backfill-refuses-corrupt-proof retained-backfill-refuses-shape-change explicit-full-revision-disagrees") {
		if !seen[name] {
			t.Fatalf("required fixture %s missing", name)
		}
	}
	return f.Cases
}

func TestPublicationFullCaptureEligibilityAndBundle(t *testing.T) {
	for _, tc := range loadPublicationFullCaptureFixtures(t) {
		t.Run(tc.Name, func(t *testing.T) {
			s, err := store.Open(storetest.CopyGoldenDB(t), store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			e := publicationEntry(t, "11111111-1111-4111-8111-111111111111")
			id := e.Metadata.SessionID
			revision := capturePublication(t, s, e)
			entries := contentEntries(id, contentCase{Prefix: "synthetic prose ", Repeats: 10000, Tail: "SAFE FULL TAIL", Entries: 2})
			if r := indexPublication(t, s, e, revision, entries); r.Err != nil {
				t.Fatal(r.Err)
			}
			if tc.IgnoreChecks {
				execContentSQL(t, s, "PRAGMA ignore_check_constraints=ON;")
			}
			if tc.SQL != "" {
				execContentSQL(t, s, tc.SQL)
			}
			if tc.IgnoreChecks {
				execContentSQL(t, s, "PRAGMA ignore_check_constraints=OFF;")
			}
			if tc.DisagreeRevision {
				before := capture(t, s, id)
				r := s.IndexSessionEntryBatch(context.Background(), []ingest.SessionEntryWrite{{SessionID: id, Result: indexformat.V1{Entries: entries}, IndexVersion: 1, RequireFullContent: true, CaptureRevision: revision, ContentCapture: ingest.SessionContentCaptureWrite{PublicationCaptureRevision: revision + 1}}})[0]
				if r.Err == nil || r.Written || capture(t, s, id) != before {
					t.Fatal("disagreeing full revision changed capture")
				}
			}
			if tc.Backfill {
				beforeHash := sessionEntriesHash(t, s, id)
				before, _, readErr := s.GetSessionContentCapture(context.Background(), id)
				if readErr != nil {
					t.Fatal(readErr)
				}
				attempt := append(entries[:0:0], entries...)
				if tc.ChangeShape {
					attempt[0].Extra = strPtr(`{"model_id":"different"}`)
				}
				r := s.IndexSessionEntryBatch(context.Background(), []ingest.SessionEntryWrite{{SessionID: id, Result: indexformat.V1{Entries: attempt}, IndexVersion: 1, RequireFullContent: true, Mode: ingest.SessionEntryWriteContentBackfill}})[0]
				if (r.Err != nil) != tc.WriteError {
					t.Fatalf("backfill error %v", r.Err)
				}
				if sessionEntriesHash(t, s, id) != beforeHash {
					t.Fatal("backfill changed bounded index hash")
				}
				if tc.WriteError && capture(t, s, id) != before {
					t.Fatal("failed backfill changed capture")
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn := takeConn(t, s.PoolForTest())
			payloadReads := 0
			if err := conn.SetAuthorizer(sqlite.AuthorizeFunc(func(a sqlite.Action) sqlite.AuthResult {
				if a.Table() == "session_entries" || a.Table() == "session_entries_ext" || strings.HasPrefix(a.Table(), "session_entry_full_content") {
					payloadReads++
					return sqlite.AuthResultDeny
				}
				return sqlite.AuthResultOK
			})); err != nil {
				t.Fatal(err)
			}
			s.PoolForTest().Put(conn)
			projection, err := s.LoadPublicationMetadata(ctx, []ingest.SessionID{id})
			if err != nil {
				t.Fatal(err)
			}
			proof := projection[id]
			if (proof.Error != nil) != tc.ProofError {
				t.Fatalf("proof error %v", proof.Error)
			}
			if !tc.ProofError && proof.Readiness != tc.Readiness {
				t.Fatalf("readiness %s, want %s", proof.Readiness, tc.Readiness)
			}
			locations, err := s.BulkLookupSessionLocations(ctx, []ingest.SessionID{id})
			if err != nil {
				t.Fatal(err)
			}
			want := tc.Readiness
			if tc.ProofError {
				want = ingest.PublicationNeedsIngest
			}
			if locations[id].PublicationReadiness != want {
				t.Fatal("bulk eligibility disagrees")
			}
			if payloadReads != 0 {
				t.Fatal("cheap readers touched payload")
			}
			denied, deniedErr := s.LoadPublicationInput(ctx, id)
			if want != ingest.PublicationReady {
				if payloadReads != 0 || len(denied.Entries) != 0 || denied.Readiness != ingest.PublicationNeedsIngest {
					t.Fatal("unready bundle hydrated payload")
				}
				if (deniedErr != nil) != tc.ProofError {
					t.Fatalf("unready error %v", deniedErr)
				}
			} else if deniedErr == nil || payloadReads == 0 {
				t.Fatal("negative control failed: eligible bundle must read payload")
			}
			conn = takeConn(t, s.PoolForTest())
			if err := conn.SetAuthorizer(nil); err != nil {
				t.Fatal(err)
			}
			s.PoolForTest().Put(conn)
			bundle, err := s.LoadPublicationInput(ctx, id)
			if (err != nil) != (tc.ProofError || tc.BodyError) {
				t.Fatalf("bundle error %v", err)
			}
			if err != nil {
				if !strings.Contains(err.Error(), "repair") && !strings.Contains(err.Error(), "ingest") {
					t.Fatalf("non-actionable error %v", err)
				}
				if bundle.Readiness != ingest.PublicationNeedsIngest {
					t.Fatal("corruption left outgoing bundle ready")
				}
			} else if want == ingest.PublicationReady {
				if !reflect.DeepEqual(bundle.Entries, entries) {
					t.Fatal("full prose or tool fields did not survive publication read")
				}
				if bundle.ContentCapture.PublicationCaptureRevision != revision || bundle.CaptureRevision != revision || bundle.ContentCapture.SessionID != id {
					t.Fatal("bundle mixed publication revisions")
				}
			}
			if _, err := s.LoadPublicationMetadata(ctx, []ingest.SessionID{id}); err != nil {
				t.Fatalf("pool-one connection was not released: %v", err)
			}
		})
	}
}
