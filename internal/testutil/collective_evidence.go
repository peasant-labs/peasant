package testutil

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/collective_evidence.yaml
var collectiveEvidenceFixtureYAML []byte

type CollectiveEvidenceCase struct {
	Name         string                               `yaml:"name"`
	Kind         schema.LocalContributionEvidenceKind `yaml:"kind"`
	Mode         string                               `yaml:"mode"`
	Token        string                               `yaml:"token"`
	Limit        int                                  `yaml:"limit"`
	Page         int                                  `yaml:"page"`
	CollectiveID *schema.VillageUUID                  `yaml:"collectiveId"`
	WantStatus   int                                  `yaml:"wantStatus"`
	WantIDs      []string                             `yaml:"wantIDs"`
	WantTotal    int64                                `yaml:"wantTotal"`
}
type CollectiveEvidenceFixtures struct {
	RequiredNames         []string                 `yaml:"requiredNames"`
	CollectiveID          schema.VillageUUID       `yaml:"collectiveId"`
	AlternateCollectiveID schema.VillageUUID       `yaml:"alternateCollectiveId"`
	TranscriptID          schema.TranscriptID      `yaml:"transcriptId"`
	RecordedAt            time.Time                `yaml:"recordedAt"`
	ObservedAt            time.Time                `yaml:"observedAt"`
	Ordinals              []int32                  `yaml:"ordinals"`
	ReceiptIDs            []string                 `yaml:"receiptIDs"`
	DiagnosticIDs         []schema.VillageUUID     `yaml:"diagnosticIDs"`
	Cases                 []CollectiveEvidenceCase `yaml:"cases"`
}

func LoadCollectiveEvidenceFixtures(t testing.TB) CollectiveEvidenceFixtures {
	t.Helper()
	var fixtures CollectiveEvidenceFixtures
	d := yaml.NewDecoder(strings.NewReader(string(collectiveEvidenceFixtureYAML)))
	d.KnownFields(true)
	if err := d.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range fixtures.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatal("missing/duplicate contribution evidence fixture name")
		}
		names[c.Name] = true
		if !c.Kind.IsValid() {
			t.Fatalf("unknown fixture evidence kind %q", c.Kind)
		}
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required evidence fixture %s missing", name)
		}
	}
	return fixtures
}

// SeedCollectiveEvidence uses real guarded Store writers, not SQL copies of the
// synchronization model. All source records and expected IDs come from YAML.
func SeedCollectiveEvidence(t testing.TB, ctx context.Context, db *store.Store, snapshot auth.CredentialSnapshot, f CollectiveEvidenceFixtures) {
	t.Helper()
	events := make([]schema.VillageShareEvent, 0, len(f.Ordinals))
	for _, ordinal := range f.Ordinals {
		events = append(events, schema.VillageShareEvent{EventNum: ordinal, Status: schema.VillageShareStatusPending, RecordedAt: f.RecordedAt})
	}
	invocation, err := db.BeginCollectiveObservation(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.InstallContributionAttemptSources(ctx, invocation, []store.ContributionAttemptSource{{CollectiveID: f.CollectiveID, TranscriptID: f.TranscriptID, Events: events, ObservedAt: f.ObservedAt}}); err != nil {
		t.Fatal(err)
	}
	for i, id := range f.ReceiptIDs {
		collective := f.CollectiveID
		if i == len(f.ReceiptIDs)-1 {
			collective = f.AlternateCollectiveID
		}
		r := schema.LocalConfirmedContributionReceipt{ReceiptID: id, Operation: schema.CollectiveOperationShare, TranscriptID: f.TranscriptID, CollectiveID: &collective, ConfirmedAt: f.ObservedAt, Outcome: schema.LocalConfirmedContributionSubmitted}
		if err := db.RecordContributionReceipt(ctx, snapshot, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range f.DiagnosticIDs {
		if err := db.RecordOperationDiagnostic(ctx, snapshot, FixtureCollectiveDiagnostic(f, id)); err != nil {
			t.Fatal(err)
		}
	}
}

func FixtureCollectiveDiagnostic(f CollectiveEvidenceFixtures, id schema.VillageUUID) schema.LocalOperationDiagnostic {
	return schema.LocalOperationDiagnostic{OperationID: id, Operation: schema.CollectiveOperationPull, TranscriptID: &f.TranscriptID, CollectiveID: &f.CollectiveID, AttemptedAt: f.ObservedAt, Outcome: schema.LocalOperationDiagnosticUncertain, Issue: schema.OperationIssue{Code: schema.OperationIssueRemote, Operation: schema.CollectiveOperationPull, Message: "Fixture source response was uncertain; no contribution success was inferred.", Retry: "Fetch the selected metadata explicitly before retrying."}}
}

func CollectiveEvidenceIDs(response schema.LocalContributionHistoryResponse) []string {
	ids := []string{}
	switch response.Kind {
	case schema.LocalContributionEvidenceRemoteAttempts:
		for _, row := range response.RemoteAttempts {
			ids = append(ids, fmt.Sprint(row.Attempt.EventNum))
		}
	case schema.LocalContributionEvidenceReceipts:
		for _, row := range response.Receipts {
			ids = append(ids, row.ReceiptID)
		}
	case schema.LocalContributionEvidenceOperationDiagnostics:
		for _, row := range response.OperationDiagnostics {
			ids = append(ids, row.OperationID.String())
		}
	}
	return ids
}
