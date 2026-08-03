package push_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
)

// TestRepositoryScope_DescribeCapsTheEnumeration holds the size of the line a
// hook prints on every commit and every push.
//
// The enumeration was uncapped: on a monorepo with 150 recorded subdirectories
// it produced one 4,309-character line, printed by every commit, which also
// dumped the user's directory layout into terminals and CI logs. The count still
// has to be exact — that is the fact a reader needs — and enough examples to
// recognise the scope.
func TestRepositoryScope_DescribeCapsTheEnumeration(t *testing.T) {
	t.Parallel()
	const recorded = 150
	admitted := make([]push.RecordedUnderRoot, 0, recorded)
	for i := range recorded {
		admitted = append(admitted, push.RecordedUnderRoot{
			Hash:      ingest.ProjectHash(fmt.Sprintf("%064x", i+1)),
			Directory: fmt.Sprintf("/repo/in-scope/services/svc-%03d", i),
		})
	}
	scope := push.NewRepositoryScope(
		"/repo/in-scope", ingest.ProjectHash(scopedProjectHash), push.IdentityFromPath, admitted, nil)

	described := scope.Describe()
	if !strings.Contains(described, fmt.Sprintf("%d directory identities", recorded)) {
		t.Errorf("the exact count must survive the cap; got: %s", described)
	}
	if !strings.Contains(described, "and 147 more") {
		t.Errorf("the remainder must be summarised rather than listed; got: %s", described)
	}
	if strings.Contains(described, "svc-149") {
		t.Errorf("the enumeration must stop at the cap; got: %s", described)
	}
	// A monorepo's scope line has to stay something a person can read in a
	// terminal, not a paragraph of paths.
	if len(described) > 600 {
		t.Errorf("the scope line is %d bytes, which is what a hook prints on every commit:\n%s", len(described), described)
	}

	// Summary is what --quiet prints, so it must name the repository and never
	// enumerate anything.
	summary := scope.Summary()
	if !strings.Contains(summary, "/repo/in-scope") {
		t.Errorf("the summary must name the repository; got: %s", summary)
	}
	if strings.Contains(summary, "svc-") {
		t.Errorf("the summary must not enumerate directories; got: %s", summary)
	}
}
