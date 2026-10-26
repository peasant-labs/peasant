// profile-opencode-native measures native SQLite acquisition and generation.
// Use only a consistent sandbox backup; the production source opens read-only.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/schema"
)

type refs struct{ entry, submission int }

func (r *refs) NewEntryRef() (schema.SourceEntryRef, error) {
	r.entry++
	return schema.NewSourceEntryRef(fmt.Sprintf("e_%026d", r.entry))
}
func (r *refs) NewSubmissionRef() (schema.SubmissionRef, error) {
	r.submission++
	return schema.NewSubmissionRef(fmt.Sprintf("s_%026d", r.submission))
}

var _ ingest.RefAllocator = (*refs)(nil)

// Git/project lookups are auxiliary to native parsing. Deny them rather than
// traversing private working directories recorded by the copied source.
type unavailableGit struct{}

func (unavailableGit) RemoteURL(context.Context, string) (string, error) { return "", os.ErrPermission }
func (unavailableGit) Branch(context.Context, string) (string, error)    { return "", os.ErrPermission }
func (unavailableGit) Worktree(context.Context, string) (string, error)  { return "", os.ErrPermission }
func (unavailableGit) TrackingBranch(context.Context, string) (string, error) {
	return "", os.ErrPermission
}
func (unavailableGit) UserEmail(context.Context) (string, error) { return "", os.ErrPermission }
func (unavailableGit) WalkUpRemoteURL(context.Context, string) (string, string, error) {
	return "", "", os.ErrPermission
}

var _ ingest.GitResolver = unavailableGit{}

type sandboxEnvironment struct{ db string }

func (e sandboxEnvironment) LookupEnv(key string) (string, bool) {
	if key == "OPENCODE_DB" {
		return e.db, true
	}
	return "", false
}

var _ ingest.OpenCodeEnvironmentLookup = sandboxEnvironment{}

// Discovery also considers conventional sibling databases and legacy trees.
// The override alone does not confine it: deny every other source before I/O.
type backupFileSystem struct{ db string }

func (f backupFileSystem) approved(path string) bool {
	if filepath.Clean(path) != f.db {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == f.db
}

func (f backupFileSystem) Stat(path string) (os.FileInfo, error) {
	// Discovery uses only this sidecar's mtime, not its contents. Preserve
	// ordinary absent-WAL freshness behavior without following a symlink.
	if filepath.Clean(path) == f.db+"-wal" {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, os.ErrPermission
		}
		return info, nil
	}
	if !f.approved(path) {
		return nil, os.ErrPermission
	}
	return os.Stat(path)
}

func (f backupFileSystem) Lstat(path string) (os.FileInfo, error) {
	if !f.approved(path) {
		return nil, os.ErrPermission
	}
	return os.Lstat(path)
}

func (f backupFileSystem) Open(path string) (io.ReadCloser, error) {
	if !f.approved(path) {
		return nil, os.ErrPermission
	}
	return os.Open(path)
}

func (backupFileSystem) ReadFile(string) ([]byte, error)             { return nil, os.ErrPermission }
func (backupFileSystem) ReadDir(string) ([]os.DirEntry, error)       { return nil, os.ErrPermission }
func (backupFileSystem) WalkDir(string, fs.WalkDirFunc) error        { return os.ErrPermission }
func (backupFileSystem) WriteFile(string, []byte, os.FileMode) error { return os.ErrPermission }
func (backupFileSystem) MkdirAll(string, os.FileMode) error          { return os.ErrPermission }
func (backupFileSystem) Rename(string, string) error                 { return os.ErrPermission }
func (backupFileSystem) Remove(string) error                         { return os.ErrPermission }
func (backupFileSystem) RemoveAll(string) error                      { return os.ErrPermission }
func (backupFileSystem) CopyFile(string, string, os.FileMode) error  { return os.ErrPermission }

var _ ingest.FileSystem = backupFileSystem{}
var _ ingest.OpenCodeCandidateFileSystem = backupFileSystem{}

func (f backupFileSystem) openSource(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
	if !f.approved(path.String()) {
		return nil, os.ErrPermission
	}
	return ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
}

type nativeAccess struct {
	adapter *ingest.OpenCodeAdapter
	db      string
	id      string
	elapsed time.Duration
}

func (a *nativeAccess) Snapshot(ctx context.Context, _ string) (ingest.OpenCodeHistorySnapshot, error) {
	start := time.Now()
	snapshot, err := a.adapter.SnapshotOpenCodeProvenance(ctx, a.db, a.id, ingest.OpenCodeSnapshotOptions{})
	a.elapsed += time.Since(start)
	return snapshot, err
}

var _ ingest.OpenCodeSourceAccess = (*nativeAccess)(nil)

type corpusItem struct {
	Metadata string `json:"metadata"`
}

