package store_test

import (
	"crypto/sha256"
	_ "embed"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/sandbox_measure.yaml
var sandboxDetailYAML []byte

// Detail lives in the external package so it executes the real transcript
// builder, which itself imports store. All reads are read-only on a sandbox.
func TestSandboxDetailLatencyBands(t *testing.T) {
	path := os.Getenv("PEASANT_SANDBOX_DB")
	if path == "" {
		t.Skip("sandbox measurement only: set PEASANT_SANDBOX_DB to a copy under /tmp/opencode")
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel("/tmp/opencode", path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatal("detail measurement refuses a path outside /tmp/opencode; copy the store into the sandbox area")
	}
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Samples       int      `yaml:"samples"`
		ListLimit     int      `yaml:"listLimit"`
		Bands         []struct {
			Name           string `yaml:"name"`
			NominalEntries int    `yaml:"nominalEntries"`
		} `yaml:"bands"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(sandboxDetailYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, band := range fixture.Bands {
		if names[band.Name] || band.Name == "" || band.NominalEntries < 1 {
			t.Fatal("invalid or repeated detail band")
		}
		names[band.Name] = true
	}
	for _, required := range fixture.RequiredNames {
		if !names[required] {
			t.Fatalf("missing detail band %s", required)
		}
		delete(names, required)
	}
	if len(names) != 0 || fixture.Samples < 2 {
		t.Fatal("undeclared bands or insufficient detail samples")
	}
	ids := strings.Split(os.Getenv("PEASANT_SANDBOX_BAND_SESSIONS"), ",")
	if len(ids) != len(fixture.Bands) {
		t.Fatal("set PEASANT_SANDBOX_BAND_SESSIONS to the small, medium, large converted session IDs")
	}
	root := t.TempDir()
	artifacts, err := store.NewOSGenerationArtifactStoreExisting(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.OpenReadOnlyWithOptions(path, store.WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = s.Close()
	}()
	t.Logf("read-only detail sandbox=%s; record revision and corpus hash alongside this log", path)
	previousCount := 0
	for i, band := range fixture.Bands {
		sid, err := schema.NewSessionID(ids[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := s.WithSessionSnapshot(t.Context(), sid, func(snapshot indexformat.ReadSnapshot) error {
			if !snapshot.FullContentVerified {
				t.Fatal("detail measurement requires converted, authoritative DB content; convert this sandbox first")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var durations []time.Duration
		var digest [32]byte
		count := 0
		for n := 0; n < fixture.Samples; n++ {
			start := time.Now()
			raw, payload, err := transcript.BuildSnapshotDetailBytes(t.Context(), s, s, sid)
			elapsed := time.Since(start)
			if err != nil || payload == nil || len(raw) == 0 {
				t.Fatalf("verified detail %s: %v", band.Name, err)
			}
			current := sha256.Sum256(raw)
			if n == 0 {
				digest = current
				entries, err := s.ListEntries(t.Context(), sid)
				if err != nil {
					t.Fatal(err)
				}
				count = len(entries)
			} else if current != digest {
				t.Fatal("detail bytes changed between samples; stop other writers and repeat")
			}
			durations = append(durations, elapsed)
		}
		if count <= previousCount {
			t.Fatalf("band %s has %d entries; bands must increase strictly", band.Name, count)
		}
		previousCount = count
		sort.Slice(durations, func(i, j int) bool {
			return durations[i] < durations[j]
		})
		percentile := func(percent int) time.Duration {
			return durations[(len(durations)*percent+99)/100-1]
		}
		t.Logf("band=%s session=%s entries=%d nominal=%d detail_sha256=%x n=%d p50=%s p95=%s max=%s", band.Name, sid, count, band.NominalEntries, digest, len(durations), percentile(50), percentile(95), durations[len(durations)-1])
	}
}
