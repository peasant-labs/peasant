package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/metadata_rewrite_cancellation.yaml
var metadataRewriteCancellationYAML []byte

// metadataCancellationArm names when the run is cancelled. It is the only axis
// the fixture varies, so the recorded filesystem calls answer one question:
// does the metadata lookup that runs before a rewrite obey the run's context?
type metadataCancellationArm string

const (
	// cancelNever lets the lookup complete; it proves the seeded layout is
	// readable, so a case that records few calls records a real refusal.
	cancelNever metadataCancellationArm = "not-cancelled"
	// cancelBeforeLookup cancels before the call, so nothing may be read.
	cancelBeforeLookup metadataCancellationArm = "before-lookup"
	// cancelDuringWalk cancels inside a filesystem call, so the walk must stop
	// at that call rather than continue through the managed directory.
	cancelDuringWalk metadataCancellationArm = "during-walk"
)

type metadataCancellationCase struct {
	Name             string                  `yaml:"name"`
	Arm              metadataCancellationArm `yaml:"arm"`
	CancelAfterCalls int                     `yaml:"cancelAfterCalls"`
	ExpectedCalls    int                     `yaml:"expectedCalls"`
	ExpectMetadata   bool                    `yaml:"expectMetadata"`
}

func loadMetadataCancellationCases(t *testing.T) []metadataCancellationCase {
	t.Helper()
	var fixture struct {
		RequiredNames []string                   `yaml:"requiredNames"`
		Cases         []metadataCancellationCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(metadataRewriteCancellationYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode the metadata cancellation fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("the metadata cancellation fixture must hold exactly one YAML document: %v", err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		if tc.Name == "" || present[tc.Name] {
			t.Fatalf("the metadata cancellation fixture has an empty or repeated case name %q", tc.Name)
		}
		present[tc.Name] = true
		switch tc.Arm {
		case cancelNever, cancelBeforeLookup, cancelDuringWalk:
		default:
			t.Fatalf("fixture case %q names the unknown arm %q; add the arm and its assertion, or correct the fixture", tc.Name, tc.Arm)
		}
		if (tc.Arm == cancelDuringWalk) != (tc.CancelAfterCalls > 0) {
			t.Fatalf("fixture case %q disagrees with itself: arm %q cancels after %d filesystem calls; only the during-walk arm cancels inside a call", tc.Name, tc.Arm, tc.CancelAfterCalls)
		}
	}
	for _, required := range fixture.RequiredNames {
		if !present[required] {
			t.Fatalf("required fixture case %q is missing; the rule it pins would stop being tested", required)
		}
	}
	return fixture.Cases
}

// countingCancelFS records every filesystem read the lookup performs and can
// cancel the run from inside one of them, which is how a case reproduces a
// cancellation that arrives mid-walk without depending on timing.
type countingCancelFS struct {
	FileSystem
	calls       int
	cancelAfter int
	cancel      context.CancelFunc
}

func (filesystem *countingCancelFS) note() {
	filesystem.calls++
	if filesystem.cancelAfter > 0 && filesystem.calls == filesystem.cancelAfter {
		filesystem.cancel()
	}
}

func (filesystem *countingCancelFS) Stat(path string) (os.FileInfo, error) {
	filesystem.note()
	return filesystem.FileSystem.Stat(path)
}

func (filesystem *countingCancelFS) ReadDir(path string) ([]os.DirEntry, error) {
	filesystem.note()
	return filesystem.FileSystem.ReadDir(path)
}

func (filesystem *countingCancelFS) ReadFile(path string) ([]byte, error) {
	filesystem.note()
	return filesystem.FileSystem.ReadFile(path)
}

// TestMetadataForRewriteObeysTheRunContext pins that the metadata lookup which
// runs before every rewrite stops reading the filesystem when the run is
// cancelled. It ran with a fresh background context, so the context checks in
// the lookup were dead on this path and a cancelled harvest kept walking the
// managed output directory.
func TestMetadataForRewriteObeysTheRunContext(t *testing.T) {
	for _, tc := range loadMetadataCancellationCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			outputDir := t.TempDir()
			sessionID, err := NewSessionID("6b1f1b2c-6a54-4c1f-9d4e-2f0f6a1c7b31")
			if err != nil {
				t.Fatal(err)
			}
			sessionDir := filepath.Join(outputDir, "test-host", string(sessionID))
			if err := os.MkdirAll(sessionDir, 0o700); err != nil {
				t.Fatal(err)
			}
			metadataPath := filepath.Join(sessionDir, string(sessionID)+defaults.MetadataSuffix)
			if err := os.WriteFile(metadataPath, (fmt.Appendf(nil, `{"schemaVersion":%d}`, CurrentSchemaVersion)), 0o600); err != nil {
				t.Fatal(err)
			}
			resolved, err := NewResolvedPath(outputDir)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			filesystem := &countingCancelFS{FileSystem: &OSFileSystem{}, cancelAfter: tc.CancelAfterCalls, cancel: cancel}
			pipeline := &Pipeline{fs: filesystem, config: PipelineConfig{OutputDir: resolved}}
			if tc.Arm == cancelBeforeLookup {
				cancel()
			}
			metadata, err := pipeline.metadataForRewrite(ctx, DiscoveredSession{SessionID: sessionID, Harness: HarnessClaudeCode})
			if tc.Arm == cancelNever {
				if err != nil {
					t.Fatalf("the seeded managed layout was not readable, so no case can prove a refusal: %v", err)
				}
				if tc.ExpectMetadata && metadata == nil {
					t.Fatal("the seeded managed metadata was not found, so no case can prove a refusal")
				}
				if filesystem.calls <= 1 {
					t.Fatalf("a complete lookup made %d filesystem calls; a cancelled case that stops at one call would then prove nothing", filesystem.calls)
				}
				return
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("case %q returned %v and metadata %v; a cancelled run must report its cancellation rather than keep reading the managed directory", tc.Name, err, metadata)
			}
			if filesystem.calls != tc.ExpectedCalls {
				t.Fatalf("case %q made %d filesystem calls after cancellation, want exactly %d; the lookup kept reading the managed output directory of a cancelled run", tc.Name, filesystem.calls, tc.ExpectedCalls)
			}
		})
	}
}
