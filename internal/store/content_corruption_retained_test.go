package store

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
)

// Retained non-emitted blobs never enter a full transcript. Verification owns
// their corruption, while authoritative snapshots verify only served bodies.
func TestContentCorruptionRetainedObjects(t *testing.T) {
	for _, c := range loadContentCorruptionCases(t) {
		if c.Owner != "verify" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			verifyOnly := len(c.Surfaces) == 1 && c.Surfaces[0] == "verify"
			if c.Expect != "refuse" || !verifyOnly || c.Why == "" {
				t.Fatalf("invalid retained corruption fixture: %+v", c)
			}
			s, sid, _, _, _, _ := seedCorruptionCandidate(t, c.Damage)
			var before indexformat.ReadSnapshot
			if err := s.WithSessionSnapshot(t.Context(), sid, func(snapshot indexformat.ReadSnapshot) error {
				before = snapshot
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			applyCorruptionDamage(t, s, sid, c.Damage)
			report, err := s.VerifyContent(t.Context(), false)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Damaged) != 1 {
				t.Fatalf("retained corruption damage count: %+v", report)
			}
			damage := report.Damaged[0]
			if damage.SessionID != sid || !strings.HasPrefix(damage.Object, "blob:") || damage.Reason == "" {
				t.Fatalf("retained corruption not attributed: %+v", report)
			}
			if err := s.WithSessionSnapshot(t.Context(), sid, func(snapshot indexformat.ReadSnapshot) error {
				if !snapshot.FullContentVerified || snapshot.GenerationID != before.GenerationID {
					t.Fatal("retained damage changed full snapshot authority")
				}
				for i, entry := range snapshot.Main.Entries {
					if harmonizedContentField(entry) != harmonizedContentField(before.Main.Entries[i]) {
						t.Fatal("non-emitted blob damage changed served bytes")
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("full snapshot read a non-emitted blob: %v", err)
			}
		})
	}
}
