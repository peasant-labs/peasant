package store

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/peasant-labs/peasant/internal/auth"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// CollectiveReadSource identifies an overlapping private source. Empty ID
// covers the source; explicit IDs are collective UUIDs and attempt IDs are
// collectiveUUID/transcriptUUID. These are not wire revisions or permissions.
type CollectiveReadSource struct {
	Source schema.CollectiveObservationSource
	ID     string
}
type CollectiveReadPlan struct {
	Domains     []schema.CollectiveFetchDomain
	Sources     []CollectiveReadSource
	Transcripts []schema.TranscriptID
}

func ContributionAttemptReadSource(collective schema.VillageUUID, transcript schema.TranscriptID) CollectiveReadSource {
	return CollectiveReadSource{Source: schema.CollectiveObservationSourceAttempts, ID: collective.String() + "/" + transcript.String()}
}

// IssueCollectiveReads durably fences overlapping older responses BEFORE I/O.
// Issuance never changes good payloads, completion dates, or evidence tokens.
func (s *Store) IssueCollectiveReads(ctx context.Context, inv CollectiveObservationInvocation, plan CollectiveReadPlan, at time.Time) error {
	if inv.store != s || inv.sequence < 1 {
		return fmt.Errorf("issue collective reads: invocation belongs to another Store; no request issued; allocate an invocation in the selected Store")
	}
	return s.withObservationTransaction(ctx, inv.credentials, true, func(conn *sqlite.Conn) error { return issueCollectiveReads(conn, inv, plan, at) })
}

// BeginCollectiveRead allocates ordering and requested fences in one commit.
func (s *Store) BeginCollectiveRead(ctx context.Context, snapshot auth.CredentialSnapshot, plan CollectiveReadPlan, at time.Time) (CollectiveObservationInvocation, error) {
	return s.beginCollectiveObservation(ctx, snapshot, func(conn *sqlite.Conn, inv CollectiveObservationInvocation) error {
		return issueCollectiveReads(conn, inv, plan, at)
	})
}

func issueCollectiveReads(conn *sqlite.Conn, inv CollectiveObservationInvocation, plan CollectiveReadPlan, at time.Time) error {
	if at.IsZero() || at.Location() != time.UTC {
		return fmt.Errorf("issue collective reads: issuance date must be actual UTC; no request issued; capture the operation clock in UTC")
	}
	sources := append([]CollectiveReadSource{}, plan.Sources...)
	for _, domain := range plan.Domains {
		if !domain.IsValid() {
			return fmt.Errorf("issue collective reads: unknown domain; no request issued; use the schema domain menu")
		}
		switch domain {
		case schema.CollectiveFetchDomainCatalog:
			for _, source := range []schema.CollectiveObservationSource{schema.CollectiveObservationSourceMembership, schema.CollectiveObservationSourceContributionDiscovery, schema.CollectiveObservationSourceOrganization, schema.CollectiveObservationSourceExplicit} {
				sources = append(sources, CollectiveReadSource{Source: source})
			}
		case schema.CollectiveFetchDomainHistory:
			sources = append(sources, CollectiveReadSource{Source: schema.CollectiveObservationSourceAttempts})
		}
	}
	if len(sources)+len(plan.Transcripts) > schema.MaxCollectiveFetchItems {
		return fmt.Errorf("issue collective reads: source plan exceeds metadata bound; no reads issued; narrow the requested source set")
	}
	for _, source := range sources {
		if !source.Source.IsValid() {
			return fmt.Errorf("issue collective reads: unknown source; no request issued; use the schema source menu")
		}
		if source.ID != "" {
			parts := strings.Split(source.ID, "/")
			if source.Source == schema.CollectiveObservationSourceAttempts {
				if len(parts) != 2 {
					return fmt.Errorf("issue attempt read: expected collective/transcript UUIDs; no request issued")
				}
			} else if source.Source != schema.CollectiveObservationSourceExplicit || len(parts) != 1 {
				return fmt.Errorf("issue scoped read: unsupported scoped source; no request issued")
			}
			for _, part := range parts {
				if _, err := schema.NewTranscriptID(part); err != nil {
					return err
				}
			}
		}
	}
	for _, id := range plan.Transcripts {
		if _, err := schema.NewTranscriptID(id.String()); err != nil {
			return err
		}
	}
	if err := checkObservationInvocation(conn, inv); err != nil {
		return err
	}
	for _, domain := range plan.Domains {
		_, installed, issued, err := readCollectiveDomain(conn, inv, domain)
		if err != nil {
			return err
		}
		if installed > inv.sequence || issued > inv.sequence {
			return ErrObservationSuperseded
		}
	}
	for _, source := range sources {
		if err := checkCollectiveSourceSequence(conn, inv, source.Source, source.ID); err != nil {
			return err
		}
	}
	for _, id := range plan.Transcripts {
		if err := checkTranscriptReadSequence(conn, inv, id); err != nil {
			return err
		}
	}
	unknown, _ := json.Marshal(schema.CollectiveFetchDomainSummary{State: schema.CollectiveFetchStateNotRequested, Coverage: schema.CollectiveEvidenceCoverageUnknown})
	for _, domain := range plan.Domains {
		if err := sqlitex.Execute(conn, `INSERT INTO collective_domain_observations(village_url,user_id,domain,attempt_seq,summary_json) VALUES(?,?,?,?,?) ON CONFLICT(village_url,user_id,domain) DO UPDATE SET attempt_seq=max(attempt_seq,excluded.attempt_seq)`, &sqlitex.ExecOptions{Args: []any{inv.villageURL, inv.userID, string(domain), inv.sequence, string(unknown)}}); err != nil {
			return err
		}
	}
	for _, source := range sources {
		if err := sqlitex.Execute(conn, `INSERT INTO collective_source_installs(village_url,user_id,source,source_id,installed_seq,issued_seq) VALUES(?,?,?,?,0,?) ON CONFLICT(village_url,user_id,source,source_id) DO UPDATE SET issued_seq=max(issued_seq,excluded.issued_seq)`, &sqlitex.ExecOptions{Args: []any{inv.villageURL, inv.userID, string(source.Source), source.ID, inv.sequence}}); err != nil {
			return err
		}
	}
	for _, id := range plan.Transcripts {
		if err := sqlitex.Execute(conn, `INSERT INTO transcript_collective_observations(village_url,user_id,transcript_id,attempt_seq,last_attempt_at) VALUES(?,?,?,?,?) ON CONFLICT(village_url,user_id,transcript_id) DO UPDATE SET attempt_seq=max(attempt_seq,excluded.attempt_seq),last_attempt_at=excluded.last_attempt_at`, &sqlitex.ExecOptions{Args: []any{inv.villageURL, inv.userID, id.String(), inv.sequence, at.Format(observationOrderedTimeLayout)}}); err != nil {
			return err
		}
	}
	return nil
}

func checkTranscriptReadSequence(conn *sqlite.Conn, inv CollectiveObservationInvocation, id schema.TranscriptID) error {
	var latest int64
	if err := sqlitex.Execute(conn, `SELECT max(installed_seq,attempt_seq) FROM transcript_collective_observations WHERE village_url=? AND user_id=? AND transcript_id=?`, &sqlitex.ExecOptions{Args: []any{inv.villageURL, inv.userID, id.String()}, ResultFunc: func(stmt *sqlite.Stmt) error { latest = stmt.ColumnInt64(0); return nil }}); err != nil {
		return err
	}
	if latest > inv.sequence {
		return ErrObservationSuperseded
	}
	return nil
}
