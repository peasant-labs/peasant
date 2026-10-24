// Command collectiveobservations-fixture seeds disposable browser-journey state
// through the same guarded Store and credential constructors as the application.
package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/observations.yaml
var fixtureYAML []byte

type account struct {
	Name     string `yaml:"name"`
	UserID   string `yaml:"userId"`
	Username string `yaml:"username"`
}
type fixtures struct {
	RequiredNames     []string  `yaml:"requiredNames"`
	Accounts          []account `yaml:"accounts"`
	CollectiveID      string    `yaml:"collectiveId"`
	TranscriptID      string    `yaml:"transcriptId"`
	ObservedAt        time.Time `yaml:"observedAt"`
	OldName           string    `yaml:"oldName"`
	NewName           string    `yaml:"newName"`
	SourceJSON        string    `yaml:"sourceJSON"`
	HistorySourceJSON string    `yaml:"historySourceJSON"`
	AssociationJSON   string    `yaml:"associationJSON"`
	AttemptsJSON      string    `yaml:"attemptsJSON"`
	ReceiptIDs        []string  `yaml:"receiptIds"`
	DiagnosticIDs     []string  `yaml:"diagnosticIds"`
	IssueJSON         string    `yaml:"issueJSON"`
	TerminalJSON      string    `yaml:"terminalJSON"`
}

