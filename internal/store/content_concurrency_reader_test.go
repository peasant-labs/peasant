package store_test

import (
	"bytes"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_concurrency.yaml
var concurrencyReaderYAML []byte

type concurrencyReaderCase struct {
	Name         string `yaml:"name"`
	Mode         string `yaml:"mode"`
	Seam         string `yaml:"seam"`
	Expect       string `yaml:"expect"`
	CrossProcess bool   `yaml:"crossProcess"`
	FirstText    string `yaml:"firstText"`
	NextText     string `yaml:"nextText"`
}

func TestContentConcurrencyReaders(t *testing.T) {
	var fixtures struct {
		Cases []concurrencyReaderCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(concurrencyReaderYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixtures.Cases {
		if c.Mode != "reader-stage" && c.Mode != "reader-commit" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			if c.Seam != "snapshot-callback" || c.FirstText == "" || c.NextText == "" {
				t.Fatalf("incomplete reader fixture: %+v", c)
			}
			if c.Mode == "reader-commit" {
				if c.Expect != "original-detail-after-sweep" {
					t.Fatal("unknown reader commit expectation")
				}
				runSnapshotActivationSweep(t, snapshotRaceCase{Name: c.Name, FirstText: c.FirstText, NextText: c.NextText, Sweep: true})
				return
			}
			if c.Expect != "original-detail" {
				t.Fatal("unknown reader stage expectation")
			}
			s := openSnapshotStore(t)
			sid := schema.SessionID("b9b9b9b9-b9b9-49b9-89b9-b9b9b9b9b9b9")
			seedHarmonizedSnapshot(t, s, string(sid), "reader-seed")
			if _, err := s.ActivateGeneration(t.Context(), snapshotActivation(sid, "reader-original", c.FirstText)); err != nil {
				t.Fatal(err)
			}
			want, _, err := transcript.BuildSnapshotDetailBytes(t.Context(), s, s, sid)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.WithSessionSnapshot(t.Context(), sid, func(snapshot indexformat.ReadSnapshot) error {
				if _, err := s.StageGeneration(t.Context(), snapshotActivation(sid, "reader-staged", c.NextText)); err != nil {
					return err
				}
				got, payload, err := transcript.BuildSnapshotDetailBytes(t.Context(), capturedSnapshotReader{snapshot}, s, sid)
				if err != nil {
					return err
				}
				if payload == nil || !bytes.Equal(got, want) {
					t.Fatalf("staging changed captured reader bytes:\ngot %s\nwant %s", got, want)
				}
				current, _, err := transcript.BuildSnapshotDetailBytes(t.Context(), s, s, sid)
				if err != nil {
					return err
				}
				if !bytes.Equal(current, want) {
					t.Fatalf("second connection saw staged authority: %s", current)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
