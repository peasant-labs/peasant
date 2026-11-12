package ingest

import (
	_ "embed"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/write_lane_stats.yaml
var writeLaneStatsFixtureData []byte

type writeLaneStatsCase struct {
	Name        string `yaml:"name"`
	NilRecorder bool   `yaml:"nilRecorder"`
	Jobs        []struct {
		Stage      string `yaml:"stage"`
		QueuedAt   int64  `yaml:"queuedAt"`
		StartedAt  int64  `yaml:"startedAt"`
		FinishedAt int64  `yaml:"finishedAt"`
	} `yaml:"jobs"`
	Want struct {
		Jobs   int64 `yaml:"jobs"`
		WaitMs int64 `yaml:"waitMs"`
		BusyMs int64 `yaml:"busyMs"`
	} `yaml:"want"`
	WantStageWaitMs map[string]int64 `yaml:"wantStageWaitMs"`
}

func LoadWriteLaneStatsFixtures(t *testing.T) []writeLaneStatsCase {
	t.Helper()
	var fixture struct {
		RequiredNames []string             `yaml:"requiredNames"`
		Cases         []writeLaneStatsCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(writeLaneStatsFixtureData, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, row := range fixture.Cases {
		if seen[row.Name] {
			t.Fatalf("write_lane_stats.yaml repeats case %s", row.Name)
		}
		seen[row.Name] = true
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("write_lane_stats.yaml declares no required names")
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("write_lane_stats.yaml is missing required case %s", name)
		}
	}
	return fixture.Cases
}

func TestWriteLaneStats(t *testing.T) {
	for _, row := range LoadWriteLaneStatsFixtures(t) {
		t.Run(row.Name, func(t *testing.T) {
			var recorder *writeLaneRecorder
			if !row.NilRecorder {
				recorder = &writeLaneRecorder{}
			}
			ran := 0
			for _, job := range row.Jobs {
				stage, err := NewStage(job.Stage)
				if err != nil {
					t.Fatal(err)
				}
				// The recorder reads the clock three times per job: queued,
				// started, finished.
				readings := []int64{job.QueuedAt, job.StartedAt, job.FinishedAt}
				clock := func() time.Time {
					if len(readings) == 0 {
						t.Fatal("the recorder read the clock more than three times for one job")
					}
					at := time.UnixMilli(readings[0])
					readings = readings[1:]
					return at
				}
				recorder.run(stage, clock, func(run func()) { run() }, func() {
					ran++
					if waiting := recorder.Snapshot().Queued; waiting != 0 {
						t.Fatalf("a running job counts as %d waiting", waiting)
					}
				})
			}
			if ran != len(row.Jobs) {
				t.Fatalf("%d of %d jobs ran", ran, len(row.Jobs))
			}
			got := recorder.Snapshot()
			if got.Jobs != row.Want.Jobs || got.WaitTotal.Milliseconds() != row.Want.WaitMs || got.BusyTotal.Milliseconds() != row.Want.BusyMs || got.Queued != 0 {
				t.Fatalf("lane stats = %+v, want %+v with none waiting", got, row.Want)
			}
			for name, want := range row.WantStageWaitMs {
				stage, err := NewStage(name)
				if err != nil {
					t.Fatal(err)
				}
				if wait := recorder.stageWait(stage).Milliseconds(); wait != want {
					t.Fatalf("stage %s lane wait = %dms, want %dms", stage, wait, want)
				}
			}
		})
	}
}

// TestWriteLaneReportsWaitingJobs holds jobs behind a lane that has not
// started them: the snapshot counts exactly the jobs still waiting, and a job
// stops counting when the lane starts it.
func TestWriteLaneReportsWaitingJobs(t *testing.T) {
	recorder := &writeLaneRecorder{}
	var held []func()
	hold := func(job func()) { held = append(held, job) }
	clock := func() time.Time { return time.UnixMilli(0) }
	for range 3 {
		recorder.run(StageIndex, clock, hold, func() {})
	}
	for started, job := range held {
		if waiting := recorder.Snapshot().Queued; waiting != len(held)-started {
			t.Fatalf("waiting jobs = %d, want %d", waiting, len(held)-started)
		}
		job()
	}
	if got := recorder.Snapshot(); got.Queued != 0 || got.Jobs != 3 {
		t.Fatalf("after the lane drained: %+v", got)
	}
}
