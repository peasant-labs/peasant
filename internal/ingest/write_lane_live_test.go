package ingest

import (
	_ "embed"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/write_lane_live.yaml
var writeLaneLiveYAML []byte

func TestWriteLaneLiveSnapshots(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name          string `yaml:"name"`
			QueuedAt      int64  `yaml:"queuedAt"`
			StartedAt     int64  `yaml:"startedAt"`
			SampleAt      int64  `yaml:"sampleAt"`
			FinishedAt    int64  `yaml:"finishedAt"`
			WantBusyMs    int64  `yaml:"wantBusyMs"`
			WantElapsedMs int64  `yaml:"wantElapsedMs"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(writeLaneLiveYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if seen[row.Name] {
			t.Fatalf("duplicate case %q", row.Name)
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			recorder := &writeLaneRecorder{startedAt: time.UnixMilli(row.QueuedAt)}
			readings := []int64{row.QueuedAt, row.StartedAt, row.FinishedAt}
			clock := func() time.Time {
				at := time.UnixMilli(readings[0])
				readings = readings[1:]
				return at
			}
			recorder.run(StageIndex, clock, func(job func()) { job() }, func() {
				got := recorder.progressAt(time.UnixMilli(row.SampleAt))
				if got.Queued != 0 || got.Busy.Milliseconds() != row.WantBusyMs || got.Elapsed.Milliseconds() != row.WantElapsedMs {
					t.Fatalf("running snapshot = %+v", got)
				}
			})
			got := recorder.progressAt(time.UnixMilli(row.FinishedAt))
			if got.Busy.Milliseconds() != row.FinishedAt-row.StartedAt {
				t.Fatalf("finished job counted twice or lost: %+v", got)
			}
		})
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing required case %q", name)
		}
	}
}

func TestWriteLaneProgressConcurrentSnapshot(t *testing.T) {
	recorder := &writeLaneRecorder{startedAt: time.Now()}
	progress := NewProgressState()
	progress.setWriteLane(recorder)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		recorder.run(StageIndex, time.Now, func(job func()) { job() }, func() {
			close(started)
			<-release
		})
		close(done)
	}()
	testwait.Receive(t, started, "writer job started")
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 100 {
				if snapshot, ok := progress.WriteLaneSnapshot(); !ok || snapshot.Queued != 0 || snapshot.Busy < 0 || snapshot.Elapsed < 0 {
					t.Errorf("invalid live snapshot: %+v, available=%t", snapshot, ok)
				}
			}
		}()
	}
	close(release)
	readers.Wait()
	testwait.Receive(t, done, "writer job completed")
	progress.Reset()
	if _, ok := progress.WriteLaneSnapshot(); ok {
		t.Fatal("a reset retained the previous run's lane")
	}
}
