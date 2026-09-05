package main

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/profile_cli/span_tree.yaml
var pushProfileTreeYAML []byte

//go:embed testdata/profile_cli/span_tree_manifest.yaml
var pushProfileTreeManifestYAML []byte

type pushProfileTreeFixtures struct {
	Parents map[perf.StageID][]perf.StageID `yaml:"parents"`
	Cases   []struct {
		Name  string `yaml:"name"`
		Error string `yaml:"error"`
		Spans []struct {
			ID      string       `yaml:"id"`
			Parent  string       `yaml:"parent"`
			Stage   perf.StageID `yaml:"stage"`
			Subject string       `yaml:"subject"`
		} `yaml:"spans"`
	} `yaml:"cases"`
}

func loadPushProfileTreeFixtures(t *testing.T) pushProfileTreeFixtures {
	t.Helper()
	var fixtures pushProfileTreeFixtures
	if err := yaml.Unmarshal(pushProfileTreeYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(pushProfileTreeManifestYAML, "CLI span tree")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, fixture := range fixtures.Cases {
		names = append(names, fixture.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "CLI span tree"); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func TestPushProfileTreeValidation(t *testing.T) {
	for _, fixture := range loadPushProfileTreeFixtures(t).Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			var spans []perf.ProfileSpan
			for _, span := range fixture.Spans {
				spans = append(spans, perf.ProfileSpan{SpanID: span.ID, ParentSpanID: span.Parent, Stage: span.Stage, SafeSubjectID: span.Subject})
			}
			err := validatePushProfileTree(spans)
			if fixture.Error == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), fixture.Error) {
				t.Fatalf("tree validation = %v; want %q", err, fixture.Error)
			}
		})
	}
}

// Every recorded CLI span must reach the sole CLI-owned root. Walking each
// ancestry (rather than checking just immediate parents) also rejects cycles.
func validatePushProfileTree(spans []perf.ProfileSpan) error {
	byID := make(map[string]perf.ProfileSpan, len(spans))
	root := ""
	for _, span := range spans {
		if span.SpanID == "" {
			return fmt.Errorf("span %s has no ID", span.Stage)
		}
		if _, exists := byID[span.SpanID]; exists {
			return fmt.Errorf("duplicate span ID %s", span.SpanID)
		}
		byID[span.SpanID] = span
		if span.Stage == perf.StagePushRun {
			if root != "" || span.ParentSpanID != "" {
				return fmt.Errorf("push.run must be the single unparented root")
			}
			root = span.SpanID
		}
	}
	if root == "" {
		return fmt.Errorf("missing push.run root")
	}
	for _, span := range spans {
		seen := make(map[string]bool)
		for current := span; current.SpanID != root; {
			if seen[current.SpanID] {
				return fmt.Errorf("cyclic ancestry from %s (%s)", span.SpanID, span.Stage)
			}
			seen[current.SpanID] = true
			parent, exists := byID[current.ParentSpanID]
			if !exists {
				return fmt.Errorf("disconnected ancestry from %s (%s): parent %q missing", span.SpanID, span.Stage, current.ParentSpanID)
			}
			if current.Stage != perf.StagePushSession && parent.Stage == perf.StagePushSession && current.SafeSubjectID != parent.SafeSubjectID {
				return fmt.Errorf("span %s attributed to a different session than its parent", current.SpanID)
			}
			current = parent
		}
	}
	return nil
}

func assertPushProfileTree(t *testing.T, spans []perf.ProfileSpan) {
	t.Helper()
	if err := validatePushProfileTree(spans); err != nil {
		t.Fatal(err)
	}
	parents := loadPushProfileTreeFixtures(t).Parents
	byID := make(map[string]perf.ProfileSpan, len(spans))
	for _, span := range spans {
		byID[span.SpanID] = span
	}
	for _, span := range spans {
		if span.Stage == perf.StagePushRun {
			continue
		}
		if !slices.Contains(parents[span.Stage], byID[span.ParentSpanID].Stage) {
			t.Errorf("stage %s parent = %s; want one of %v", span.Stage, byID[span.ParentSpanID].Stage, parents[span.Stage])
		}
	}
}
