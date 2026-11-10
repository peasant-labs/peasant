package ingest

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/attachments"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// TestAssignImagePartitionsKeepsSameIndexHistory proves two images at the same
// entry index, one in main and one in earlier history, keep distinct partitions.
// A skipped activation is not an installing disposition, so it cannot write rows.
func TestAssignImagePartitionsKeepsSameIndexHistory(t *testing.T) {
	t.Parallel()
	if activationInstallsImages(ActivationSkipped) || activationInstallsImages(ActivationLatestAttemptPreview) || activationInstallsImages(ActivationNotCommitted) {
		t.Fatal("a skipped, held, or rejected attempt would install image rows")
	}
	if !activationInstallsImages(ActivationCommittedNow) || !activationInstallsImages(ActivationAlreadyCommitted) {
		t.Fatal("an accepted generation would not install image rows")
	}
	mainRef := "img-" + contentAddress([]byte("png"))
	earlierRef := "img-" + contentAddress([]byte("earlier"))
	mainExtra := `{"ref":"` + mainRef + `"}`
	earlierExtra := `{"ref":"` + earlierRef + `"}`
	pending := []attachments.Pending{
		{Row: attachments.Row{SourceEntryRef: mainRef}},
		{Row: attachments.Row{SourceEntryRef: earlierRef}},
	}
	assignImagePartitions(&GenerationCandidate{Result: indexformat.V2{Generation: indexformat.Generation{
		Main: indexformat.Partition{Entries: []schema.SessionEntry{{
			EntryIndex: 1, EntryType: schema.EntryTypeImage, Extra: &mainExtra,
		}}},
		Earlier: []indexformat.EarlierPartition{{
			State: schema.EarlierHistoryUncertainMigrated,
			Content: indexformat.Partition{Entries: []schema.SessionEntry{{
				EntryIndex: 1, EntryType: schema.EntryTypeImage, Extra: &earlierExtra,
			}}},
		}},
	}}}, pending)
	if pending[0].Row.PartitionID != "main" || pending[1].Row.PartitionID != string(schema.EarlierHistoryUncertainMigrated) {
		t.Fatalf("partitions = %q %q, want main and the earlier-history state", pending[0].Row.PartitionID, pending[1].Row.PartitionID)
	}
}
