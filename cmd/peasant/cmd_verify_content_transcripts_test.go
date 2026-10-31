package main

import (
	_ "embed"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/verify_saved_transcripts.yaml
var verifySavedTranscriptsYAML []byte

type savedTranscriptDamageCase struct {
	Name       string `yaml:"name"`
	Damage     string `yaml:"damage"`
	TruncateTo int64  `yaml:"truncate_to"`
}

func (c savedTranscriptDamageCase) apply(path string) error {
	if c.Damage == "delete" {
		return os.Remove(path)
	}
	return os.Truncate(path, c.TruncateTo)
}

func loadSavedTranscriptDamageCases(t *testing.T) []savedTranscriptDamageCase {
	t.Helper()
	var fixture struct {
		RequiredNames []string                    `yaml:"required_names"`
		Cases         []savedTranscriptDamageCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(verifySavedTranscriptsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, name := range fixture.RequiredNames {
		if !slices.ContainsFunc(fixture.Cases, func(c savedTranscriptDamageCase) bool { return c.Name == name }) {
			t.Fatalf("fixture lost required case %q", name)
		}
	}
	return fixture.Cases
}

// TestVerifyContentFailsOnADamagedSyncedTranscript guards #682: a saved
// transcript under peasant-sync/ that was deleted or truncated must fail
// verify --content and name the session, and the printed recovery heals it.
func TestVerifyContentFailsOnADamagedSyncedTranscript(t *testing.T) {
	t.Parallel()
	for _, c := range loadSavedTranscriptDamageCases(t) {
		name := c.Name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			seed := seedVerifyHarvest(t)
			syncDir := filepath.Join(seed.dir, "out") // output.basePath in the seed config
			matches, err := filepath.Glob(filepath.Join(syncDir, "*", seed.sessionID, seed.sessionID+"--transcript.jsonl"))
			if err != nil || len(matches) != 1 {
				t.Fatalf("find the saved transcript under %s: %v %v", syncDir, matches, err)
			}
			if err := c.apply(matches[0]); err != nil {
				t.Fatal(err)
			}

			out, err := seed.run(t, "verify", "--content")
			if err == nil || strings.Contains(out, "Status: PASSED") {
				t.Errorf("verify --content with the saved transcript %s: err = %v, want a failure and no PASSED:\n%s", name, err, out)
			}
			if !strings.Contains(out, "saved transcripts damaged or missing: 1") || !strings.Contains(out, seed.sessionID) {
				t.Errorf("verify --content did not name the session with the %s transcript:\n%s", name, out)
			}

			if out, err := seed.run(t, "--force", "--session", seed.sessionID); err != nil {
				t.Fatalf("recovery harvest: %v\n%s", err, out)
			}
			if out, err := seed.run(t, "verify", "--content"); err != nil || !strings.Contains(out, "Status: PASSED") {
				t.Errorf("verify --content after the printed recovery: err = %v, want PASSED:\n%s", err, out)
			}
		})
	}
}
