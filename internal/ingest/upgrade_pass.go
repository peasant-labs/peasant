package ingest

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// upgrade_pass.go finishes writes an earlier build left half-applied in
// <output>/.peasant-state. That build installed each session's files through a
// durable file transaction with per-file backups and a lock directory; this
// build installs by rename with no transaction, so the state directory only
// ever holds work an upgraded binary must finish once. This whole file is
// transitional: DELETE it once no store in the field can still carry a
// .peasant-state directory (target: two releases after the one that first
// ships the rename write path).
//
// The pass reads no store and mirrors nothing. It restores the previous copy of
// each half-changed file, verifies the pair, and removes the state directory
// only when every intent resolved and every pair verifies. A session it cannot
// resolve is left for the next harvest to ingest again.

// upgradeIntentFile is the subset of the retired intent file this pass reads.
// The path is root-relative to the output directory.
type upgradeIntentFile struct {
	Path         string  `json:"path"`
	PreviousHash *string `json:"previousHash"`
}

// upgradeIntent is the subset of the retired intent.json this pass decodes.
type upgradeIntent struct {
	SessionID SessionID           `json:"sessionId"`
	Phase     string              `json:"phase"`
	Directory string              `json:"directory"`
	Files     []upgradeIntentFile `json:"files"`
}

// The retired transaction format's phase names.
const (
	upgradePhasePrepared      = "prepared"
	upgradePhaseFilesChanging = "files-changing"
	upgradePhaseRollingBack   = "rolling-back"
)

// UpgradeResult reports what the one-time upgrade pass did.
type UpgradeResult struct {
	// Restored names sessions whose previous copy this pass put back and whose
	// pair then verified.
	Restored []SessionID
	// ToIngest names sessions the pass could not resolve, which the next harvest
	// re-ingests from their source.
	ToIngest []SessionID
	// Ran reports that a state directory existed and the pass processed it.
	Ran bool
}

// RunUpgradePass finishes the interrupted writes of an earlier build. It reads
// only the filesystem and reports through the RECOVER stage. On a store with no
// state directory it returns immediately and the RECOVER stage stays unstarted.
func RunUpgradePass(fs FileSystem, outputDir string, prog *ProgressState) UpgradeResult {
	stateDir := filepath.Join(outputDir, managedArtifactStateDir)
	txDir := filepath.Join(stateDir, "transactions")
	entries, err := fs.ReadDir(txDir)
	if err != nil {
		return UpgradeResult{}
	}
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			keys = append(keys, entry.Name())
		}
	}
	if len(keys) == 0 {
		return UpgradeResult{}
	}
	sort.Strings(keys)

	emitProgress(prog, ProgressEvent{Kind: KindStart, Stage: StageRecover, Total: len(keys)})
	result := UpgradeResult{Ran: true}
	allResolved := true
	for done, key := range keys {
		intent, ok := decodeUpgradeIntent(fs, filepath.Join(txDir, key, "intent.json"))
		if !ok {
			allResolved = false
			emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageRecover, Done: done + 1, Total: len(keys)})
			continue
		}
		restored := false
		if intent.Phase != upgradePhasePrepared {
			for index, file := range intent.Files {
				// A file with no previous hash is a first install; it is left in
				// place. Its pair does not verify, so the session falls to the
				// next harvest to ingest again.
				if file.PreviousHash == nil {
					continue
				}
				backup := filepath.Join(txDir, key, "old", fmt.Sprintf("%04d", index))
				data, readErr := fs.ReadFile(backup)
				if readErr != nil || schema.ComputeTranscriptHash(data) != *file.PreviousHash {
					allResolved = false
					continue
				}
				if renameErr := fs.Rename(backup, filepath.Join(outputDir, file.Path)); renameErr != nil {
					allResolved = false
					continue
				}
				restored = true
			}
		}
		metadataPath := filepath.Join(outputDir, intent.Directory, string(intent.SessionID)+defaults.MetadataSuffix)
		if _, verifyErr := readArtifactPair(fs, outputDir, metadataPath, intent.SessionID); verifyErr != nil {
			result.ToIngest = append(result.ToIngest, intent.SessionID)
			allResolved = false
		} else if restored {
			result.Restored = append(result.Restored, intent.SessionID)
		}
		emitProgress(prog, ProgressEvent{Kind: KindAdvance, Stage: StageRecover, Done: done + 1, Total: len(keys)})
	}

	// Remove the state directory (transactions and locks) only when nothing is
	// left to finish; otherwise leave it and let the run continue.
	if allResolved && len(result.ToIngest) == 0 {
		_ = fs.RemoveAll(stateDir)
	}
	emitProgress(prog, ProgressEvent{Kind: KindEnd, Stage: StageRecover, Done: len(result.Restored), Total: len(keys)})
	return result
}

// UpgradeReport is the line the harvest prints when the upgrade pass ran.
func (r UpgradeResult) Report() string {
	return fmt.Sprintf("Finished %d interrupted writes from an earlier build. Sessions restored from their previous copy: %s. Sessions to ingest again: %s. Run `peasant harvest --force --session <id>` for each.",
		len(r.Restored)+len(r.ToIngest), formatSessionList(r.Restored), formatSessionList(r.ToIngest))
}

func decodeUpgradeIntent(fs FileSystem, path string) (upgradeIntent, bool) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return upgradeIntent{}, false
	}
	var intent upgradeIntent
	if err := json.Unmarshal(data, &intent); err != nil || intent.SessionID == "" {
		return upgradeIntent{}, false
	}
	return intent, true
}

func formatSessionList(ids []SessionID) string {
	if len(ids) == 0 {
		return "none"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = string(id)
	}
	return joinComma(parts)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
