package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// HeldPublishedTranscriptIDs projects existing authoritative receipts for held
// recorded sessions; it creates neither publication nor association evidence.
func (s *Store) HeldPublishedTranscriptIDs(ctx context.Context, scope schema.CollectiveObservationScope) ([]schema.TranscriptID, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	ids := []schema.TranscriptID{}
	seen := map[schema.TranscriptID]bool{}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)
	rows := 0
	err = sqlitex.Execute(conn, `SELECT p.receipt_json FROM session_publications p WHERE p.village_origin=? AND p.owner_user_id=? AND EXISTS(SELECT 1 FROM sessions s WHERE s.session_id=p.session_id) ORDER BY p.session_id,p.project_hash LIMIT ?`, &sqlitex.ExecOptions{Args: []any{scope.VillageURL, scope.UserID.String(), schema.MaxCollectiveFetchItems + 1}, ResultFunc: func(stmt *sqlite.Stmt) error {
		rows++
		if rows > schema.MaxCollectiveFetchItems {
			return fmt.Errorf("held publication source exceeds bounded inventory read; no truncated prefix selected; narrow selected inventory")
		}
		var receipt schema.AuthoritativePublishResponse
		if err := json.Unmarshal([]byte(stmt.ColumnText(0)), &receipt); err != nil {
			return err
		}
		if err := receipt.Validate(); err != nil {
			return err
		}
		if seen[receipt.TranscriptID] {
			return nil
		}
		if len(ids) >= schema.MaxCollectiveFetchItems {
			return fmt.Errorf("held published transcript inventory exceeds metadata bound; no prefix selected; narrow the selected inventory")
		}
		seen[receipt.TranscriptID] = true
		ids = append(ids, receipt.TranscriptID)
		return nil
	}})
	return ids, err
}