func loadFixtures() (fixtures, error) {
	var f fixtures
	d := yaml.NewDecoder(strings.NewReader(string(fixtureYAML)))
	d.KnownFields(true)
	if err := d.Decode(&f); err != nil {
		return f, err
	}
	seen := map[string]bool{}
	for _, a := range f.Accounts {
		if a.Name == "" || seen[a.Name] {
			return f, errors.New("fixture account names must be unique")
		}
		seen[a.Name] = true
	}
	for _, name := range f.RequiredNames {
		if !seen[name] {
			return f, fmt.Errorf("required fixture account %q missing", name)
		}
	}
	return f, nil
}
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func villageID(raw string) (schema.VillageUUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil || id.String() != raw {
		return "", fmt.Errorf("fixture UUID is not canonical: %q", raw)
	}
	return schema.VillageUUID(id.String()), nil
}
func exportFixtures(f fixtures, output io.Writer) error {
	canonical, err := schema.LoadContentCapabilitySessionGraphFixtures()
	if err != nil {
		return err
	}
	var detail schema.SessionDetailPayload
	found := false
	for _, c := range canonical.Derivation.Cases {
		if c.Name == "legacy-empty" {
			if err := json.Unmarshal([]byte(c.Input.DetailJSON), &detail); err != nil {
				return err
			}
			found = true
		}
	}
	if !found {
		return errors.New("canonical legacy-empty content fixture absent; update the runner fixture binding before pulling")
	}
	detail.ID = f.TranscriptID
	content := schema.TranscriptContent{ContractVersion: defaults.PublishSchemaVersion, Kind: schema.ContentKindSessionDetail, SessionDetail: &detail}
	if err := schema.ValidateTranscriptContent(content); err != nil {
		return err
	}
	blob, err := json.Marshal(content)
	if err != nil {
		return err
	}
	id, err := schema.NewTranscriptID(f.TranscriptID)
	if err != nil {
		return err
	}
	metadata := schema.PullTranscriptInfo{TranscriptID: id, LocalID: id.String(), Owner: schema.NewVillagePublicProfile(schema.VillageUUID(f.Accounts[0].UserID), f.Accounts[0].Username, nil, nil), Harness: detail.Harness, Visibility: schema.VisibilityGroup, License: schema.LicenseCCBYSA, ContentHash: schema.ComputeTranscriptHash(blob), ContractVersion: defaults.PublishSchemaVersion, PublishedAt: f.ObservedAt.UnixMilli(), UpdatedAt: f.ObservedAt.UnixMilli()}
	return json.NewEncoder(output).Encode(struct {
		Fixtures           fixtures                     `json:"fixtures"`
		Content            json.RawMessage              `json:"content"`
		ContentBytesBase64 string                       `json:"contentBytesBase64"`
		Metadata           schema.PullTranscriptInfo    `json:"metadata"`
		Version            schema.SchemaVersionResponse `json:"version"`
		TranscriptFilename string                       `json:"transcriptFilename"`
	}{Fixtures: f, Content: blob, ContentBytesBase64: base64.StdEncoding.EncodeToString(blob), Metadata: metadata, Version: schema.SchemaVersionResponse{AnnotationSchemaVersion: "0.1.0", SupportedTargetKinds: []string{}, SupportedTypeIDs: []string{}, PushContractVersion: defaults.PublishSchemaVersion, MinPushContractVersion: defaults.PublishSchemaVersion, PullContractVersion: defaults.PullContractVersion, MinPullContractVersion: defaults.PullContractVersion}, TranscriptFilename: pull.TranscriptFilename})
}
func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("collectiveobservations-fixture", flag.ContinueOnError)
	root := flags.String("root", "", "test-owned state root with an ownership marker")
	remote := flags.String("village-url", "", "loopback fixture Village URL")
	selected := flags.String("account", "alice", "named fixture account")
	action := flags.String("action", "seed", "seed, diagnostic, receipt, terminal, association-invalidated, snapshot, or fixture")
	if err := flags.Parse(args); err != nil {
		return err
	}
	f, err := loadFixtures()
	if err != nil {
		return err
	}
	if *action == "fixture" {
		return exportFixtures(f, output)
	}
	if *root == "" {
		return errors.New("fixture seeding requires --root; no live Store is selected")
	}
	owner := os.Getenv("PEASANT_OBSERVATIONS_OWNER")
	marker, err := os.ReadFile(filepath.Join(*root, ".collective-observations-fixture"))
	if err != nil || owner == "" || string(marker) != owner {
		return errors.New("fixture ownership marker does not match this process; no credentials or database changed; use the owned journey runner")
	}
	info, err := os.Lstat(*root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("fixture root must be a real owned directory, not a symlink")
	}
	info, err = os.Lstat(filepath.Join(*root, ".collective-observations-fixture"))
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("fixture ownership marker must be a regular file")
	}
	u, err := url.Parse(*remote)
	if err != nil {
		return fmt.Errorf("parse fixture Village origin: %w; no credentials were saved", err)
	}
	loopback := u.Scheme == "http" && u.Hostname() == "127.0.0.1" && u.Port() != ""
	plainOrigin := u.Path == "" && u.RawQuery == "" &&
		u.Fragment == "" && u.User == nil
	if !loopback || !plainOrigin {
		return errors.New("fixture Village must be an explicit http://127.0.0.1:<port> origin; no remote credentials will be saved")
	}
	config, data := filepath.Join(*root, "config"), filepath.Join(*root, "data")
	if *action == "seed" {
		var chosen *account
		for _, a := range f.Accounts {
			if a.Name == *selected {
				value := a
				chosen = &value
			}
		}
		if chosen == nil {
			return fmt.Errorf("unknown fixture account %q; use a required named account", *selected)
		}
		if err := auth.SaveCredentialsFrom(&auth.Credentials{APIKey: "disposable-observations-fixture", KeyID: "fixture-key-id", UserID: chosen.UserID, Username: chosen.Username, VillageURL: *remote, LinkedAt: f.ObservedAt}, config); err != nil {
			return err
		}
	}
	snapshot, err := auth.CaptureCredentials(ctx, config)
	if err != nil {
		return err
	}
	if snapshot.Credentials().VillageURL != *remote {
		return errors.New("saved fixture credentials belong to another Village; refusing to relabel existing scope")
	}
	dbPath := defaults.ResolveDBFilePathWith(data).String()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return err
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	collective, err := villageID(f.CollectiveID)
	if err != nil {
		return err
	}
	transcript, err := schema.NewTranscriptID(f.TranscriptID)
	if err != nil {
		return err
	}
	var issue schema.OperationIssue
	if err := json.Unmarshal([]byte(f.IssueJSON), &issue); err != nil {
		return err
	}
	if err := issue.Validate(); err != nil {
		return err
	}
	if *action == "seed" {
		invocation, err := db.BeginCollectiveObservation(ctx, snapshot)
		if err != nil {
			return err
		}
		var sources []schema.CollectiveFetchSourceResult
		if err := json.Unmarshal([]byte(f.SourceJSON), &sources); err != nil {
			return err
		}
		row := schema.CollectiveObservation{ID: collective, Name: f.OldName, ObservedAt: f.ObservedAt, State: schema.CollectiveObservationStateObserved, Reasons: []schema.CollectiveObservationReason{schema.CollectiveObservationReasonMembership}}
		if _, err := db.CommitCollectiveCatalog(ctx, invocation, []schema.CollectiveObservation{row}, sources, f.ObservedAt, f.ObservedAt); err != nil {
			return err
		}
		var events []schema.VillageShareEvent
		if err := json.Unmarshal([]byte(f.AttemptsJSON), &events); err != nil {
			return err
		}
		pair := store.ContributionAttemptSource{CollectiveID: collective, TranscriptID: transcript, Events: events, ObservedAt: f.ObservedAt}
		var historySources []schema.CollectiveFetchSourceResult
		if err := json.Unmarshal([]byte(f.HistorySourceJSON), &historySources); err != nil {
			return err
		}
		if _, err := db.CommitCollectiveHistory(ctx, invocation, []store.ContributionAttemptSource{pair}, historySources, schema.CollectiveEvidenceCoverageAvailable, f.ObservedAt, f.ObservedAt); err != nil {
			return err
		}
		var associations []schema.VillageTranscriptCollective
		if err := json.Unmarshal([]byte(f.AssociationJSON), &associations); err != nil {
			return err
		}
		if err := db.InstallTranscriptAssociations(ctx, invocation, transcript, associations, f.ObservedAt, false); err != nil {
			return err
		}
		for _, id := range f.ReceiptIDs {
			if err := db.RecordContributionReceipt(ctx, snapshot, schema.LocalConfirmedContributionReceipt{ReceiptID: id, Operation: schema.CollectiveOperationShare, TranscriptID: transcript, CollectiveID: &collective, ConfirmedAt: f.ObservedAt, Outcome: schema.LocalConfirmedContributionSubmitted}); err != nil {
				return err
			}
		}
		for _, id := range f.DiagnosticIDs {
			operationID, err := villageID(id)
			if err != nil {
				return err
			}
			if err := db.RecordOperationDiagnostic(ctx, snapshot, schema.LocalOperationDiagnostic{OperationID: operationID, Operation: schema.CollectiveOperationShare, AttemptedAt: f.ObservedAt, Outcome: schema.LocalOperationDiagnosticUncertain, Issue: issue}); err != nil {
				return err
			}
		}
	} else if *action == "diagnostic" {
		id, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		operationID, err := villageID(id.String())
		if err != nil {
			return err
		}
		if err := db.RecordOperationDiagnostic(ctx, snapshot, schema.LocalOperationDiagnostic{OperationID: operationID, Operation: schema.CollectiveOperationShare, AttemptedAt: f.ObservedAt, Outcome: schema.LocalOperationDiagnosticUncertain, Issue: issue}); err != nil {
			return err
		}
	} else if *action == "receipt" {
		if err := db.RecordContributionReceipt(ctx, snapshot, schema.LocalConfirmedContributionReceipt{ReceiptID: "fixture-new-receipt", Operation: schema.CollectiveOperationShare, TranscriptID: transcript, CollectiveID: &collective, ConfirmedAt: f.ObservedAt, Outcome: schema.LocalConfirmedContributionSubmitted}); err != nil {
			return err
		}
	} else if *action == "association-invalidated" {
		invocation, err := db.BeginCollectiveObservation(ctx, snapshot)
		if err != nil {
			return err
		}
		if err := db.InstallTranscriptAssociations(ctx, invocation, transcript, []schema.VillageTranscriptCollective{}, f.ObservedAt, true); err != nil {
			return err
		}
	} else if *action == "terminal" {
		var terminal schema.VillageShareEvent
		if err := json.Unmarshal([]byte(f.TerminalJSON), &terminal); err != nil {
			return err
		}
		invocation, err := db.BeginCollectiveObservation(ctx, snapshot)
		if err != nil {
			return err
		}
		if _, err := db.InstallContributionAttemptSources(ctx, invocation, []store.ContributionAttemptSource{{CollectiveID: collective, TranscriptID: transcript, Events: []schema.VillageShareEvent{terminal}, ObservedAt: f.ObservedAt}}); err != nil {
			return err
		}
	} else if *action != "snapshot" {
		return fmt.Errorf("unknown fixture action %q; no matching fixture operation exists", *action)
	}
	view, err := db.ReadCollectiveObservations(ctx, snapshot)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(view)
}
