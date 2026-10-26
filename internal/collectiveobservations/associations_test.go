package collectiveobservations

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/associations.yaml
var associationYAML []byte

type associationCase struct {
	Name          string                               `yaml:"name"`
	Before        []schema.VillageTranscriptCollective `yaml:"before"`
	After         []schema.VillageTranscriptCollective `yaml:"after"`
	SkipBefore    bool                                 `yaml:"skipBefore"`
	Status        int                                  `yaml:"status"`
	CancelRead    bool                                 `yaml:"cancelRead"`
	WantError     bool                                 `yaml:"wantError"`
	WantState     schema.PullAssociationMetadataState  `yaml:"wantState"`
	WantNames     []string                             `yaml:"wantNames"`
	RetainedNames []string                             `yaml:"retainedNames"`
	WantVersions  int64                                `yaml:"wantVersions"`
}

func TestAssociationServiceRetainsScopedEvidence(t *testing.T) {
	var fixtures struct {
		TranscriptID  schema.TranscriptID `yaml:"transcriptId"`
		ObservedAt    time.Time           `yaml:"observedAt"`
		RequiredNames []string            `yaml:"requiredNames"`
		Cases         []associationCase   `yaml:"cases"`
	}
	d := yaml.NewDecoder(strings.NewReader(string(associationYAML)))
	d.KnownFields(true)
	if err := d.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range fixtures.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatal("missing/duplicate association case")
		}
		names[c.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required association case %s missing", name)
		}
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			home := t.TempDir()
			db := storetest.Open(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			creds := auth.Credentials{APIKey: "fixture-key", KeyID: "fixture-key-id", UserID: "11111111-1111-4111-8111-111111111111", Username: "fixture"}
			var phase atomic.Int32
			requested := make(chan struct{}, 1)
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Errorf("unexpected metadata request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(400)
					return
				}
				if r.URL.Path == "/api/v1/auth/me" {
					_ = json.NewEncoder(w).Encode(schema.VillageUser{ID: schema.VillageUUID(creds.UserID)})
					return
				}
				if r.URL.Path != "/api/v1/transcripts/"+fixtures.TranscriptID.String()+"/collectives" {
					t.Errorf("foreign metadata used non-dedicated endpoint %s", r.URL.Path)
					w.WriteHeader(400)
					return
				}
				if phase.Load() == 0 {
					_ = json.NewEncoder(w).Encode(schema.VillageTranscriptCollectivesResponse{Collectives: c.Before})
					return
				}
				if c.CancelRead {
					requested <- struct{}{}
					<-r.Context().Done()
					return
				}
				if c.Status != 0 {
					w.WriteHeader(c.Status)
					_ = json.NewEncoder(w).Encode(schema.VillageErrorResponse{Error: "unavailable"})
					return
				}
				_ = json.NewEncoder(w).Encode(schema.VillageTranscriptCollectivesResponse{Collectives: c.After})
			}))
			defer remote.Close()
			creds.VillageURL = remote.URL
			if err := auth.SaveCredentialsFrom(&creds, home); err != nil {
				t.Fatal(err)
			}
			at := fixtures.ObservedAt
			service := New(Dependencies{Store: db, ConfigHome: home, Now: func() time.Time { return at }})
			if !c.SkipBefore {
				result, err := service.ObserveAssociations(ctx, fixtures.TranscriptID)
				if err != nil || result.State != schema.PullAssociationMetadataObserved {
					t.Fatalf("initial observation: %#v %v", result, err)
				}
			}
			phase.Store(1)
			at = at.Add(time.Hour)
			var metadata schema.PullAssociationMetadataResult
			var err error
			if c.CancelRead {
				operation, stop := context.WithCancel(ctx)
				defer stop()
				done := make(chan struct{})
				go func() { metadata, err = service.ObserveAssociations(operation, fixtures.TranscriptID); close(done) }()
				select {
				case <-requested:
					stop()
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			} else {
				metadata, err = service.ObserveAssociations(ctx, fixtures.TranscriptID)
			}
			if (err != nil) != c.WantError || metadata.State != c.WantState {
				t.Fatalf("metadata=%#v err=%v want=%s error=%v", metadata, err, c.WantState, c.WantError)
			}
			if err := metadata.Validate(); err != nil {
				t.Fatal(err)
			}
			if c.WantState == schema.PullAssociationMetadataStale && (!metadata.ObservedAt.Equal(fixtures.ObservedAt) || metadata.Warning == nil) {
				t.Fatal("failure fabricated a new observation time or omitted warning")
			}
			if c.WantState == schema.PullAssociationMetadataUnknown && metadata.ObservedAt != nil {
				t.Fatal("unknown metadata fabricated a date")
			}
			rows, err := service.Associations(ctx, []schema.TranscriptID{fixtures.TranscriptID, schema.TranscriptID("66666666-6666-4666-8666-666666666666")})
			if err != nil {
				t.Fatal(err)
			}
			gotNames := []string{}
			for _, row := range rows[fixtures.TranscriptID].Collectives {
				gotNames = append(gotNames, row.Name)
			}
			if !reflect.DeepEqual(gotNames, c.WantNames) {
				t.Fatalf("actual per-row associations=%v want=%v", gotNames, c.WantNames)
			}
			if rows[schema.TranscriptID("66666666-6666-4666-8666-666666666666")].Metadata != nil {
				t.Fatal("selector union inferred an unrelated transcript association")
			}
			conn, err := db.Pool().Take(ctx)
			if err != nil {
				t.Fatal(err)
			}
			retained := []string{}
			var versions int64
			err = sqlitex.Execute(conn, `SELECT name FROM transcript_collective_evidence ORDER BY name`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error { retained = append(retained, stmt.ColumnText(0)); return nil }})
			if err == nil {
				err = sqlitex.Execute(conn, `SELECT count(*) FROM transcript_collective_evidence`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error { versions = stmt.ColumnInt64(0); return nil }})
			}
			db.Pool().Put(conn)
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(retained)
			if !reflect.DeepEqual(retained, c.RetainedNames) || versions != c.WantVersions {
				t.Fatalf("dated evidence=%v versions=%d want=%v %d", retained, versions, c.RetainedNames, c.WantVersions)
			}
			// A new selected principal must not see any of this private observation.
			other := creds
			other.UserID = "77777777-7777-4777-8777-777777777777"
			if err := auth.SaveCredentialsFrom(&other, home); err != nil {
				t.Fatal(err)
			}
			rows, err = service.Associations(ctx, []schema.TranscriptID{fixtures.TranscriptID})
			if err != nil || rows[fixtures.TranscriptID].Metadata != nil {
				t.Fatalf("association leaked into other principal: %#v %v", rows, err)
			}
		})
	}
}
