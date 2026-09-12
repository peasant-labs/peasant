package ingest_test

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/upgrade_pass.yaml
var upgradePassFixtureData []byte

// TestUpgradePassEachPhase drives the one-time upgrade pass over a seeded
// .peasant-state directory, one case per retired transaction phase. The seeded
// intent uses ROOT-RELATIVE paths, so an implementation that joins them as
// absolute paths fails to find the pair and every case breaks.
func TestUpgradePassEachPhase(t *testing.T) {
	var fixtures struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name                     string `yaml:"name"`
			Phase                    string `yaml:"phase"`
			PreviousHashSet          bool   `yaml:"previous_hash_set"`
			CurrentMatchesTranscript bool   `yaml:"current_matches_transcript"`
			WantRestored             bool   `yaml:"want_restored"`
			WantToIngest             bool   `yaml:"want_to_ingest"`
			WantStateRemoved         bool   `yaml:"want_state_removed"`
			BackupHashMismatch       bool   `yaml:"backup_hash_mismatch"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(upgradePassFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, c := range fixtures.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("empty or duplicate upgrade fixture %q", c.Name)
		}
		names[c.Name] = true
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing required fixture %s", name)
		}
	}

	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			fs := testutil.NewMemFS()
			output := "/output"
			sid := "11111111-1111-4111-8111-111111111111"
			host := testutil.TestHostSlug

			// The transcript stays on disk unchanged. The consistent metadata
			// records its checksum, so the pair verifies once that metadata is
			// the one on disk.
			consistentMeta := upgradeMetaBytes(t, sid, host, rebuildTranscript)

			// Root-relative locators, exactly as the retired writer recorded them.
			relDir := ingest.SessionDir("", host, sid, "")
			relMetadata := filepath.Join(relDir, sid+"--metadata.json")
			relTranscript := filepath.Join(relDir, sid+"--transcript.jsonl")

			if err := fs.WriteFile(filepath.Join(output, relTranscript), []byte(rebuildTranscript), 0600); err != nil {
				t.Fatal(err)
			}

			// A first install left only a transcript: no metadata on disk and no
			// previous copy to restore.
			if c.PreviousHashSet {
				if c.CurrentMatchesTranscript {
					// prepared: the metadata on disk is already the consistent one.
					if err := fs.WriteFile(filepath.Join(output, relMetadata), consistentMeta, 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					// An interrupted write left a different metadata on disk; the
					// backup holds the consistent previous copy.
					if err := fs.WriteFile(filepath.Join(output, relMetadata), []byte(`{"half":"written"}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}

			// Seed the transaction directory: intent.json plus, when there is a
			// previous copy, the backup at old/0000.
			key := "txkey"
			txKeyDir := filepath.Join(output, ".peasant-state", "transactions", key)
			var previousHash *string
			if c.PreviousHashSet {
				h := schema.ComputeTranscriptHash(consistentMeta)
				previousHash = &h
				backup := consistentMeta
				if c.BackupHashMismatch {
					// The backup on disk does not hash to the recorded previous
					// hash: a torn or wrong backup. The pass must refuse to
					// rename it over the live file.
					backup = []byte(`{"corrupt":"backup"}`)
				}
				if err := fs.WriteFile(filepath.Join(txKeyDir, "old", "0000"), backup, 0600); err != nil {
					t.Fatal(err)
				}
			}
			intent := map[string]any{
				"sessionId": sid,
				"phase":     c.Phase,
				"directory": relDir,
				"files": []map[string]any{
					{"path": relMetadata, "previousHash": previousHash},
				},
			}
			intentJSON, err := json.Marshal(intent)
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.WriteFile(filepath.Join(txKeyDir, "intent.json"), intentJSON, 0600); err != nil {
				t.Fatal(err)
			}
			// A lock file the retired build left behind; the pass removes the
			// whole state directory only when everything resolves.
			if err := fs.WriteFile(filepath.Join(output, ".peasant-state", "locks", key+".lock"), nil, 0600); err != nil {
				t.Fatal(err)
			}

			result := ingest.RunUpgradePass(fs, output, nil)
			if !result.Ran {
				t.Fatal("pass did not run over a seeded state directory")
			}
			if got := len(result.Restored) == 1; got != c.WantRestored {
				t.Errorf("restored = %v, want %v (result %+v)", result.Restored, c.WantRestored, result)
			}
			if got := len(result.ToIngest) == 1; got != c.WantToIngest {
				t.Errorf("to-ingest = %v, want %v (result %+v)", result.ToIngest, c.WantToIngest, result)
			}
			_, statErr := fs.Stat(filepath.Join(output, ".peasant-state"))
			removed := statErr != nil
			if removed != c.WantStateRemoved {
				t.Errorf("state removed = %v, want %v", removed, c.WantStateRemoved)
			}
			if c.WantRestored {
				// The restored pair now verifies: the consistent metadata is back
				// on disk and matches the transcript.
				onDisk, err := fs.ReadFile(filepath.Join(output, relMetadata))
				if err != nil || schema.ComputeTranscriptHash(onDisk) != schema.ComputeTranscriptHash(consistentMeta) {
					t.Errorf("restored metadata does not match the previous copy: %v", err)
				}
			}
			if c.BackupHashMismatch {
				// The live file must be untouched: a backup that fails its hash
				// check is never renamed over it. Dropping the backup-hash check
				// clobbers the live file with the corrupt backup and reddens this.
				onDisk, err := fs.ReadFile(filepath.Join(output, relMetadata))
				if err != nil || string(onDisk) != `{"half":"written"}` {
					t.Errorf("a backup that failed its hash check was renamed over the live file: %q (%v)", onDisk, err)
				}
			}
		})
	}
}

// upgradeMetaBytes builds Strike metadata whose recorded checksum matches the
// transcript, so the pair verifies.
func upgradeMetaBytes(t *testing.T, sid, host, transcript string) []byte {
	t.Helper()
	meta := makeMinimalMeta(t, sid)
	meta.Project.Hash = testutil.TestProjectHash
	meta.ModelHarness = ingest.HarnessStrike
	meta.HostSlug = ingest.HostSlug(host)
	meta.Source.FilePath = "/synthetic/strike-source.jsonl"
	meta.ContentHash = schema.ComputeTranscriptHash([]byte(transcript))
	meta.MetadataHash = schema.ComputeMetadataHash(meta)
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	// Confirm the seed is a shape the artifact reader accepts.
	if _, err := ingest.NewManagedArtifact(encoded, []byte(transcript)); err != nil {
		t.Fatal(fmt.Errorf("seed metadata is not a valid pair: %w", err))
	}
	return encoded
}
