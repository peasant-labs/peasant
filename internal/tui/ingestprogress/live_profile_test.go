package ingestprogress_test

import (
	_ "embed"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/harvestprogress"
	"github.com/peasant-labs/peasant/internal/tui/ingestprogress"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/live_profile.yaml
var liveProfileYAML []byte

type liveLaneSource struct {
	*ingest.ProgressState
	lane      ingest.WriteLaneProgress
	available bool
}

func (s *liveLaneSource) WriteLaneSnapshot() (ingest.WriteLaneProgress, bool) {
	return s.lane, s.available
}

func TestMountedHarvestLaneAndCensus(t *testing.T) {
	var fixture struct {
		RequiredLaneCases []string `yaml:"requiredLaneCases"`
		LaneCases         []struct {
			Name            string   `yaml:"name"`
			Available       bool     `yaml:"available"`
			CandidatesKnown bool     `yaml:"candidatesKnown"`
			Candidates      int      `yaml:"candidates"`
			FinalCanceled   bool     `yaml:"finalCanceled"`
			Queued          int      `yaml:"queued"`
			BusyMs          int64    `yaml:"busyMs"`
			ElapsedMs       int64    `yaml:"elapsedMs"`
			Contains        []string `yaml:"contains"`
			Absent          []string `yaml:"absent"`
		} `yaml:"laneCases"`
		RequiredCensusCases []string `yaml:"requiredCensusCases"`
		CensusCases         []struct {
			Name      string `yaml:"name"`
			Available bool   `yaml:"available"`
			Rows      []struct {
				Reason       string `yaml:"reason"`
				Sessions     int    `yaml:"sessions"`
				IndexedBytes int64  `yaml:"indexedBytes"`
			} `yaml:"rows"`
			Contains []string `yaml:"contains"`
			Absent   []string `yaml:"absent"`
		} `yaml:"censusCases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(liveProfileYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	laneNames := make(map[string]bool)
	for _, row := range fixture.LaneCases {
		if len(row.Contains) == 0 {
			t.Fatalf("lane case %q has no contains assertions", row.Name)
		}
		if laneNames[row.Name] {
			t.Fatalf("duplicate lane case %q", row.Name)
		}
		laneNames[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			source := &liveLaneSource{ProgressState: ingest.NewProgressState(), available: row.Available,
				lane: ingest.WriteLaneProgress{Queued: row.Queued, Busy: time.Duration(row.BusyMs) * time.Millisecond, Elapsed: time.Duration(row.ElapsedMs) * time.Millisecond}}
			if row.CandidatesKnown && !row.FinalCanceled {
				source.Update(ingest.ProgressEvent{Kind: ingest.KindCandidates, Stage: ingest.StageExtract, Total: row.Candidates})
			}
			source.Update(ingest.ProgressEvent{Kind: ingest.KindStart, Stage: ingest.StageFilter, Total: 2})
			now := time.Unix(100, 0)
			model := harvestprogress.New(harvestprogress.Options{Progress: source, Theme: theme.New(theme.ModeDark), StartedAt: now})
			updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			if row.FinalCanceled {
				source.Update(ingest.ProgressEvent{Kind: ingest.KindEnd, Stage: ingest.StageFilter, Done: 2, Total: 2})
				if row.CandidatesKnown {
					source.Update(ingest.ProgressEvent{Kind: ingest.KindCandidates, Stage: ingest.StageExtract, Total: row.Candidates})
					source.Update(ingest.ProgressEvent{Kind: ingest.KindStart, Stage: ingest.StageExtract, Total: row.Candidates})
				}
				updated, _ = updated.Update(ingestprogress.FinalMsg{At: now.Add(time.Second), Snapshot: source.Snapshot(), Outcome: ingestprogress.FinalCanceled})
			}
			view := ansi.Strip(updated.View().Content)
			assertLiveProfileText(t, view, row.Contains, row.Absent)
		})
	}
	for _, name := range fixture.RequiredLaneCases {
		if !laneNames[name] {
			t.Fatalf("missing lane case %q", name)
		}
	}
	censusNames := make(map[string]bool)
	for _, row := range fixture.CensusCases {
		if len(row.Contains) == 0 {
			t.Fatalf("census case %q has no contains assertions", row.Name)
		}
		if censusNames[row.Name] {
			t.Fatalf("duplicate census case %q", row.Name)
		}
		censusNames[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			var census *ingest.RefreshCensus
			if row.Available {
				census = &ingest.RefreshCensus{}
				for _, input := range row.Rows {
					reason, err := ingest.NewOutcomeReason(input.Reason)
					if err != nil {
						t.Fatal(err)
					}
					census.Rows = append(census.Rows, ingest.RefreshCensusRow{Reason: reason, Lane: reason.RefreshLane(), Sessions: input.Sessions, SourceBytes: input.IndexedBytes})
				}
			}
			assertLiveProfileText(t, strings.Join(ingest.CensusLines(census), "\n"), row.Contains, row.Absent)
		})
	}
	for _, name := range fixture.RequiredCensusCases {
		if !censusNames[name] {
			t.Fatalf("missing census case %q", name)
		}
	}
}

func assertLiveProfileText(t *testing.T, got string, contains, absent []string) {
	t.Helper()
	for _, want := range contains {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	for _, unwanted := range absent {
		if strings.Contains(got, unwanted) {
			t.Errorf("unexpected %q in %q", unwanted, got)
		}
	}
}
