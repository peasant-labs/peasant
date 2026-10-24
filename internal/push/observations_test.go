package push_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/collectiveobservations"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/action_observations.yaml
var actionObservationYAML []byte

type actionObservationFixture struct {
	RequiredNames      []string            `yaml:"requiredNames"`
	Now                time.Time           `yaml:"now"`
	UserID             string              `yaml:"userID"`
	OtherUserID        string              `yaml:"otherUserID"`
	CollectiveID       schema.VillageUUID  `yaml:"collectiveID"`
	SecondCollectiveID schema.VillageUUID  `yaml:"secondCollectiveID"`
	TranscriptID       schema.TranscriptID `yaml:"transcriptID"`
	Cases              []struct {
		Name                   string                    `yaml:"name"`
		Share                  bool                      `yaml:"share"`
		Remove                 bool                      `yaml:"remove"`
		BeforeStatus           schema.VillageShareStatus `yaml:"beforeStatus"`
		FailWithdrawal         int                       `yaml:"failWithdrawal"`
		WithdrawalTimeout      bool                      `yaml:"withdrawalTimeout"`
		LoseWithdrawalResponse bool                      `yaml:"loseWithdrawalResponse"`
		RotateAfterWithdrawal  bool                      `yaml:"rotateAfterWithdrawal"`
		CloseAfterWithdrawal   bool                      `yaml:"closeAfterWithdrawal"`
		ObservationStatus      int                       `yaml:"observationStatus"`
		ObservationTimeout     bool                      `yaml:"observationTimeout"`
		Unshares               int32                     `yaml:"unshares"`
		Withdrawn              bool                      `yaml:"withdrawn"`
		ErrorOmits             []string                  `yaml:"errorOmits"`
		ErrorContainsAll       []string                  `yaml:"errorContainsAll"`
		RefusalBody            string                    `yaml:"refusalBody"`
		History                []struct {
			CollectiveID schema.VillageUUID       `yaml:"collectiveID"`
			TranscriptID schema.TranscriptID      `yaml:"transcriptID"`
			Attempt      schema.VillageShareEvent `yaml:"attempt"`
		} `yaml:"history"`
		ExpectedDiagnostics map[schema.VillageUUID]struct {
			Operation    schema.CollectiveOperation             `yaml:"operation"`
			TranscriptID schema.TranscriptID                    `yaml:"transcriptID"`
			Outcome      schema.LocalOperationDiagnosticOutcome `yaml:"outcome"`
		} `yaml:"expectedDiagnostics"`
		AutoApprove             bool                                             `yaml:"autoApprove"`
		FailObservation         bool                                             `yaml:"failObservation"`
		RotateDuringObservation bool                                             `yaml:"rotateDuringObservation"`
		RotateDuringTarget      bool                                             `yaml:"rotateDuringTarget"`
		RotateDuringReadback    bool                                             `yaml:"rotateDuringReadback"`
		RotateABA               bool                                             `yaml:"rotateABA"`
		Attempts                map[schema.VillageUUID]schema.VillageShareStatus `yaml:"attempts"`
		SecondTarget            bool                                             `yaml:"secondTarget"`
		SecondRefused           bool                                             `yaml:"secondRefused"`
		ExistingShare           bool                                             `yaml:"existingShare"`
		FailShare               bool                                             `yaml:"failShare"`
		FailShareStatus         int                                              `yaml:"failShareStatus"`
		LoseShareResponse       bool                                             `yaml:"loseShareResponse"`
		FailTarget              bool                                             `yaml:"failTarget"`
		FailReadback            bool                                             `yaml:"failReadback"`
		RotateAfterPublish      bool                                             `yaml:"rotateAfterPublish"`
		RotateAfterShare        bool                                             `yaml:"rotateAfterShare"`
		RotateSameAccount       bool                                             `yaml:"rotateSameAccount"`
		CloseAfterPublish       bool                                             `yaml:"closeAfterPublish"`
		CloseAfterShare         bool                                             `yaml:"closeAfterShare"`
		DryRun                  bool                                             `yaml:"dryRun"`
		Receipts                int64                                            `yaml:"receipts"`
		Publishes               int                                              `yaml:"publishes"`
		Shares                  int32                                            `yaml:"shares"`
		Status                  schema.VillageShareStatus                        `yaml:"status"`
		WantError               bool                                             `yaml:"wantError"`
		ErrorContains           string                                           `yaml:"errorContains"`
	} `yaml:"cases"`
}

