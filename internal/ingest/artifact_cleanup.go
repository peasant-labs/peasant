package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
)

// Cleanup collects only validated preparation/retirement leftovers while holding
// their session lock. Unknown evidence is retained and reported without blocking
// independent sessions. Call only from persistent harvest/recovery, never reads.
func (p *ArtifactPublisher) Cleanup(ctx context.Context, include func(SessionID, Harness) bool) []DiagnosticEntry {
	root, err := p.fs.OpenArtifactRoot(p.output)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []DiagnosticEntry{artifactRecoveryDiagnostic(p.output, err)}
	}
	defer root.Close()
	parent := filepath.Join(managedArtifactStateDir, "transactions")
	entries, err := root.ReadDir(parent)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []DiagnosticEntry{artifactRecoveryDiagnostic(parent, err)}
	}
	var diagnostics []DiagnosticEntry
	for _, entry := range entries {
		if ctx.Err() != nil {
			diagnostics = append(diagnostics, artifactRecoveryDiagnostic(parent, ctx.Err()))
			break
		}
		name := entry.Name()
		prepared := strings.HasPrefix(name, ".prepare-")
		retired := strings.HasPrefix(name, ".retired-")
		if !prepared && !retired {
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(strings.TrimPrefix(name, ".prepare-"), ".retired-"), "-", 2)
		path := filepath.Join(parent, name)
		if !entry.IsDir() || len(parts) != 2 || !validArtifactHash(parts[0]) || parts[1] == "" {
			diagnostics = append(diagnostics, artifactRecoveryDiagnostic(path, fmt.Errorf("unrecognized lifecycle ownership name or file type")))
			continue
		}
		key := parts[0]
		cleanupErr := func() error {
			lock, err := root.Lock(ctx, artifactLockPath(key), ArtifactLockWrite, false)
			if err != nil {
				return err
			}
			defer lock.Close()
			if _, err := root.Lstat(path); errors.Is(err, fs.ErrNotExist) {
				return nil
			} else if err != nil {
				return err
			}
			intent, err := readArtifactIntentAt(root, filepath.Join(path, "intent.json"), key)
			if errors.Is(err, fs.ErrNotExist) && include == nil {
				return removeEmptyArtifactLifecycle(root, path)
			}
			if err != nil {
				return err
			}
			if include != nil && !include(intent.SessionID, intent.Harness) {
				return nil
			}
			if prepared && intent.Phase != artifactPrepared {
				return fmt.Errorf("preparation directory contains an active/rollback intent; evidence was preserved")
			}
			if prepared || intent.Phase == artifactRollingBack {
				for _, file := range intent.Files {
					hash, err := currentArtifactFileHash(root, file.Path)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(hash, file.PreviousHash) {
						return fmt.Errorf("prior owned files no longer match this lifecycle record; stale evidence was preserved")
					}
				}
			} else {
				artifact, committed, err := committedArtifactIntent(root, intent)
				if err != nil {
					return err
				}
				if !committed {
					return fmt.Errorf("retirement does not match a validated committed artifact; evidence was preserved")
				}
				if intent.NeedsDatabase {
					if p.mirror == nil {
						return fmt.Errorf("database-backed retirement requires verified reconciliation; file-only mode preserved the evidence")
					}
					// Reconciliation here carries no newly acquired cursor/origin.
					// Existing source progress is preserved, never replayed backwards.
					results := p.mirror.MirrorArtifacts(ctx, []ArtifactMirrorRequest{{Artifact: artifact}})
					if len(results) != 1 || !results[0].Mirrored || results[0].Err != nil {
						return fmt.Errorf("retired artifact could not be verified against the database mirror; evidence was preserved")
					}
				}
			}
			return cleanupArtifactIntentAt(root, path, intent)
		}()
		if cleanupErr != nil {
			diagnostics = append(diagnostics, artifactRecoveryDiagnostic(path, cleanupErr))
		}
	}
	return diagnostics
}

func artifactRecoveryDiagnostic(path string, err error) DiagnosticEntry {
	return DiagnosticEntry{ErrorType: "artifact_recovery_incomplete", Location: path, Message: fmt.Sprintf("inspect retained publication state during harvest cleanup: %v; unknown/stale recovery evidence was not discarded", err), Remediation: "Restore readable valid recovery state, or inspect the retained files before manually removing stale temporary evidence; then retry harvest."}
}

// No intent permits removal of empty directory scaffolding only. A partial
// intent or any backup/candidate bytes are evidence, not an empty directory.
func removeEmptyArtifactLifecycle(root ArtifactRoot, path string) error {
	entries, err := root.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() != "old" && entry.Name() != "new" {
			return fmt.Errorf("missing intent with nonempty/unknown lifecycle content; evidence was preserved")
		}
		children, err := root.ReadDir(filepath.Join(path, entry.Name()))
		if err != nil {
			return err
		}
		if len(children) != 0 {
			return fmt.Errorf("missing intent with retained candidate/backup bytes; evidence was preserved")
		}
	}
	for _, entry := range entries {
		if err := root.Remove(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	if err := root.Remove(path); err != nil {
		return err
	}
	return root.SyncDir(filepath.Dir(path))
}