type caseResult struct {
	Case    int    `json:"case"`
	Input   string `json:"input"`
	Output  string `json:"output"`
	Refused bool   `json:"refused"`
}

type report struct {
	Mode               string       `json:"mode"`
	Phase              string       `json:"phase"`
	Cases              int          `json:"cases"`
	Success            int          `json:"success"`
	Refused            int          `json:"refused"`
	Current            int          `json:"current"`
	Legacy             int          `json:"legacy"`
	BuildSeconds       float64      `json:"buildSeconds,omitempty"`
	AcquisitionSeconds float64      `json:"acquisitionSeconds,omitempty"`
	AllocatedBytes     uint64       `json:"allocatedBytes,omitempty"`
	Allocations        uint64       `json:"allocations,omitempty"`
	Results            []caseResult `json:"results,omitempty"`
}

func main() {
	db := flag.String("db", "", "consistent sandbox SQLite backup, never a live database")
	root := flag.String("sandbox-root", "", "required sandbox directory containing the backup")
	manifest := flag.String("manifest", "", "explicit retained corpus manifest; only metadata/session selection is read")
	mode := flag.String("mode", "production", "production (discovered index dispatch), fresh (current materialization plus native generation), generation, current or legacy (materialization plus strict capture)")
	phase := flag.String("phase", "parity", "parity, timing, or profile")
	cpu := flag.String("cpuprofile", "", "CPU profile destination, required only for profile")
	flag.Parse()
	if err := run(*db, *root, *manifest, *mode, *phase, *cpu); err != nil {
		// Native errors can carry private payloads and locators.
		fmt.Fprintln(os.Stderr, "native measurement refused; verify the sandbox backup, manifest, and phase flags; no private dependency errors are printed")
		os.Exit(1)
	}
}