// This is the mounted action implementation, real scoped service/credential
// guard and real SQLite. Only Village is an HTTP fixture dependency. No extra
// process harness, copied action algorithm, live profile or external DB.
func TestActionObservationProductionPaths(t *testing.T) {
	var f actionObservationFixture
	if err := testutil.DecodeNamedFixtureYAML(actionObservationYAML, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			path := storetest.CopyGoldenDB(t)
			db, err := store.Open(path, store.WithSkipMigrations())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			seedShareContent(t, db, false, "synthetic action evidence")
			collectives := []testutil.VillageCollective{{ID: f.CollectiveID, Name: "fixture", Member: true, Acceptance: schema.VillageGroupAcceptanceCurated}}
			if c.AutoApprove {
				collectives[0].Acceptance = schema.VillageGroupAcceptanceOpen
			}
			if c.SecondTarget {
				collectives = append(collectives, testutil.VillageCollective{ID: f.SecondCollectiveID, Name: "second", Member: !c.SecondRefused, Acceptance: schema.VillageGroupAcceptanceCurated})
			}
			remote := testutil.NewCollectiveVillage(t, collectives...)
			if c.ExistingShare {
				remote.Share(schema.TranscriptID(testutil.TestSessionUUID), f.CollectiveID, schema.VillageShareStatusApproved)
			}
			if c.Remove {
				remote.Share(f.TranscriptID, f.CollectiveID, c.BeforeStatus)
			}
			upstream, err := url.Parse(remote.URL())
			if err != nil {
				t.Fatal(err)
			}
			proxy := httputil.NewSingleHostReverseProxy(upstream)
			creds := &auth.Credentials{APIKey: "synthetic-key", KeyID: "synthetic-key-id", UserID: f.UserID, Username: "fixture"}
			var calls, shares, unshares, eventReads atomic.Int32
			var confirmedTranscript atomic.Value
			proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(http.StatusBadGateway) }
			rotate := func() {
				other := *creds
				other.UserID, other.APIKey = f.OtherUserID, "other-synthetic-key"
				if c.RotateSameAccount {
					other.UserID, other.APIKey = creds.UserID, creds.APIKey
				}
				if err := auth.SaveCredentialsFrom(&other, home); err != nil {
					t.Error(err)
				}
				if c.RotateABA {
					if err := auth.SaveCredentialsFrom(creds, home); err != nil {
						t.Error(err)
					}
				}
			}
			proxy.ModifyResponse = func(response *http.Response) error {
				if c.RotateDuringReadback && strings.HasSuffix(response.Request.URL.Path, "/events") && eventReads.Load() == 1 {
					rotate()
				}
				observationRead := eventReads.Load() == 2
				if c.Remove {
					observationRead = eventReads.Load() == 1
				}
				if c.RotateDuringObservation && strings.HasSuffix(response.Request.URL.Path, "/events") && observationRead {
					rotate()
				}
				if response.Request.Method == http.MethodDelete {
					if c.RotateAfterWithdrawal {
						rotate()
					}
					if c.LoseWithdrawalResponse {
						_ = response.Body.Close()
						return errors.New("synthetic withdrawal response loss")
					}
					if c.CloseAfterWithdrawal {
						_ = db.Close()
					}
				}
				if response.Request.Method == http.MethodPost && strings.HasSuffix(response.Request.URL.Path, "/publish") {
					data, err := io.ReadAll(response.Body)
					if err != nil {
						return err
					}
					_ = response.Body.Close()
					response.Body = io.NopCloser(bytes.NewReader(data))
					receipt, err := schema.DecodePublishResponse(data)
					if err != nil {
						return err
					}
					confirmedTranscript.Store(receipt.TranscriptID)
					if c.RotateAfterPublish {
						rotate()
					}
					if c.CloseAfterPublish {
						_ = db.Close()
					}
				}
				if response.Request.Method == http.MethodPost && strings.HasSuffix(response.Request.URL.Path, "/share") {
					if c.RotateAfterShare {
						rotate()
					}
					if c.CloseAfterShare {
						_ = db.Close()
					}
					if c.LoseShareResponse {
						_ = response.Body.Close()
						return errors.New("synthetic response loss")
					}
				}
				return nil
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch {
				case r.URL.Path == "/api/v1/auth/me":
					_ = json.NewEncoder(w).Encode(schema.VillageUser{ID: schema.VillageUUID(f.UserID)})
				case r.URL.Path == "/api/v1/collectives/"+f.CollectiveID.String() || r.URL.Path == "/api/v1/collectives/"+f.SecondCollectiveID.String():
					if c.FailTarget {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					id := f.CollectiveID
					if strings.HasSuffix(r.URL.Path, f.SecondCollectiveID.String()) {
						id = f.SecondCollectiveID
					}
					_ = json.NewEncoder(w).Encode(schema.VillageGroupDetailResponse{Group: schema.VillageGroupDetailRecord{Creator: schema.AnonymousVillageOwner(), ID: id, Name: "fixture", AcceptanceMode: schema.VillageGroupAcceptanceCurated, DataAccess: schema.VillageGroupDataAccessMembersOnly}})
					if c.RotateDuringTarget {
						rotate()
					}
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/share"):
					shares.Add(1)
					data, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					var request schema.VillageShareTranscriptRequest
					if err := json.Unmarshal(data, &request); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					wanted := []schema.VillageUUID{f.CollectiveID}
					if c.SecondTarget {
						wanted = append(wanted, f.SecondCollectiveID)
					}
					if len(request.GroupIDs) != len(wanted) {
						t.Errorf("share split the actual consented target set: got=%v want=%v", request.GroupIDs, wanted)
					}
					for i, id := range request.GroupIDs {
						if i >= len(wanted) || id != wanted[i] {
							t.Errorf("share changed target identity: %v", request.GroupIDs)
						}
					}
					r.Body = io.NopCloser(bytes.NewReader(data))
					if c.FailShare || c.FailShareStatus != 0 {
						status := c.FailShareStatus
						if status == 0 {
							status = http.StatusForbidden
						}
						w.WriteHeader(status)
						_, _ = io.WriteString(w, c.RefusalBody)
						// Refusal responses must also run the persistence fault;
						// they never pass through the successful proxy callback.
						if c.CloseAfterShare {
							_ = db.Close()
						}
						return
					}
					proxy.ServeHTTP(w, r)
				case r.Method == http.MethodDelete:
					unshares.Add(1)
					if c.WithdrawalTimeout {
						<-r.Context().Done()
						return
					}
					if c.FailWithdrawal != 0 {
						w.WriteHeader(c.FailWithdrawal)
						_, _ = io.WriteString(w, c.RefusalBody)
						if c.CloseAfterWithdrawal {
							_ = db.Close()
						}
						return
					}
					proxy.ServeHTTP(w, r)
				case c.FailReadback && strings.HasSuffix(r.URL.Path, "/events"):
					w.WriteHeader(http.StatusServiceUnavailable)
				case strings.HasSuffix(r.URL.Path, "/events"):
					if c.Remove && c.ObservationStatus != 0 {
						w.WriteHeader(c.ObservationStatus)
						return
					}
					if c.Remove && c.ObservationTimeout {
						<-r.Context().Done()
						return
					}
					if eventReads.Add(1) == 2 && c.FailObservation {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					proxy.ServeHTTP(w, r)
				default:
					proxy.ServeHTTP(w, r)
				}
			}))
			defer server.Close()
			creds.VillageURL = server.URL
			if err := auth.SaveCredentialsFrom(creds, home); err != nil {
				t.Fatal(err)
			}
			var httpClient *http.Client
			if c.ObservationTimeout || c.WithdrawalTimeout {
				httpClient = &http.Client{Timeout: 250 * time.Millisecond}
			}
			client := village.NewVillageClient(server.URL, creds.APIKey, httpClient)
			service := collectiveobservations.New(collectiveobservations.Dependencies{Store: db, ConfigHome: home, Now: func() time.Time { return f.Now }, NewIdentityReader: func(auth.Credentials) collectiveobservations.IdentityReader { return client }})
			cfg := config.BaseConfig()
			pipeline, err := push.NewPipeline(db, client, creds, cfg, &ingest.OSFileSystem{}, push.PipelineConfig{Force: true, DryRun: c.DryRun}, &testutil.NoopRedactor{}, io.Discard, push.WithObservations(service, func() time.Time { return f.Now }))
			if err != nil {
				t.Fatal(err)
			}
			var message string
			beforeCommits, err := storetest.CountWALCommitFrames(path)
			if err != nil {
				t.Fatal(err)
			}
			if c.Share || c.Remove {
				targets := []schema.VillageUUID{f.CollectiveID}
				if c.SecondTarget {
					targets = append(targets, f.SecondCollectiveID)
				}
				changes := push.CollectiveChanges{Add: targets}
				if c.Remove {
					changes = push.CollectiveChanges{Remove: targets}
				}
				published, runErr := (push.SharePublish{Pipeline: pipeline, Store: db, Village: client, Creds: creds}).Run(ctx, []string{testutil.TestSessionUUID}, changes)
				if runErr != nil {
					message = runErr.Error()
				} else if published.Response.Errors != 0 {
					message = published.Response.Sessions[0].Error
				}
				if c.Status == schema.VillageShareStatusPending && message == "" {
					steps := published.Response.Sessions[0].Steps
					if steps[len(steps)-1].Outcome != schema.SyncPushStepPendingApproval {
						t.Fatal("pending submission was reported as approved")
					}
				}
			} else {
				result, runErr := pipeline.Run(ctx)
				if runErr != nil {
					message = runErr.Error()
				} else if result.Errors != 0 {
					message = result.Sessions[0].Error.Error()
				}
			}
			if (message != "") != c.WantError || !strings.Contains(message, c.ErrorContains) {
				t.Fatalf("action error=%q wantError=%v containing=%q", message, c.WantError, c.ErrorContains)
			}
			for _, wanted := range c.ErrorContainsAll {
				if !strings.Contains(message, wanted) {
					t.Fatalf("action lost required failure evidence: %q omits %q", message, wanted)
				}
			}
			for _, unwanted := range c.ErrorOmits {
				if strings.Contains(message, unwanted) {
					t.Fatalf("action misreported actual effect: %q includes %q", message, unwanted)
				}
			}
			if unshares.Load() != c.Unshares {
				t.Fatalf("withdrawal requests=%d want=%d", unshares.Load(), c.Unshares)
			}
			if c.Remove {
				_, live := remote.Audience(f.TranscriptID)[f.CollectiveID]
				if live == c.Withdrawn {
					t.Fatalf("actual remote withdrawal=%v want=%v", !live, c.Withdrawn)
				}
			}
			if len(remote.Publishes()) != c.Publishes || shares.Load() != c.Shares {
				t.Fatalf("remote publishes=%d shares=%d", len(remote.Publishes()), shares.Load())
			}
			if c.Publishes > 0 && confirmedTranscript.Load() != f.TranscriptID {
				t.Fatalf("fixture transcript identity=%s actual authoritative response=%v", f.TranscriptID, confirmedTranscript.Load())
			}
			if c.SecondRefused && len(remote.Audience(schema.TranscriptID(testutil.TestSessionUUID))) != 0 {
				t.Fatal("mixed-target refusal applied a prefix or changed audience")
			}
			if c.DryRun && calls.Load() != 0 {
				t.Fatal("dry run contacted Village")
			}
			if c.DryRun {
				afterCommits, err := storetest.CountWALCommitFrames(path)
				if err != nil || afterCommits != beforeCommits {
					t.Fatalf("dry run wrote local state: before=%d after=%d err=%v", beforeCommits, afterCommits, err)
				}
			}
			if c.CloseAfterPublish || c.CloseAfterShare || c.CloseAfterWithdrawal {
				db, err = store.Open(path, store.WithSkipMigrations())
				if err != nil {
					t.Fatal(err)
				}
				service = collectiveobservations.New(collectiveobservations.Dependencies{Store: db, ConfigHome: home})
			}
			rotated := c.RotateAfterPublish || c.RotateAfterShare ||
				c.RotateAfterWithdrawal || c.RotateDuringObservation ||
				c.RotateDuringTarget || c.RotateDuringReadback
			if rotated {
				// The selected NEW account receives none of the old remote effects.
				// Restore the ORIGINAL saved generation only after proving isolation.
				selected, e := service.History(ctx, schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceReceipts, Page: 1, Limit: 200})
				otherPrincipal := !c.RotateSameAccount && !c.RotateABA
				if e != nil || (otherPrincipal && selected.Total != 0) {
					t.Fatalf("new account contaminated: %+v %v", selected, e)
				}
				newAttempts, e := service.History(ctx, schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceRemoteAttempts, Page: 1, Limit: 200})
				if e != nil || newAttempts.Total != 0 {
					t.Fatalf("new account attempt contamination: %+v %v", newAttempts, e)
				}
				newDiagnostics, e := service.History(ctx, schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceOperationDiagnostics, Page: 1, Limit: 200})
				if e != nil || newDiagnostics.Total != 0 {
					t.Fatalf("new account diagnostic contamination: %+v %v", newDiagnostics, e)
				}
				if err := auth.SaveCredentialsFrom(creds, home); err != nil {
					t.Fatal(err)
				}
			}
			receipts, err := service.History(ctx, schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceReceipts, Page: 1, Limit: 200})
			if err != nil || receipts.Total != c.Receipts {
				t.Fatalf("receipts=%+v err=%v want=%d", receipts, err, c.Receipts)
			}
			for _, receipt := range receipts.Receipts {
				if receipt.ConfirmedAt != f.Now || receipt.TranscriptID != f.TranscriptID || receipt.ReceiptID == "" {
					t.Fatalf("receipt not authentic: %+v", receipt)
				}
				if receipt.Operation == schema.CollectiveOperationPublish &&
					(receipt.Outcome != schema.LocalConfirmedContributionPublished || receipt.CollectiveID != nil || receipt.SourceReceiptID == nil) {
					t.Fatalf("upload inferred collective approval: %+v", receipt)
				}
				if receipt.Operation == schema.CollectiveOperationShare &&
					(receipt.Outcome != schema.LocalConfirmedContributionSubmitted || receipt.CollectiveID == nil ||
						(*receipt.CollectiveID != f.CollectiveID && *receipt.CollectiveID != f.SecondCollectiveID)) {
					t.Fatalf("share receipt: %+v", receipt)
				}
			}
			diagnostics, err := service.History(ctx, schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceOperationDiagnostics, Page: 1, Limit: 200})
			if err != nil || diagnostics.Total != int64(len(c.ExpectedDiagnostics)) {
				t.Fatalf("diagnostics=%+v err=%v expected=%v", diagnostics, err, c.ExpectedDiagnostics)
			}
			operationIDs := map[schema.VillageUUID]bool{}
			for _, diagnostic := range diagnostics.OperationDiagnostics {
				if diagnostic.TranscriptID == nil || diagnostic.CollectiveID == nil || operationIDs[diagnostic.OperationID] {
					t.Fatalf("diagnostic identity/outcome=%+v", diagnostic)
				}
				if err := diagnostic.Validate(); err != nil {
					t.Fatal(err)
				}
				operationIDs[diagnostic.OperationID] = true
				want, found := c.ExpectedDiagnostics[*diagnostic.CollectiveID]
				if !found || diagnostic.Operation != want.Operation ||
					*diagnostic.TranscriptID != want.TranscriptID || diagnostic.Outcome != want.Outcome {
					t.Fatalf("unexpected or incorrectly scoped diagnostic: %+v expected=%v", diagnostic, c.ExpectedDiagnostics)
				}
				delete(c.ExpectedDiagnostics, *diagnostic.CollectiveID)
				if strings.Contains(diagnostic.Issue.Message, creds.APIKey) {
					t.Fatal("diagnostic exposed credential")
				}
			}
			if len(c.ExpectedDiagnostics) != 0 {
				t.Fatalf("missing targeted diagnostics: %v", c.ExpectedDiagnostics)
			}
			attempts, err := service.History(ctx, schema.LocalContributionHistoryQuery{Kind: schema.LocalContributionEvidenceRemoteAttempts, Page: 1, Limit: 200})
			if err != nil || attempts.Total != int64(len(c.Attempts)+len(c.History)) {
				t.Fatalf("observed attempts=%+v expected=%v err=%v", attempts, c.Attempts, err)
			}
			if c.Remove {
				for index, row := range attempts.RemoteAttempts {
					want := c.History[index]
					if row.CollectiveID != want.CollectiveID || row.TranscriptID != want.TranscriptID || !reflect.DeepEqual(row.Attempt, want.Attempt) {
						t.Fatalf("withdrawal history=%+v want=%+v", row, want)
					}
					if err := row.Attempt.Validate(); err != nil {
						t.Fatal(err)
					}
				}
				return
			}
			for _, row := range attempts.RemoteAttempts {
				if row.TranscriptID.String() != testutil.TestSessionUUID || row.Attempt.EventNum != 1 || row.Attempt.Status != c.Attempts[row.CollectiveID] {
					t.Fatalf("incorrect authoritative attempt identity: %+v", row)
				}
				if err := row.Attempt.Validate(); err != nil {
					t.Fatal(err)
				}
				if row.Attempt.Status == schema.VillageShareStatusApproved && (row.Attempt.DecidedAt != nil || row.Attempt.DecidedByActor != schema.VillageShareEventActorModerator) {
					t.Fatalf("initial automatic approval invented a decision timestamp: %+v", row)
				}
				delete(c.Attempts, row.CollectiveID)
			}
			if len(c.Attempts) != 0 {
				t.Fatalf("requested target evidence omitted: %v", c.Attempts)
			}
		})
	}
}
