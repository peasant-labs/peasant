package sourcepreview_test

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/sourcepreview"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/budget.yaml
var budgetFixtureData []byte

// budgetFixturePath names the embedded case file in failure messages.
const budgetFixturePath = "testdata/budget.yaml"

type budgetFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Budgets       []struct {
		Name  string `yaml:"name"`
		Value int64  `yaml:"value"`
	} `yaml:"budgets"`
	UnboundedOptions struct {
		SourceCapBytes int64 `yaml:"sourceCapBytes"`
		TurnCap        int   `yaml:"turnCap"`
	} `yaml:"unboundedOptions"`
}

// productionBudgets maps each fixture name to the constant production passes.
func productionBudgets() map[string]int64 {
	return map[string]int64{
		"first-read-bytes":         defaults.TranscriptInitialReadBytes,
		"continuation-read-bytes":  defaults.TranscriptContinuationReadBytes,
		"segment-budget-bytes":     schema.TranscriptSegmentBudgetBytes,
		"frame-cap-bytes":          defaults.KickstartWebPreviewFrameCapBytes,
		"web-source-cap-bytes":     defaults.KickstartWebPreviewSourceCapBytes,
		"web-serialized-cap-bytes": defaults.KickstartWebPreviewSerializedCapBytes,
		"web-turn-cap":             defaults.KickstartWebPreviewTurnCap,
		"web-live-captures":        defaults.KickstartWebPreviewLiveCaptures,
		"web-parse-workers":        defaults.KickstartWebPreviewParseWorkers,
		"web-idle-seconds":         int64(defaults.KickstartWebPreviewIdle.Seconds()),
	}
}

func TestBudgets(t *testing.T) {
	t.Parallel()
	var fixture budgetFixture
	if err := testutil.DecodeFixtureYAML(budgetFixtureData, &fixture); err != nil {
		t.Fatalf("decode %s: %v", budgetFixturePath, err)
	}
	production := productionBudgets()
	stated := map[string]bool{}
	for _, budget := range fixture.Budgets {
		value, ok := production[budget.Name]
		if !ok {
			t.Fatalf("budget %q has no production constant; name the constant in productionBudgets", budget.Name)
		}
		if value != budget.Value {
			t.Fatalf("budget %q is %d in production but the fixture states %d; change both only as a deliberate budget change", budget.Name, value, budget.Value)
		}
		stated[budget.Name] = true
	}
	for _, name := range fixture.RequiredNames {
		if !stated[name] {
			t.Fatalf("budget fixture is missing required budget %q", name)
		}
	}

	web := sourcepreview.CaptureOptions{FirstReadBytes: defaults.TranscriptInitialReadBytes, SliceReadBytes: defaults.TranscriptContinuationReadBytes,
		SourceCapBytes: defaults.KickstartWebPreviewSourceCapBytes, TurnCap: defaults.KickstartWebPreviewTurnCap}
	unbounded := sourcepreview.CaptureOptions{FirstReadBytes: defaults.TranscriptInitialReadBytes, SliceReadBytes: defaults.TranscriptContinuationReadBytes,
		SourceCapBytes: fixture.UnboundedOptions.SourceCapBytes, TurnCap: fixture.UnboundedOptions.TurnCap}
	for _, opts := range []sourcepreview.CaptureOptions{web, unbounded} {
		if err := opts.Validate(); err != nil {
			t.Fatalf("options %+v do not validate: %v", opts, err)
		}
	}
	if unbounded.SourceCapBytes != 0 || unbounded.TurnCap != 0 || unbounded == web {
		t.Fatalf("the unbounded options %+v must keep both caps zero and differ from the web's %+v", unbounded, web)
	}
	segments := sourcepreview.SegmenterOptions{BudgetBytes: schema.TranscriptSegmentBudgetBytes, FrameCapBytes: defaults.KickstartWebPreviewFrameCapBytes,
		SerializedCapBytes: defaults.KickstartWebPreviewSerializedCapBytes}
	if err := segments.Validate(); err != nil {
		t.Fatalf("the web segmenter options do not validate: %v", err)
	}
}