func run(db, root, manifest, mode, phase, cpu string) error {
	if root == "" || db == "" {
		return errors.New("invalid measurement configuration")
	}
	switch phase {
	case "parity", "timing", "profile":
	default:
		return errors.New("invalid measurement phase")
	}
	if (phase == "profile") != (cpu != "") {
		return errors.New("CPU profiling requires its own phase and destination")
	}
	switch mode {
	case "production", "fresh", "generation", "current", "legacy":
	default:
		return errors.New("invalid measurement mode")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	db, err = filepath.EvalSymlinks(db)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, db)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return errors.New("database is outside the sandbox")
	}
	var items []corpusItem
	if manifest != "" {
		raw, err := os.ReadFile(manifest)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
			return errors.New("invalid corpus")
		}
	} else if mode != "legacy" {
		return errors.New("a selected metadata corpus is required")
	}
	// Metadata and selection are loaded before measurement. Native rows are not:
	// every BuildGeneration below opens and reads the actual SQLite backup.
	metadata := make([]schema.UnifiedMetadata, len(items))
	inputs := make([]string, len(items))
	for i, item := range items {
		data, err := os.ReadFile(item.Metadata)
		if err != nil || json.Unmarshal(data, &metadata[i]) != nil || metadata[i].ModelHarness != schema.HarnessOpenCode {
			return errors.New("invalid OpenCode metadata")
		}
		if _, err := ingest.NewSessionID(metadata[i].SessionID.String()); err != nil {
			return err
		}
		if filepath.Clean(metadata[i].Source.FilePath) == db {
			return errors.New("backup is the recorded live source")
		}
		inputs[i] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	filesystem := backupFileSystem{db: db}
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(
		filesystem, unavailableGit{}, salt.Salt{}, "latest", sandboxEnvironment{db: db},
		filesystem, filesystem.openSource, ingest.DefaultOpenCodeSQLiteSourceOptions(),
	)
	if err != nil {
		return errors.New("sandbox source configuration refused")
	}
	rootPath, err := ingest.NewResolvedPath(filepath.Dir(db))
	if err != nil {
		return err
	}
	discovered, err := adapter.Discover(context.Background(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{rootPath}})
	if err != nil {
		return errors.New("native discovery refused")
	}
	sessions := make(map[schema.SessionID]ingest.DiscoveredSession, len(discovered))
	for _, session := range discovered {
		sessions[session.SessionID] = session
	}
	if manifest == "" {
		// Legacy-only sessions may have no retained artifact. Select their
		// identities from real discovery; no metadata or payload is fabricated
		// for the producer. This mode acquires both through materialization.
		for _, session := range discovered {
			if session.TranscriptOrigin != ingest.TranscriptOriginOpenCodeLegacySQLite {
				continue
			}
			metadata = append(metadata, schema.UnifiedMetadata{SessionID: session.SessionID, ModelHarness: schema.HarnessOpenCode})
			selection, err := json.Marshal(session)
			if err != nil {
				return err
			}
			inputs = append(inputs, fmt.Sprintf("%x", sha256.Sum256(selection)))
		}
		if len(metadata) == 0 {
			return errors.New("no legacy SQLite authority was discovered")
		}
	}
	for _, meta := range metadata {
		session, exists := sessions[meta.SessionID]
		if !exists || (session.TranscriptOrigin != ingest.TranscriptOriginOpenCodeCurrentSQLite && session.TranscriptOrigin != ingest.TranscriptOriginOpenCodeLegacySQLite) {
			return errors.New("selected session has no discovered SQLite authority")
		}
		if !filesystem.approved(session.SourcePath.String()) {
			return errors.New("selected session authority is not the approved backup")
		}
		requiresCurrent := mode == "generation" || mode == "current" || mode == "fresh"
		if requiresCurrent && session.TranscriptOrigin != ingest.TranscriptOriginOpenCodeCurrentSQLite {
			return errors.New("requested mode contradicts discovered authority")
		}
		if mode == "legacy" && session.TranscriptOrigin != ingest.TranscriptOriginOpenCodeLegacySQLite {
			return errors.New("requested mode contradicts discovered authority")
		}
	}
	if phase == "profile" {
		file, err := os.Create(cpu)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := pprof.StartCPUProfile(file); err != nil {
			return err
		}
		defer pprof.StopCPUProfile()
	}
	indexer := ingest.NewOpenCodeIndexer(filesystem)
	out := report{Mode: mode, Phase: phase, Cases: len(metadata)}
	ctx := context.Background()
	for i, meta := range metadata {
		session := sessions[meta.SessionID]
		if session.TranscriptOrigin == ingest.TranscriptOriginOpenCodeCurrentSQLite {
			out.Current++
		} else {
			out.Legacy++
		}
		access := &nativeAccess{adapter: adapter, db: db, id: meta.SessionID.String()}
		input := ingest.GenerationBuildInput{
			Session:      session,
			GenerationID: "gen-native-profile", RecordedMetadata: &meta,
			Allocator: &refs{}, OpenCode: access,
		}
		var before, after runtime.MemStats
		if phase == "timing" {
			runtime.ReadMemStats(&before)
		}
		start := time.Now()
		var candidate any
		var buildErr error
		if mode == "fresh" {
			acquisitionStart := time.Now()
			captured, materializeErr := adapter.MaterializeTranscriptWithCursor(ctx, session)
			materializationTime := time.Since(acquisitionStart)
			buildErr = materializeErr
			var generation ingest.GenerationCandidate
			if buildErr == nil {
				input.RecordedMetadata = captured.Metadata
				input.CapturedTranscript = captured.Transcript
				if captured.Session != nil {
					input.Session = *captured.Session
				}
				generation, buildErr = indexer.BuildGeneration(ctx, input)
			}
			access.elapsed += materializationTime
			candidate = struct {
				Materialized ingest.CursorMaterializedTranscript
				Generation   ingest.GenerationCandidate
			}{captured, generation}
		} else if mode == "generation" || mode == "production" && session.TranscriptOrigin == ingest.TranscriptOriginOpenCodeCurrentSQLite {
			candidate, buildErr = indexer.BuildGeneration(ctx, input)
		} else {
			acquisitionStart := time.Now()
			captured, materializeErr := adapter.MaterializeTranscriptWithCursor(ctx, session)
			access.elapsed = time.Since(acquisitionStart)
			buildErr = materializeErr
			var capture ingest.TranscriptCaptureResult
			if buildErr == nil {
				capture, buildErr = indexer.IndexTranscriptBytesForCapture(ctx, session, captured.Transcript)
			}
			candidate = struct {
				Materialized ingest.CursorMaterializedTranscript
				Capture      ingest.TranscriptCaptureResult
			}{captured, capture}
		}
		elapsed := time.Since(start)
		if phase == "timing" {
			runtime.ReadMemStats(&after)
			out.BuildSeconds += elapsed.Seconds()
			out.AcquisitionSeconds += access.elapsed.Seconds()
			out.AllocatedBytes += after.TotalAlloc - before.TotalAlloc
			out.Allocations += after.Mallocs - before.Mallocs
		}
		if buildErr == nil {
			out.Success++
		} else {
			out.Refused++
		}
		if phase == "parity" {
			failure := ""
			if buildErr != nil {
				failure = fmt.Sprintf("%T: %s", buildErr, buildErr.Error())
			}
			encoded, err := json.Marshal(struct {
				Result  any
				Failure string
			}{candidate, failure})
			if err != nil {
				return errors.New("candidate encoding failed")
			}
			out.Results = append(out.Results, caseResult{Case: i, Input: inputs[i], Output: fmt.Sprintf("%x", sha256.Sum256(encoded)), Refused: buildErr != nil})
		}
		runtime.KeepAlive(candidate)
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		return err
	}
	if out.Refused > 0 && phase != "parity" {
		return errors.New("refused sessions invalidate timing")
	}
	return nil
}
