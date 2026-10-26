package store_test

import (
	"context"
	_ "embed"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/contribution_read_snapshot.yaml
var contributionReadSnapshotYAML []byte

// The SQLite authorizer is only a deterministic scheduling seam: it changes
// neither SQL nor results. A second real Store commits after token/coverage are
// read but before count/rows. Separate captured profiles for the SAME account
// allow a legitimate concurrent writer past the per-profile credential guard.
func TestContributionHistorySingleSQLiteSnapshot(t *testing.T) {
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name      string   `yaml:"name"`
			ReceiptID string   `yaml:"receiptId"`
			WantIDs   []string `yaml:"wantIDs"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(contributionReadSnapshotYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range fixtures.Cases {
		if names[c.Name] {
			t.Fatal("duplicate read snapshot fixture")
		}
		names[c.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required snapshot fixture %s absent", name)
		}
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "snapshots.db")
			storetest.CopyGoldenTo(t, path)
			reader, err := store.Open(path, store.WithPoolSize(1), store.WithSkipMigrations())
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			writer, err := store.Open(path, store.WithPoolSize(1), store.WithSkipMigrations())
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			creds := auth.Credentials{APIKey: "fixture", KeyID: "fixture-key", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture", VillageURL: "https://village.invalid/installation"}
			readHome, writeHome := t.TempDir(), t.TempDir()
			if err := auth.SaveCredentialsFrom(&creds, readHome); err != nil {
				t.Fatal(err)
			}
			if err := auth.SaveCredentialsFrom(&creds, writeHome); err != nil {
				t.Fatal(err)
			}
			readSnapshot, err := auth.CaptureCredentials(ctx, readHome)
			if err != nil {
				t.Fatal(err)
			}
			writeSnapshot, err := auth.CaptureCredentials(ctx, writeHome)
			if err != nil {
				t.Fatal(err)
			}
			f := testutil.LoadCollectiveEvidenceFixtures(t)
			testutil.SeedCollectiveEvidence(t, ctx, reader, readSnapshot, f)
			beginWrite := make(chan struct{})
			writeDone := make(chan error, 1)
			workerStopped := make(chan struct{})
			var once sync.Once
			go func() {
				defer close(workerStopped)
				select {
				case <-beginWrite:
				case <-ctx.Done():
					writeDone <- ctx.Err()
					return
				}
				writeDone <- writer.RecordContributionReceipt(ctx, writeSnapshot, schema.LocalConfirmedContributionReceipt{ReceiptID: c.ReceiptID, Operation: schema.CollectiveOperationPublish, TranscriptID: f.TranscriptID, ConfirmedAt: f.ObservedAt, Outcome: schema.LocalConfirmedContributionPublished})
			}()
			defer func() {
				cancel()
				select {
				case <-workerStopped:
				case <-time.After(2 * time.Second):
					t.Error("selected-category writer did not stop before Store cleanup")
				}
			}()
			conn, err := reader.Pool().Take(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = conn.SetAuthorizer(sqlite.AuthorizeFunc(func(action sqlite.Action) sqlite.AuthResult {
				if action.Type() == sqlite.OpRead && action.Table() == "local_contribution_receipts" {
					once.Do(func() {
						close(beginWrite)
						select {
						case err := <-writeDone:
							if err != nil {
								t.Errorf("concurrent real Store writer: %v", err)
							}
						case <-ctx.Done():
							t.Errorf("concurrent writer did not wake reader: %v", ctx.Err())
						}
					})
				}
				return sqlite.AuthResultOK
			}))
			reader.Pool().Put(conn)
			if err != nil {
				t.Fatal(err)
			}
			response, err := reader.ReadContributionHistory(ctx, readSnapshot, schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceReceipts, Page: 1, Limit: 200})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-beginWrite:
			default:
				t.Fatal("read never reached the selected-row scheduling seam")
			}
			if response.Total != int64(len(c.WantIDs)) || !reflect.DeepEqual(testutil.CollectiveEvidenceIDs(response), c.WantIDs) {
				t.Fatalf("token snapshot mixed later rows/total: total=%d IDs=%v", response.Total, testutil.CollectiveEvidenceIDs(response))
			}
			token := response.TraversalRevision
			_, err = reader.ReadContributionHistory(ctx, readSnapshot, schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceReceipts, Page: 2, Limit: 200, Revision: &token})
			var conflict *store.ContributionHistoryError
			if !errors.As(err, &conflict) || conflict.Status != 409 {
				t.Fatalf("concurrent selected mutation did not invalidate old traversal: %v", err)
			}
		})
	}
}
