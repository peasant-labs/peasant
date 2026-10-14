package push_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
)

// TestValidatePublicationInputReportsLatestAttemptCoexistence pins the
// actionable refusal a latest attempt produces. The generic not-ready
// text would hide that the session is readable and publishable as last-good,
// so the coexistence reason must win and must name the way out.
func TestValidatePublicationInputReportsLatestAttemptCoexistence(t *testing.T) {
	bundle := ingest.PublicationInputBundle{
		Readiness:            ingest.PublicationNeedsIngest,
		LatestAttemptPending: true,
	}
	err := push.ValidatePublicationInput(bundle)
	if !errors.Is(err, push.ErrMetadataMissing) {
		t.Fatalf("error = %v, want the metadata-missing category", err)
	}
	for _, want := range []string{
		"latest capture is an incomplete preview",
		"re-index the complete source",
		"publish the last-good explicitly with --last-good",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "positional omission placeholders") {
		t.Fatalf("the latest-attempt refusal fell back to the generic not-ready text: %v", err)
	}
	// The ordinary refusal is unchanged for a not-ready bundle that retains
	// nothing, so the coexistence reason never masks it.
	generic := push.ValidatePublicationInput(ingest.PublicationInputBundle{Readiness: ingest.PublicationNeedsIngest})
	if generic == nil || !strings.Contains(generic.Error(), "positional omission placeholders") {
		t.Fatalf("generic refusal = %v, want the unchanged not-ready text", generic)
	}
}
