package ingest

import (
	_ "embed"
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/index_lane_profile.yaml
var indexLaneProfileYAML []byte

func TestIndexLaneProfile(t *testing.T) {
	var fixture struct {
		Waits     []int       `yaml:"waits_ms"`
		Sizes     []int       `yaml:"activation_sizes"`
		Peaks     []int64     `yaml:"staged_peaks"`
		Count     int         `yaml:"expected_wait_count"`
		P50       int         `yaml:"expected_p50_ms"`
		Max       int         `yaml:"expected_max_ms"`
		Histogram map[int]int `yaml:"expected_histogram"`
		Peak      int64       `yaml:"expected_peak"`
	}
	if err := yaml.Unmarshal(indexLaneProfileYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	p := &IndexProfiler{}
	for _, wait := range fixture.Waits {
		p.RecordFlushWait(time.Duration(wait) * time.Millisecond)
	}
	for _, size := range fixture.Sizes {
		p.RecordActivationSize(size)
	}
	for _, peak := range fixture.Peaks {
		p.RecordStagedPeak(peak)
	}
	snapshot := p.Snapshot()
	if snapshot.FlushWaitCount != fixture.Count || snapshot.FlushWaitP50 != time.Duration(fixture.P50)*time.Millisecond || snapshot.FlushWaitMax != time.Duration(fixture.Max)*time.Millisecond {
		t.Fatalf("partial-batch wait profile differs from fixture: %+v", snapshot)
	}
	if snapshot.StagedPeakBytes != fixture.Peak || !reflect.DeepEqual(snapshot.ActivationSizes, fixture.Histogram) {
		t.Fatalf("write-lane profile differs from fixture: %+v", snapshot)
	}
	clear(snapshot.ActivationSizes)
	if !reflect.DeepEqual(p.Snapshot().ActivationSizes, fixture.Histogram) {
		t.Fatal("snapshot mutation changed the recorded histogram")
	}
}
