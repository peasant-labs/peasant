package main

import (
	"bytes"
	_ "embed"
	"errors"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/push_error_log.yaml
var pushErrorLogYAML []byte

//go:embed testdata/push_error_log_manifest.yaml
var pushErrorLogManifestYAML []byte

// TestPushErrorLogStatesTheSummaryCounts pins that the error log counts a
// not-ready session the way the summary does.
func TestPushErrorLogStatesTheSummaryCounts(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Cases []struct {
			Name     string   `yaml:"name"`
			Plan     bool     `yaml:"plan"`
			Contains []string `yaml:"contains"`
			Omits    []string `yaml:"omits"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(pushErrorLogYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode push_error_log.yaml: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(pushErrorLogManifestYAML, "push error log")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "push error log"); err != nil {
		t.Fatal(err)
	}
	notReady := push.ValidatePublicationInput(ingest.PublicationInputBundle{Readiness: ingest.PublicationNeedsIngest})
	if _, ok := push.NotReadyReasonOf(notReady); !ok {
		t.Fatalf("fixture refusal %v is not a not-ready refusal", notReady)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			result := &push.PushResult{
				Errors: 2,
				Sessions: []push.SessionPushResult{
					{SessionID: "sess-fail", HostSlug: "host-a", Status: push.PushStatusError, Error: errors.New("upload: village returned 400")},
					{SessionID: "sess-not-ready", HostSlug: "host-a", Status: push.PushStatusError, Error: notReady},
				},
			}
			counts := countsOfResult(result)
			if tc.Plan {
				counts = countsOfPlan([]planEntry{{Verdict: PlanFailed}, {Verdict: PlanNotReady}})
			}
			path, err := writePushErrorLog(result, counts, nil, nil, "https://api.village.example", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.Contains {
				if !strings.Contains(string(raw), want) {
					t.Errorf("log misses %q\n%s", want, raw)
				}
			}
			for _, forbidden := range tc.Omits {
				if strings.Contains(string(raw), forbidden) {
					t.Errorf("log carries %q\n%s", forbidden, raw)
				}
			}
		})
	}
}
