package main

import (
	_ "embed"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/time_filter.yaml
var timeFilterYAML []byte

//go:embed testdata/time_filter.manifest.yaml
var timeFilterManifestYAML []byte

type timeFilterAccepted struct {
	Name    string `yaml:"name"`
	Input   string `yaml:"input"`
	MinDays int    `yaml:"minDays"`
	MaxDays int    `yaml:"maxDays"`
}

type timeFilterRefused struct {
	Name      string `yaml:"name"`
	Input     string `yaml:"input"`
	WantError string `yaml:"wantError"`
}

type timeFilterFixture struct {
	Accepted  []timeFilterAccepted `yaml:"accepted"`
	Refused   []timeFilterRefused  `yaml:"refused"`
	PruneOnly []timeFilterRefused  `yaml:"pruneOnly"`
}

func loadTimeFilterFixture(t *testing.T) timeFilterFixture {
	t.Helper()
	var fixture timeFilterFixture
	if err := yaml.Unmarshal(timeFilterYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Required []string `yaml:"required"`
	}
	if err := yaml.Unmarshal(timeFilterManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, c := range fixture.Accepted {
		present[c.Name] = true
	}
	for _, c := range append(slices.Clone(fixture.Refused), fixture.PruneOnly...) {
		present[c.Name] = true
	}
	for _, name := range manifest.Required {
		if !present[name] {
			t.Errorf("time_filter.yaml lost the required case %q", name)
		}
	}
	return fixture
}

// TestTimeFiltersShareOneGrammar proves harvest --since, sessions list
// --since and prune --before/--after read the same grammar and refuse the
// same inputs, through the functions those commands call.
func TestTimeFiltersShareOneGrammar(t *testing.T) {
	t.Parallel()
	fixture := loadTimeFilterFixture(t)
	daysAgo := func(ms int64) int { return int(time.Since(time.UnixMilli(ms)).Hours() / 24) }
	for _, tc := range fixture.Accepted {
		h, err := parseSinceDuration(tc.Input)
		if err != nil {
			t.Errorf("%s: harvest --since %s: %v", tc.Name, tc.Input, err)
		} else if d := daysAgo(h.UnixMilli()); d < tc.MinDays || d > tc.MaxDays {
			t.Errorf("%s: harvest --since %s: %d days ago, want %d-%d", tc.Name, tc.Input, d, tc.MinDays, tc.MaxDays)
		}
		s, err := parseDateOrDuration(tc.Input, true)
		if err != nil {
			t.Errorf("%s: sessions list --since %s: %v", tc.Name, tc.Input, err)
		} else if d := daysAgo(s); d < tc.MinDays || d > tc.MaxDays {
			t.Errorf("%s: sessions list --since %s: %d days ago, want %d-%d", tc.Name, tc.Input, d, tc.MinDays, tc.MaxDays)
		}
		f, err := buildPruneFilter(nil, "", "", tc.Input, tc.Input, false)
		if err != nil {
			t.Errorf("%s: prune --before/--after %s: %v", tc.Name, tc.Input, err)
			continue
		}
		for name, v := range map[string]*int64{"before": f.Before, "after": f.After} {
			if v == nil {
				t.Errorf("%s: prune --%s %s: unset", tc.Name, name, tc.Input)
			} else if d := daysAgo(*v); d < tc.MinDays || d > tc.MaxDays {
				t.Errorf("%s: prune --%s %s: %d days ago, want %d-%d", tc.Name, name, tc.Input, d, tc.MinDays, tc.MaxDays)
			}
		}
	}
	for _, tc := range fixture.Refused {
		_, harvestErr := parseSinceDuration(tc.Input)
		_, listErr := parseDateOrDuration(tc.Input, true)
		_, pruneErr := buildPruneFilter(nil, "", "", tc.Input, "", false)
		for name, err := range map[string]error{"harvest": harvestErr, "sessions list": listErr, "prune": pruneErr} {
			if err == nil || !strings.Contains(err.Error(), tc.WantError) {
				t.Errorf("%s: %s %q: err = %v, want it to name %q", tc.Name, name, tc.Input, err, tc.WantError)
			}
		}
	}
	for _, tc := range fixture.PruneOnly {
		if _, err := buildPruneFilter(nil, "", "", tc.Input, "", false); err == nil || !strings.Contains(err.Error(), tc.WantError) {
			t.Errorf("%s: prune %q: err = %v, want it to name %q", tc.Name, tc.Input, err, tc.WantError)
		}
	}
	h, err := parseSinceDuration("2026-01-02")
	if err != nil || h.Format("2006-01-02") != "2026-01-02" {
		t.Errorf("harvest --since 2026-01-02 = %v, %v", h, err)
	}
	f, err := buildPruneFilter(nil, "", "", "2026-01-02", "", false)
	if err != nil || time.UnixMilli(*f.Before).UTC().Format("2006-01-02") != "2026-01-02" {
		t.Errorf("prune --before 2026-01-02: %v", err)
	}
}

// TestPruneCmdAcceptsRelativeBefore runs the real prune command with the
// relative form harvest documents.
func TestPruneCmdAcceptsRelativeBefore(t *testing.T) {
	t.Parallel()
	if _, _, err := executePruneCmd(t, t.TempDir(), []string{"--before", "7d", "--dry-run"}); err != nil {
		t.Fatalf("prune --before 7d --dry-run: %v", err)
	}
}

// TestPruneRefusesTheAmbiguousMonthUnit proves the destructive prune command
// refuses a bare "m" (read by people as minutes, by the parser as months)
// with a hint, and takes the explicit "mo".
func TestPruneRefusesTheAmbiguousMonthUnit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedPruneTestSessions(t, dir)
	for _, flag := range []string{"--after", "--before"} {
		out, _, err := executePruneCmd(t, dir, []string{flag, "30m", "--dry-run"})
		if err == nil || !strings.Contains(err.Error(), "30mo") || strings.Contains(out, "session(s) would be deleted") {
			t.Fatalf("prune %s 30m: err %v, want a refusal naming 30mo and no selection:\n%s", flag, err, out)
		}
	}
	// Outside prune a bare m keeps meaning months (a maintainer decision).
	if s, err := parseDateOrDuration("3m", true); err != nil || time.Since(time.UnixMilli(s)).Hours()/24 < 88 {
		t.Fatalf("sessions list --since 3m: %v, want about three months ago", err)
	}
	if _, _, err := executePruneCmd(t, dir, []string{"--after", "30mo", "--dry-run"}); err != nil {
		t.Fatalf("prune --after 30mo: %v", err)
	}
}
