package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/pull-association-actions.yaml
var pullAssociationActionsYAML []byte

type pullAssociationAnnotationFixture struct {
	TranscriptID   schema.TranscriptID `yaml:"transcriptId"`
	Hash           string              `yaml:"hash"`
	HashInputJSON  string              `yaml:"hashInputJSON"`
	PayloadJSON    string              `yaml:"payloadJSON"`
	AuthorUserID   string              `yaml:"authorUserId"`
	AuthorUsername string              `yaml:"authorUsername"`
	LocalSessionID string              `yaml:"localSessionId"`
	RetractedAt    *int64              `yaml:"retractedAt"`
}

// Read only this fixture's disposable SQLite file. No extra production getter
// is needed merely to assert the complete persisted payload and provenance.
func readPullAssociationAnnotationRows(t *testing.T, path string) map[string]store.PulledAnnotationRow {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rows := map[string]store.PulledAnnotationRow{}
	err = sqlitex.Execute(conn, `SELECT village_host,content_hash,transcript_id,COALESCE(local_session_id,''),author_user_id,author_username,payload,pulled_at,retracted_at FROM pulled_annotations ORDER BY village_host,content_hash`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		id, err := schema.NewTranscriptID(stmt.ColumnText(2))
		if err != nil {
			return err
		}
		row := store.PulledAnnotationRow{VillageHost: stmt.ColumnText(0), ContentHash: stmt.ColumnText(1), TranscriptID: id, LocalSessionID: stmt.ColumnText(3), AuthorUserID: stmt.ColumnText(4), AuthorUsername: stmt.ColumnText(5), Payload: stmt.ColumnText(6), PulledAt: stmt.ColumnInt64(7)}
		if stmt.ColumnType(8) != sqlite.TypeNull {
			at := stmt.ColumnInt64(8)
			row.RetractedAt = &at
		}
		key := row.VillageHost + "|" + row.ContentHash
		if _, duplicate := rows[key]; duplicate {
			t.Fatalf("duplicate annotation projection key %s", key)
		}
		rows[key] = row
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestPullAssociationActionFixtures(t *testing.T) {
	var corpus struct {
		ObservedAt time.Time `yaml:"observedAt"`
		Content    []struct {
			ID               schema.TranscriptID `yaml:"id"`
			Blob             string              `yaml:"blob"`
			OwnerUserID      string              `yaml:"ownerUserId"`
			OwnerUsername    string              `yaml:"ownerUsername"`
			Title            string              `yaml:"title"`
			Harness          schema.Harness      `yaml:"harness"`
			Visibility       schema.Visibility   `yaml:"visibility"`
			License          schema.License      `yaml:"license"`
			LocalSessionID   string              `yaml:"localSessionId"`
			ProjectName      string              `yaml:"projectName"`
			AnnotationHashes []string            `yaml:"annotationHashes"`
		} `yaml:"content"`
		ExpectedInstallations map[string][]schema.TranscriptID              `yaml:"expectedInstallations"`
		AnnotationSets        map[string][]pullAssociationAnnotationFixture `yaml:"annotationSets"`
		RequiredNames         []string                                      `yaml:"requiredNames"`
		Cases                 []struct {
			Name              string                              `yaml:"name"`
			Mode              string                              `yaml:"mode"`
			Bulk              bool                                `yaml:"bulk"`
			Preinstall        bool                                `yaml:"preinstall"`
			Prior             bool                                `yaml:"prior"`
			DryRun            bool                                `yaml:"dryRun"`
			MetadataFailure   string                              `yaml:"metadataFailure"`
			WantState         schema.PullAssociationMetadataState `yaml:"wantState"`
			WantStatus        string                              `yaml:"wantStatus"`
			WantMetadataCalls int32                               `yaml:"wantMetadataCalls"`
			WantWarning       bool                                `yaml:"wantWarning"`
			WantError         bool                                `yaml:"wantError"`
			Installation      string                              `yaml:"installation"`
			AnnotationSet     string                              `yaml:"annotationSet"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(pullAssociationActionsYAML, &corpus); err != nil {
		t.Fatal(err)
	}
	fixtureIDs := map[schema.TranscriptID]bool{}
	for _, content := range corpus.Content {
		if fixtureIDs[content.ID] || content.Blob == "" {
			t.Fatal("duplicate content fixture identity or missing exact bytes")
		}
		fixtureIDs[content.ID] = true
	}
	for _, ids := range corpus.ExpectedInstallations {
		for _, id := range ids {
			if !fixtureIDs[id] {
				t.Fatalf("expected installation %s lacks exact content fixture", id)
			}
		}
	}
	for _, set := range corpus.AnnotationSets {
		seen := map[string]bool{}
		for _, fixture := range set {
			if !fixtureIDs[fixture.TranscriptID] || seen[fixture.Hash] {
				t.Fatal("unknown annotation transcript or duplicate annotation fixture hash")
			}
			seen[fixture.Hash] = true
			var input schema.AnnotationPushItem
			if err := json.Unmarshal([]byte(fixture.HashInputJSON), &input); err != nil {
				t.Fatal(err)
			}
			if err := input.Validate(); err != nil {
				t.Fatal(err)
			}
			if input.ComputeContentHash() != fixture.Hash {
				t.Fatalf("published annotation fixture hash differs from declared checksum: %s", fixture.Hash)
			}
			var annotation schema.PullAnnotation
			if err := json.Unmarshal([]byte(fixture.PayloadJSON), &annotation); err != nil {
				t.Fatal(err)
			}
			if err := annotation.AnnotationSummary.Validate(); err != nil {
				t.Fatal(err)
			}
			canonical, err := json.Marshal(annotation)
			if err != nil {
				t.Fatal(err)
			}
			if string(canonical) != fixture.PayloadJSON {
				t.Fatal("expected annotation payload is not pinned canonical shared JSON")
			}
			if annotation.ContentHash == nil || *annotation.ContentHash != fixture.Hash {
				t.Fatal("annotation fixture payload differs from declared checksum")
			}
			published := schema.AnnotationPushItem{TargetKind: annotation.TargetKind, SessionID: annotation.TargetSessionID, TypeID: annotation.TypeID, Value: annotation.Value, IsPrimary: annotation.IsPrimary, Confidence: annotation.Confidence, Reason: annotation.Reason, AnnotatorName: annotation.AnnotatorName, Provenance: annotation.Provenance}
			if published.ComputeContentHash() != fixture.Hash {
				t.Fatal("served session annotation payload does not match its independently declared published checksum")
			}
			if annotation.AuthorUserID != fixture.AuthorUserID || annotation.AuthorUsername != fixture.AuthorUsername {
				t.Fatal("annotation fixture author differs from independent projection expectation")
			}
		}
	}
	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			ids := pullCollectiveTestIDs(2)
			remote := newPullCollectiveDouble(t)
			seedPullTranscripts(t, remote, ids, pullCollectiveAID)
			annotationPlan := corpus.AnnotationSets[c.AnnotationSet]
			if c.AnnotationSet != "" && annotationPlan == nil {
				t.Fatal("missing named annotation fixture set")
			}
			wireAnnotations := map[schema.TranscriptID][]schema.PullAnnotation{}
			for _, fixture := range annotationPlan {
				var annotation schema.PullAnnotation
				if err := json.Unmarshal([]byte(fixture.PayloadJSON), &annotation); err != nil {
					t.Fatal(err)
				}
				wireAnnotations[fixture.TranscriptID] = append(wireAnnotations[fixture.TranscriptID], annotation)
			}
			expectedHashes := func(id schema.TranscriptID, empty []string) []string {
				hashes := append([]string{}, empty...)
				for _, fixture := range annotationPlan {
					if fixture.TranscriptID == id {
						hashes = append(hashes, fixture.Hash)
					}
				}
				sort.Strings(hashes)
				return hashes
			}
			for _, content := range corpus.Content {
				remote.SetPullTranscriptOwnedBy(content.ID, []byte(content.Blob), wireAnnotations[content.ID], content.OwnerUserID, content.OwnerUsername, content.LocalSessionID)
			}
			upstream, err := url.Parse(remote.URL())
			if err != nil {
				t.Fatal(err)
			}
			proxy := httputil.NewSingleHostReverseProxy(upstream)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var metadataCalls, contentCalls atomic.Int32
			var active atomic.Bool
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				if r.Method != http.MethodGet && path != "/api/v1/pull/transcripts/skip-gate" {
					t.Errorf("unexpected remote mutation %s %s", r.Method, path)
				}
				if strings.Contains(path, "/events") || strings.HasSuffix(path, "/shares") {
					t.Errorf("foreign content probed owner-only history: %s", path)
				}
				if path == "/api/v1/auth/me" {
					_ = json.NewEncoder(w).Encode(schema.VillageUser{ID: schema.VillageUUID(stubOwnerUserID)})
					return
				}
				if strings.HasSuffix(path, "/collectives") && strings.HasPrefix(path, "/api/v1/transcripts/") {
					metadataCalls.Add(1)
					if active.Load() {
						switch c.MetadataFailure {
						case "cancel":
							cancel()
							<-r.Context().Done()
							return
						case "forbidden":
							http.Error(w, "disclosure refused", http.StatusForbidden)
							return
						case "missing":
							http.Error(w, "not disclosed", http.StatusNotFound)
							return
						case "remote":
							http.Error(w, "metadata unavailable", http.StatusServiceUnavailable)
							return
						}
					}
					_ = json.NewEncoder(w).Encode(schema.VillageTranscriptCollectivesResponse{Collectives: []schema.VillageTranscriptCollective{{ID: pullCollectiveAID, Name: "Renamed independently", SharedAt: corpus.ObservedAt}}})
					return
				}
				if strings.HasSuffix(path, "/content") {
					contentCalls.Add(1)
					if active.Load() {
						switch c.Mode {
						case "content-forbidden":
							http.Error(w, "content denied", http.StatusForbidden)
							return
						case "content-missing":
							http.Error(w, "content missing", http.StatusNotFound)
							return
						case "partial-content-error":
							if strings.Contains(path, ids[1].String()) {
								http.Error(w, "content denied", http.StatusForbidden)
								return
							}
						case "etag":
							w.WriteHeader(http.StatusNotModified)
							return
						}
					}
				}
				if active.Load() && c.Mode == "negotiation-error" && path == "/api/v1/schema/version" {
					http.Error(w, "required negotiation unavailable", 503)
					return
				}
				contentRefusal := c.Mode == "content-forbidden" || c.Mode == "content-missing"
				isMetadata := path == "/api/v1/pull/transcripts/"+ids[0].String()
				if active.Load() && contentRefusal && isMetadata {
					status := http.StatusNotFound
					if c.Mode == "content-forbidden" {
						status = http.StatusForbidden
					}
					http.Error(w, "required metadata denied", status)
					return
				}
				omitAdvertisedHash := c.Mode == "etag" || c.Mode == "download"
				if active.Load() && omitAdvertisedHash && isMetadata {
					content := corpus.Content[0]
					_ = json.NewEncoder(w).Encode(schema.PullTranscriptInfo{TranscriptID: ids[0], OwnerUserID: content.OwnerUserID, OwnerUsername: content.OwnerUsername, LocalID: content.LocalSessionID, Title: content.Title, Harness: content.Harness, Visibility: content.Visibility, AnnotationCount: len(wireAnnotations[content.ID])})
					return
				}
				if active.Load() && c.Mode == "switch" && strings.HasSuffix(path, "/annotations") {
					if err := auth.SaveCredentialsFrom(&auth.Credentials{UserID: "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb", Username: "other", APIKey: "other-key", KeyID: "other-id", VillageURL: server.URL}, dir); err != nil {
						t.Error(err)
					}
				}
				proxy.ServeHTTP(w, r)
			}))
			defer server.Close()
			if err := auth.SaveCredentialsFrom(&auth.Credentials{UserID: stubOwnerUserID, Username: "owneruser", APIKey: "fixture", KeyID: "fixture-id", VillageURL: server.URL}, dir); err != nil {
				t.Fatal(err)
			}
			if c.Preinstall {
				args := []string{"transcripts", "pull", ids[0].String(), "--json"}
				if c.Bulk {
					args = []string{"transcripts", "pull", "--collective", pullCollectiveAName, "--json"}
				}
				if _, stderr, err := runVillageSeparated(t, dir, args...); err != nil {
					t.Fatalf("seed real content: %v %s", err, stderr)
				}
			}
			original, err := auth.CaptureCredentials(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			if c.Prior {
				db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
				if err != nil {
					t.Fatal(err)
				}
				invocation, err := db.BeginCollectiveObservation(t.Context(), original)
				if err != nil {
					t.Fatal(err)
				}
				priorIDs := ids[:1]
				if c.Bulk {
					priorIDs = ids
				}
				for _, id := range priorIDs {
					if err := db.InstallTranscriptAssociations(t.Context(), invocation, id, []schema.VillageTranscriptCollective{{ID: pullCollectiveAID, Name: "Prior saved name", SharedAt: corpus.ObservedAt}}, corpus.ObservedAt, false); err != nil {
						t.Fatal(err)
					}
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			beforeBytes := map[schema.TranscriptID][]byte{}
			beforeManifest := map[schema.TranscriptID][]byte{}
			beforeRows := map[schema.TranscriptID]store.PulledTranscriptRow{}
			beforeAnnotations := map[string]store.PulledAnnotationRow{}
			if c.Preinstall {
				db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
				if err != nil {
					t.Fatal(err)
				}
				rows, err := db.ListPulledTranscripts(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					beforeRows[row.TranscriptID] = row
					data, err := os.ReadFile(filepath.Join(row.PullDir, pull.TranscriptFilename))
					if err != nil {
						t.Fatal(err)
					}
					beforeBytes[row.TranscriptID] = data
					data, err = os.ReadFile(filepath.Join(row.PullDir, pull.ManifestFilename))
					if err != nil {
						t.Fatal(err)
					}
					beforeManifest[row.TranscriptID] = data
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				beforeAnnotations = readPullAssociationAnnotationRows(t, string(defaults.ResolveDBFilePathWith(dir)))
			}
			metadataCalls.Store(0)
			contentCalls.Store(0)
			active.Store(true)
			root := newTestRoot()
			root.AddCommand(BuildVillageCommand())
			root.SetContext(ctx)
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			args := []string{"--data-dir", dir, "--config-dir", dir, "--state-dir", dir, "village", "transcripts", "pull"}
			if c.Bulk {
				args = append(args, "--collective", pullCollectiveAName)
			} else {
				args = append(args, ids[0].String())
			}
			args = append(args, "--json")
			if c.DryRun {
				args = append(args, "--dry-run")
			}
			root.SetArgs(args)
			err = root.Execute()
			if (err != nil) != c.WantError {
				t.Fatalf("required outcome changed: err=%v stderr=%s stdout=%s", err, stderr.String(), stdout.String())
			}
			if metadataCalls.Load() != c.WantMetadataCalls {
				t.Fatalf("dedicated metadata calls=%d want=%d stderr=%s", metadataCalls.Load(), c.WantMetadataCalls, stderr.String())
			}
			if c.Mode == "hash" && contentCalls.Load() != 0 {
				t.Fatal("current hash/skip gate downloaded content")
			}
			if strings.Contains(stdout.String(), "warning:") {
				t.Fatal("warning contaminated JSON stdout")
			}
			if strings.Contains(stderr.String(), "warning: transcript") != c.WantWarning {
				t.Fatalf("stderr warning mismatch: %s", stderr.String())
			}
			var results []jsonPullResult
			if c.Bulk {
				var aggregate jsonPullCollective
				if err := json.Unmarshal(stdout.Bytes(), &aggregate); err != nil {
					t.Fatal(err)
				}
				results = aggregate.Results
				if c.Mode == "partial-content-error" {
					if aggregate.Totals.Failed != 1 || aggregate.Totals.Pulled != 1 {
						t.Fatalf("bulk totals changed: %+v", aggregate.Totals)
					}
					if strings.Contains(stderr.String(), "warning: transcript "+ids[1].String()) {
						t.Fatal("warning attributed to failed content row")
					}
				} else if aggregate.Totals.Failed != 0 {
					t.Fatal("optional metadata counted as content failure")
				}
			} else {
				var result jsonPullResult
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				results = []jsonPullResult{result}
			}
			for _, result := range results {
				if result.Error != "" {
					if result.AssociationMetadata != nil {
						t.Fatal("required content failure observed optional evidence")
					}
					continue
				}
				if result.Status != c.WantStatus {
					t.Fatalf("content status=%s want=%s", result.Status, c.WantStatus)
				}
				if c.DryRun {
					if result.AssociationMetadata != nil {
						t.Fatal("dry run observed metadata")
					}
					continue
				}
				if result.AssociationMetadata == nil {
					t.Fatal("successful mounted content path omitted metadata")
				}
				metadata := result.AssociationMetadata
				if err := metadata.Validate(); err != nil {
					t.Fatal(err)
				}
				if metadata.State != c.WantState {
					t.Fatalf("metadata=%+v want=%s", metadata, c.WantState)
				}
				if c.WantState == schema.PullAssociationMetadataStale && !metadata.ObservedAt.Equal(corpus.ObservedAt) {
					t.Fatal("failure refreshed prior observation date")
				}
				if c.WantState == schema.PullAssociationMetadataUnknown && metadata.ObservedAt != nil {
					t.Fatal("unknown invented observation date")
				}
				if c.WantState == schema.PullAssociationMetadataInvalidated && (metadata.ObservedAt == nil || metadata.Warning != nil) {
					t.Fatal("known negative misclassified as transient failure")
				}
				if c.WantWarning && metadata.Warning == nil {
					t.Fatal("optional source failure lacks typed warning")
				}
			}
			// Content success is independently observable after the optional service
			// callback, not inferred from its report. Reopen SQLite and read actual
			// canonical artifact paths for exact fixture bytes and projections.
			verifyContent := func() {
				db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				rows, err := db.ListPulledTranscripts(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				profile := c.Installation
				if profile == "" {
					profile = "single"
				}
				wantIDs, ok := corpus.ExpectedInstallations[profile]
				if !ok {
					t.Fatal("unknown expected installation fixture profile")
				}
				wantSet := map[schema.TranscriptID]bool{}
				for _, id := range wantIDs {
					if wantSet[id] {
						t.Fatal("duplicate expected installed identity")
					}
					wantSet[id] = true
				}
				actualSet := map[schema.TranscriptID]bool{}
				for _, row := range rows {
					if actualSet[row.TranscriptID] || !wantSet[row.TranscriptID] {
						t.Fatalf("unexpected/duplicate installed content projection: %s", row.TranscriptID)
					}
					actualSet[row.TranscriptID] = true
					for _, content := range corpus.Content {
						if content.ID != row.TranscriptID {
							continue
						}
						path := pullDirFor(t, dir, server.URL, content.ID)
						data, err := os.ReadFile(filepath.Join(path, pull.TranscriptFilename))
						if err != nil {
							t.Fatalf("successful content became unreadable after optional metadata: %v", err)
						}
						if !bytes.Equal(data, []byte(content.Blob)) {
							t.Fatalf("installed content bytes changed after optional metadata for %s", content.ID)
						}
						hash := schema.ComputeTranscriptHash([]byte(content.Blob))
						if schema.ComputeTranscriptHash(data) != hash {
							t.Fatal("installed content hash differs from fixture")
						}
						wantHashes := expectedHashes(content.ID, content.AnnotationHashes)
						expected := store.PulledTranscriptRow{VillageHost: strings.TrimPrefix(server.URL, "http://"), TranscriptID: content.ID, OwnerUserID: content.OwnerUserID, OwnerUsername: content.OwnerUsername, LocalSessionID: content.LocalSessionID, Title: content.Title, Harness: content.Harness, ProjectName: content.ProjectName, ContentHash: hash, Visibility: content.Visibility, License: content.License, PullDir: path, AnnotationCount: len(wantHashes), FirstPulledAt: row.FirstPulledAt, LastPulledAt: row.LastPulledAt}
						if !reflect.DeepEqual(row, expected) {
							t.Fatalf("installed content projection differs after optional metadata: got=%+v want=%+v", row, expected)
						}
						if row.FirstPulledAt <= 0 || row.LastPulledAt < row.FirstPulledAt {
							t.Fatal("installed content projection has invalid pull dates")
						}
						manifestBytes, err := os.ReadFile(filepath.Join(path, pull.ManifestFilename))
						if err != nil {
							t.Fatal(err)
						}
						var manifest pull.PullManifest
						if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
							t.Fatal(err)
						}
						manifestPlan := []pull.ManifestAnnotation{}
						for _, fixture := range annotationPlan {
							if fixture.TranscriptID == content.ID {
								manifestPlan = append(manifestPlan, pull.ManifestAnnotation{ContentHash: fixture.Hash, AuthorUserID: fixture.AuthorUserID, AuthorUsername: fixture.AuthorUsername})
							}
						}
						expectedManifest := pull.PullManifest{ManifestVersion: pull.PullManifestVersion, VillageURL: server.URL, VillageHost: expected.VillageHost, TranscriptID: content.ID, LocalSessionID: content.LocalSessionID, OwnerUserID: content.OwnerUserID, OwnerUsername: content.OwnerUsername, ServedETag: `"` + hash + `"`, ServedBlobHash: hash, BlobContractVersion: defaults.PublishSchemaVersion, PullEnvelopeVersion: defaults.PullContractVersion, PulledAt: row.FirstPulledAt, Annotations: manifestPlan}
						if !reflect.DeepEqual(manifest, expectedManifest) {
							t.Fatalf("full content manifest identity changed: got=%+v want=%+v", manifest, expectedManifest)
						}
						manifestHashes := []string{}
						for _, annotation := range manifest.Annotations {
							manifestHashes = append(manifestHashes, annotation.ContentHash)
						}
						sort.Strings(manifestHashes)
						if !reflect.DeepEqual(manifestHashes, wantHashes) {
							t.Fatal("manifest annotation projection differs from exact fixture set")
						}
						if c.Preinstall {
							if !bytes.Equal(data, beforeBytes[content.ID]) || !bytes.Equal(manifestBytes, beforeManifest[content.ID]) {
								t.Fatal("content shortcut rewrote original content/provenance during optional metadata")
							}
							before := beforeRows[content.ID]
							if row.FirstPulledAt != before.FirstPulledAt {
								t.Fatal("content shortcut replaced original installation date")
							}
						}
					}
				}
				if !reflect.DeepEqual(actualSet, wantSet) {
					t.Fatalf("reopened content identities differ: got=%v want=%v", actualSet, wantSet)
				}
				annotations, err := db.ListPulledAnnotationHashesByTranscript(t.Context(), strings.TrimPrefix(server.URL, "http://"))
				if err != nil {
					t.Fatal(err)
				}
				actualAnnotations := map[schema.TranscriptID]map[string]bool{}
				for id, hashes := range annotations {
					if !wantSet[id] {
						t.Fatalf("annotation projection belongs to failed/unselected content: %s", id)
					}
					for _, hash := range hashes {
						if actualAnnotations[id] == nil {
							actualAnnotations[id] = map[string]bool{}
						}
						if actualAnnotations[id][hash] {
							t.Fatal("duplicate projected annotation hash")
						}
						actualAnnotations[id][hash] = true
					}
				}
				expectedAnnotations := map[schema.TranscriptID]map[string]bool{}
				for _, content := range corpus.Content {
					if wantSet[content.ID] {
						for _, hash := range expectedHashes(content.ID, content.AnnotationHashes) {
							if expectedAnnotations[content.ID] == nil {
								expectedAnnotations[content.ID] = map[string]bool{}
							}
							if expectedAnnotations[content.ID][hash] {
								t.Fatal("duplicate expected annotation hash")
							}
							expectedAnnotations[content.ID][hash] = true
						}
					}
				}
				if !reflect.DeepEqual(actualAnnotations, expectedAnnotations) {
					t.Fatalf("exact reopened annotation projection differs: got=%v want=%v", actualAnnotations, expectedAnnotations)
				}
				actualRows := readPullAssociationAnnotationRows(t, string(defaults.ResolveDBFilePathWith(dir)))
				expectedRows := map[string]store.PulledAnnotationRow{}
				for _, fixture := range annotationPlan {
					if wantSet[fixture.TranscriptID] {
						var pulledAt int64
						for _, row := range rows {
							if row.TranscriptID == fixture.TranscriptID {
								pulledAt = row.FirstPulledAt
							}
						}
						host := strings.TrimPrefix(server.URL, "http://")
						expectedRows[host+"|"+fixture.Hash] = store.PulledAnnotationRow{VillageHost: host, ContentHash: fixture.Hash, TranscriptID: fixture.TranscriptID, LocalSessionID: fixture.LocalSessionID, AuthorUserID: fixture.AuthorUserID, AuthorUsername: fixture.AuthorUsername, Payload: fixture.PayloadJSON, PulledAt: pulledAt, RetractedAt: fixture.RetractedAt}
					}
				}
				if !reflect.DeepEqual(actualRows, expectedRows) {
					t.Fatalf("exact reopened populated annotation payload/provenance differs: got=%+v want=%+v", actualRows, expectedRows)
				}
				if c.Preinstall && !reflect.DeepEqual(actualRows, beforeAnnotations) {
					t.Fatal("content shortcut changed prior annotation payload/author/date/retraction projection")
				}
				for _, content := range corpus.Content {
					if !wantSet[content.ID] {
						if _, err := os.Stat(filepath.Join(pullDirFor(t, dir, server.URL, content.ID), pull.ManifestFilename)); !os.IsNotExist(err) {
							t.Fatalf("failed/unselected content falsely installed a canonical manifest: %s err=%v", content.ID, err)
						}
					}
				}
			}
			verifyContent()
			if c.Mode == "switch" {
				current, err := auth.CaptureCredentials(t.Context(), dir)
				if err != nil {
					t.Fatal(err)
				}
				db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				rows, err := db.ReadTranscriptAssociations(t.Context(), current, ids)
				if err != nil {
					t.Fatal(err)
				}
				if rows[ids[0]].Metadata != nil {
					t.Fatal("old content action wrote private metadata to newly selected profile")
				}
				content, err := db.ListPulledTranscripts(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if len(content) != 1 {
					t.Fatal("optional account rotation rolled back independently successful content")
				}
				return
			}
			db, err := openPreparedStore(t, string(defaults.ResolveDBFilePathWith(dir)))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			evidence, err := db.ReadTranscriptAssociations(t.Context(), original, ids)
			if err != nil {
				t.Fatal(err)
			}
			if c.WantState == schema.PullAssociationMetadataInvalidated && len(evidence[ids[0]].Collectives) != 0 {
				t.Fatal("known negative left old current filter matches")
			}
			if c.WantState == schema.PullAssociationMetadataStale {
				saved := evidence[ids[0]]
				if saved.Metadata == nil || saved.Metadata.State != schema.PullAssociationMetadataStale {
					t.Fatal("optional failure did not persist stale metadata")
				}
				if saved.Metadata.ObservedAt == nil || !saved.Metadata.ObservedAt.Equal(corpus.ObservedAt) {
					t.Fatal("optional failure changed the last good source date")
				}
				if len(saved.Collectives) != 1 || saved.Collectives[0].Name != "Prior saved name" {
					t.Fatal("optional failure did not retain exact previously disclosed source and date in reopened Store")
				}
			}
			if c.WantState == schema.PullAssociationMetadataInvalidated && !evidence[ids[0]].Metadata.ObservedAt.After(corpus.ObservedAt) {
				t.Fatal("dedicated negative failed to record a genuinely new observation date")
			}
			if c.WantState == schema.PullAssociationMetadataObserved && evidence[ids[0]].Collectives[0].Name != "Renamed independently" {
				t.Fatal("content shortcut skipped new independent association name")
			}
			if c.DryRun {
				rows, err := db.ListPulledTranscripts(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 0 || evidence[ids[0]].Metadata != nil {
					t.Fatal("dry run wrote content or metadata")
				}
				return
			}
			if !c.WantError {
				manifest, err := os.ReadFile(filepath.Join(results[0].PullDir, pull.ManifestFilename))
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(manifest, []byte("associationMetadata")) || bytes.Contains(manifest, []byte("Renamed independently")) {
					t.Fatal("account private association leaked to content manifest")
				}
			}
		})
	}
}
