package ingest

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/source_record_unusable.yaml
var sourceRecordUnusableFixtureData []byte

//go:embed testdata/source_unavailable_cause.yaml
var sourceUnavailableCauseFixtureData []byte

type sourceRecordUnusableFixtures struct {
	Required []string `yaml:"required_names"`
	Cases    []struct {
		Name string `yaml:"name"`
		Kind string `yaml:"kind"`
		Want bool   `yaml:"want"`
	} `yaml:"cases"`
}

// TestSourceRecordUnusableClassifiesOnlyTheSourceBytes pins the boundary the
// settle depends on: the bytes on disk can settle, because they are the problem
// the user has to fix, while a store, locator, or write fault must keep
// reporting until it clears on its own.
func TestSourceRecordUnusableClassifiesOnlyTheSourceBytes(t *testing.T) {
	var fixtures sourceRecordUnusableFixtures
	if err := yaml.Unmarshal(sourceRecordUnusableFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate source-record fixture %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	// This white-box test cannot import internal/testutil (it would close an
	// import cycle through this package), so it checks the required names here.
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing source-record fixture %s", name)
		}
	}
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			var err error
			switch fixture.Kind {
			case "not_exist":
				err = fs.ErrNotExist
			case "wrapped_not_exist":
				err = fmt.Errorf("capture transcript source: %w", fs.ErrNotExist)
			case "malformed":
				err = malformedSourceRecord{}
			case "malformed_with_cause":
				err = fmt.Errorf("capture transcript source: %w", malformedSourceRecord{cause: errors.New("required message content missing")})
			case "locator":
				err = errors.New("synthetic locator lookup failure")
			case "store":
				err = fmt.Errorf("commit session entries: %w", errors.New("database is locked"))
			default:
				t.Fatalf("case %s names unknown kind %q", fixture.Name, fixture.Kind)
			}
			if got := sourceRecordUnusable(err); got != fixture.Want {
				t.Fatalf("sourceRecordUnusable(%v) = %v, want %v", err, got, fixture.Want)
			}
		})
	}
}

func TestSourceUnavailableCauseClassifiesTheClosedSet(t *testing.T) {
	var fixtures struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name      string `yaml:"name"`
			Kind      string `yaml:"kind"`
			WantCause string `yaml:"want_cause"`
			WantOK    bool   `yaml:"want_ok"`
		} `yaml:"cases"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(sourceUnavailableCauseFixtureData))
	dec.KnownFields(true)
	if err := dec.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		t.Fatalf("source cause fixture must be one document: %v", err)
	}
	if len(fixtures.Required) == 0 {
		t.Fatal("source cause fixture declares no required names")
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate source-cause fixture %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing source-cause fixture %s", name)
		}
	}
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			var err error
			switch fixture.Kind {
			case "nil":
				err = nil
			case "not_exist":
				err = fs.ErrNotExist
			case "wrapped_not_exist":
				err = fmt.Errorf("capture transcript source: %w", fs.ErrNotExist)
			case "malformed":
				err = malformedSourceRecord{}
			case "malformed_with_cause":
				err = fmt.Errorf("capture transcript source: %w", malformedSourceRecord{cause: errors.New("required message content missing")})
			case "locator":
				err = errors.New("synthetic locator lookup failure")
			case "store":
				err = fmt.Errorf("commit session entries: %w", errors.New("database is locked"))
			default:
				t.Fatalf("case %s names unknown kind %q", fixture.Name, fixture.Kind)
			}
			got, ok := sourceUnavailableCause(err)
			if ok != fixture.WantOK || (ok && string(got) != fixture.WantCause) {
				t.Fatalf("sourceUnavailableCause(%v) = %q, %v; want %q, %v", err, got, ok, fixture.WantCause, fixture.WantOK)
			}
		})
	}
}
