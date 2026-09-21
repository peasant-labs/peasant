package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/interrupted_pair_install.yaml
var interruptedPairYAML []byte

type installSeam string

const (
	beforeTranscript  installSeam = "before-transcript"
	afterTranscript   installSeam = "after-transcript"
	afterMetadata     installSeam = "after-metadata"
	refusePreparation installSeam = "refuse-preparation"
)

type interruptedPairCase struct {
	Name         string      `yaml:"name"`
	Seam         installSeam `yaml:"seam"`
	Commands     []string    `yaml:"commands"`
	RemoveSource bool        `yaml:"remove_source"`
	CountPeer    bool        `yaml:"count_peer"`
}

type interruptedPairFixtures struct {
	RequiredNames   []string              `yaml:"required_names"`
	Target          ingest.SessionID      `yaml:"target"`
	Peer            ingest.SessionID      `yaml:"peer"`
	OriginalText    string                `yaml:"original_text"`
	ReplacementText string                `yaml:"replacement_text"`
	Cases           []interruptedPairCase `yaml:"cases"`
}

func loadInterruptedPairFixtures(t *testing.T) interruptedPairFixtures {
	t.Helper()
	var f interruptedPairFixtures
	d := yaml.NewDecoder(bytes.NewReader(interruptedPairYAML))
	d.KnownFields(true)
	if err := d.Decode(&f); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		t.Fatal("interrupted pair fixture must contain one document", err)
	}
	names := make(map[string]bool)
	for _, c := range f.Cases {
		if strings.TrimSpace(c.Name) == "" || names[c.Name] {
			t.Fatalf("invalid case name %q", c.Name)
		}
		names[c.Name] = true
		if !slices.Contains(f.RequiredNames, c.Name) {
			t.Fatalf("case %s absent from manifest", c.Name)
		}
		switch c.Seam {
		case beforeTranscript, afterTranscript, afterMetadata, refusePreparation:
		default:
			t.Fatalf("unknown seam %q", c.Seam)
		}
		if len(c.Commands) == 0 {
			t.Fatal("case has no recovery command", c.Name)
		}
		for _, command := range c.Commands {
			if command != "harvest" && command != "index" {
				t.Fatal("unknown recovery command", command)
			}
		}
	}
	if err := testutil.RequireFixtureNames("interrupted_pair_install.yaml", "case", f.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.NewSessionID(string(f.Target)); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.NewSessionID(string(f.Peer)); err != nil {
		t.Fatal(err)
	}
	if f.Target == f.Peer || f.OriginalText == "" || f.OriginalText == f.ReplacementText {
		t.Fatal("fixture requires distinct sessions and transcript content")
	}
	return f
}

// Only the exact installed target can release the parent. In the principal
// seam the real OS rename finishes before notification, and the call never
// returns to the metadata installer. SIGKILL, not cancellation, ends the child.
type gatedInstallFS struct {
	*ingest.OSFileSystem
	transcript, metadata string
	seam                 installSeam
	ready                *os.File
}

var _ ingest.FileSystem = (*gatedInstallFS)(nil)

func (f *gatedInstallFS) Rename(src, dst string) error {
	block := func() {
		if _, err := f.ready.Write([]byte("renamed\n")); err != nil {
			panic(err)
		}
		select {}
	}
	if dst == f.transcript && f.seam == beforeTranscript {
		block()
	}
	if err := f.OSFileSystem.Rename(src, dst); err != nil {
		return err
	}
	if (dst == f.transcript && f.seam == afterTranscript) || (dst == f.metadata && f.seam == afterMetadata) {
		block()
	}
	return nil
}

// Child-only environment switches never enter the production command. Ordinary
// restart uses the public OS-backed builder with the unchanged real handler.
func TestInterruptedPairInstallProcess(t *testing.T) {
	if os.Getenv("PEASANT_PAIR_CHILD") != "1" {
		return
	}
	root := newTestRoot()
	command := BuildHarvestCommand()
	if seam := os.Getenv("PEASANT_PAIR_SEAM"); seam != "" {
		ready := os.NewFile(3, "install-ready")
		defer ready.Close()
		command = buildHarvestCommand(&gatedInstallFS{OSFileSystem: &ingest.OSFileSystem{}, transcript: os.Getenv("PEASANT_PAIR_TRANSCRIPT"), metadata: os.Getenv("PEASANT_PAIR_METADATA"), seam: installSeam(seam), ready: ready})
	}
	root.AddCommand(command)
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	root.SetArgs(strings.Split(os.Getenv("PEASANT_PAIR_ARGS"), "\n"))
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
}

type installWorld struct {
	dir, output, config, native                        string
	transcript, metadata, peerTranscript, peerMetadata string
	nativeBytes                                        []byte
	nativeClock                                        time.Time
}

func (w installWorld) args(command string) []string {
	args := []string{"harvest"}
	if command == "index" {
		args = append(args, "index")
	} else {
		args = append(args, "--include-active")
	}
	return append(args, "--config", w.config, "--data-dir", w.dir, "--config-dir", w.dir, "--state-dir", w.dir, "--output", w.output, "--json")
}

func runInstallCommand(t *testing.T, w installWorld, command string, filesystem ingest.FileSystem) (jsonPipelineResult, string) {
	t.Helper()
	root := newTestRoot()
	root.AddCommand(buildHarvestCommand(filesystem))
	var out, diagnostics bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&diagnostics)
	root.SetArgs(w.args(command))
	if err := root.Execute(); err != nil {
		t.Fatalf("mounted %s: %v\n%s\n%s", command, err, &out, &diagnostics)
	}
	var result jsonPipelineResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("invalid command JSON: %v\n%s", err, &out)
	}
	return result, diagnostics.String()
}

func installChild(t *testing.T, w installWorld, command string, seam installSeam) (jsonPipelineResult, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInterruptedPairInstallProcess$")
	child.Env = append(os.Environ(), "PEASANT_PAIR_CHILD=1", "PEASANT_PAIR_ARGS="+strings.Join(w.args(command), "\n"), "PEASANT_PAIR_SEAM="+string(seam), "PEASANT_PAIR_TRANSCRIPT="+w.transcript, "PEASANT_PAIR_METADATA="+w.metadata)
	var out, diagnostics bytes.Buffer
	child.Stdout, child.Stderr = &out, &diagnostics
	if seam == "" {
		if err := child.Run(); err != nil {
			t.Fatalf("fresh %s: %v\n%s\n%s", command, err, &out, &diagnostics)
		}
		// The test binary prints PASS after the command's JSON document.
		var result jsonPipelineResult
		if err := json.NewDecoder(&out).Decode(&result); err != nil {
			t.Fatal(err, out.String())
		}
		return result, diagnostics.String()
	}
	r, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer writer.Close()
	child.ExtraFiles = []*os.File{writer}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	_ = writer.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := r.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
	}
	notice := make([]byte, len("renamed\n"))
	if _, err := io.ReadFull(r, notice); err != nil {
		t.Fatalf("exact install seam not reached: %v", err)
	}
	if string(notice) != "renamed\n" {
		t.Fatalf("invalid seam notice %q", notice)
	}
	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	err = child.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("expected SIGKILL, got %v", err)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("child did not die by SIGKILL: %v", status)
	}
	t.Logf("confirmed real %s seam and SIGKILL", seam)
	return jsonPipelineResult{}, diagnostics.String()
}

func readInstallFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func installDB(t *testing.T, w installWorld) *store.Store {
	t.Helper()
	db, err := store.Open(defaults.ResolveDBFilePathWith(w.dir).String(), store.WithIndexFormats(store.V2IndexFormat()))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

type installSnapshot struct {
	State   *ingest.SessionIndexState
	Entries []schema.SessionEntry
	Repair  []ingest.SessionID
}

func snapshotInstall(t *testing.T, w installWorld, id ingest.SessionID) installSnapshot {
	t.Helper()
	db := installDB(t, w)
	defer db.Close()
	var s installSnapshot
	var err error
	s.State, err = db.ReadIndexState(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	s.Entries, err = db.ListEntries(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	s.Repair, err = db.ListSessionsNeedingRepair(t.Context(), ingest.NativeGenerationTargets(ingest.HarvesterVersionRegistry))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func requireInstallSettled(t *testing.T, w installWorld, id ingest.SessionID, transcript, metadata string) installSnapshot {
	t.Helper()
	s := snapshotInstall(t, w, id)
	if s.State == nil || s.State.ArtifactHash == nil || s.State.IndexedInputHash == nil || !s.State.PublicationBound || s.State.ContentStatus != ingest.ContentCaptureComplete || len(s.Entries) == 0 || slices.Contains(s.Repair, id) {
		t.Fatalf("session is not settled: %+v repair=%v entries=%d", s.State, s.Repair, len(s.Entries))
	}
	a, err := ingest.NewManagedArtifact(readInstallFile(t, metadata), readInstallFile(t, transcript))
	if err != nil {
		t.Fatal(err)
	}
	versions := ingest.NativeGenerationTargets(ingest.HarvesterVersionRegistry)[defaults.HarnessClaudeCode]
	if a.ArtifactHash != *s.State.ArtifactHash || a.Metadata.SchemaVersion != ingest.CurrentSchemaVersion || a.Metadata.AdapterVersion == nil || *a.Metadata.AdapterVersion != versions.AdapterVersion || s.State.IndexerVersion != versions.IndexerVersion {
		t.Fatalf("pair/producer state does not match: %+v %+v", a.Metadata, s.State)
	}
	return s
}

func setupInstallWorld(t *testing.T, fixture interruptedPairFixtures) installWorld {
	t.Helper()
	dir := t.TempDir()
	w := installWorld{dir: dir, output: filepath.Join(dir, "managed"), config: filepath.Join(dir, "config.yaml")}
	source := filepath.Join(dir, "native", "projects")
	project := filepath.Join(dir, "synthetic-project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "-synthetic-project"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []ingest.SessionID{fixture.Target, fixture.Peer} {
		data := strings.ReplaceAll(string(redactionPolicyTranscriptTemplate), recordedWorkingDirectoryPlaceholder, project)
		data = strings.ReplaceAll(data, recordedSessionIDPlaceholder, string(id))
		path := filepath.Join(source, "-synthetic-project", string(id)+".jsonl")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		clock := time.Now().Add(-time.Hour).Truncate(time.Second)
		if err := os.Chtimes(path, clock, clock); err != nil {
			t.Fatal(err)
		}
		if id == fixture.Target {
			w.native, w.nativeBytes, w.nativeClock = path, []byte(data), clock
		}
	}
	config := fmt.Sprintf("version: 1\nsources:\n  claude-code:\n    enabled: true\n    paths: [%q]\n  opencode:\n    enabled: false\n  codex:\n    enabled: false\n  cursor:\n    enabled: false\n  strike:\n    enabled: false\n  pi:\n    enabled: false\noutput:\n  basePath: %q\nredaction:\n  level: standard\n", source, w.output)
	if err := os.WriteFile(w.config, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	result, _ := runInstallCommand(t, w, "harvest", &ingest.OSFileSystem{})
	for _, s := range result.Sessions {
		if s.Error != "" {
			t.Fatal(s.Error)
		}
		if s.SessionID == fixture.Target {
			w.transcript = filepath.Join(s.OutputPath, string(fixture.Target)+"--transcript.jsonl")
			w.metadata = filepath.Join(s.OutputPath, string(fixture.Target)+defaults.MetadataSuffix)
		}
		if s.SessionID == fixture.Peer {
			w.peerTranscript = filepath.Join(s.OutputPath, string(fixture.Peer)+"--transcript.jsonl")
			w.peerMetadata = filepath.Join(s.OutputPath, string(fixture.Peer)+defaults.MetadataSuffix)
		}
	}
	if w.transcript == "" || w.peerTranscript == "" {
		t.Fatalf("initial harvest missed fixture sessions: %+v", result)
	}
	return w
}

// Optional reader APIs are overridden explicitly so embedding OSFileSystem
// cannot conceal peer I/O. Inventory callback reads are classified separately.
type installCounts struct {
	MetadataInventory, MetadataOutside, Transcript, PeerVisits int
	WalkRoots                                                  map[string]int
	TargetRenames                                              int
}

type countingInstallFS struct {
	*ingest.OSFileSystem
	w           installWorld
	mu          sync.Mutex
	inInventory bool
	counts      installCounts
}

var _ ingest.FileSystem = (*countingInstallFS)(nil)

func newCountingInstallFS(w installWorld) *countingInstallFS {
	return &countingInstallFS{OSFileSystem: &ingest.OSFileSystem{}, w: w, counts: installCounts{WalkRoots: make(map[string]int)}}
}
func (f *countingInstallFS) read(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if path == f.w.peerMetadata {
		if f.inInventory {
			f.counts.MetadataInventory++
		} else {
			f.counts.MetadataOutside++
		}
	}
	if path == f.w.peerTranscript {
		f.counts.Transcript++
	}
}
func (f *countingInstallFS) ReadFile(path string) ([]byte, error) {
	f.read(path)
	return f.OSFileSystem.ReadFile(path)
}
func (f *countingInstallFS) ReadFileHeader(path string, limit int) ([]byte, error) {
	f.read(path)
	return f.OSFileSystem.ReadFileHeader(path, limit)
}
func (f *countingInstallFS) Open(path string) (io.ReadCloser, error) {
	f.read(path)
	return f.OSFileSystem.Open(path)
}
func (f *countingInstallFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	f.mu.Lock()
	f.counts.WalkRoots[root]++
	f.mu.Unlock()
	return f.OSFileSystem.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		f.mu.Lock()
		if path == f.w.peerMetadata || path == f.w.peerTranscript {
			f.counts.PeerVisits++
		}
		f.inInventory = root == f.w.output
		f.mu.Unlock()
		defer func() { f.mu.Lock(); f.inInventory = false; f.mu.Unlock() }()
		return fn(path, entry, err)
	})
}
func (f *countingInstallFS) Rename(src, dst string) error {
	f.mu.Lock()
	if dst == f.w.transcript || dst == f.w.metadata {
		f.counts.TargetRenames++
	}
	f.mu.Unlock()
	return f.OSFileSystem.Rename(src, dst)
}

func requireNoExtraPeerIO(t *testing.T, baseline, actual installCounts, w installWorld) {
	t.Helper()
	if actual.Transcript != 0 || actual.MetadataInventory > baseline.MetadataInventory || actual.MetadataOutside > baseline.MetadataOutside || actual.PeerVisits > baseline.PeerVisits {
		t.Fatalf("repair added peer I/O: baseline=%+v actual=%+v", baseline, actual)
	}
	for root, n := range actual.WalkRoots {
		// Only target-local staging and install walks can be added.
		if root == filepath.Dir(w.transcript) || strings.HasPrefix(filepath.Base(root), defaults.TempDirPrefix) {
			continue
		}
		if n > baseline.WalkRoots[root] {
			t.Fatalf("additional non-target walk %s: baseline=%v actual=%v", root, baseline.WalkRoots, actual.WalkRoots)
		}
	}
}

func TestInterruptedPairInstallMounted(t *testing.T) {
	f := loadInterruptedPairFixtures(t)
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			for _, command := range c.Commands {
				w := setupInstallWorld(t, f)
				before := requireInstallSettled(t, w, f.Target, w.transcript, w.metadata)
				peer := requireInstallSettled(t, w, f.Peer, w.peerTranscript, w.peerMetadata)
				t0, m0 := readInstallFile(t, w.transcript), readInstallFile(t, w.metadata)
				pt, pm := readInstallFile(t, w.peerTranscript), readInstallFile(t, w.peerMetadata)
				baselineFS := newCountingInstallFS(w)
				runInstallCommand(t, w, command, baselineFS)
				changed := bytes.ReplaceAll(w.nativeBytes, []byte(f.OriginalText), []byte(f.ReplacementText))
				if bytes.Equal(changed, w.nativeBytes) {
					t.Fatal("native mutation was vacuous")
				}
				if err := os.WriteFile(w.native, changed, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(w.native, w.nativeClock.Add(time.Minute), w.nativeClock.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
				if c.Seam == refusePreparation {
					db := installDB(t, w)
					conn, err := db.Pool().Take(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					err = sqlitex.ExecuteTransient(conn, fmt.Sprintf(`CREATE TRIGGER refuse_target_prepare BEFORE UPDATE OF indexed_input_hash ON sessions WHEN OLD.session_id = '%s' AND OLD.indexed_input_hash IS NOT NULL AND NEW.indexed_input_hash IS NULL BEGIN SELECT RAISE(FAIL, 'synthetic preparation refusal'); END`, f.Target), nil)
					db.Pool().Put(conn)
					_ = db.Close()
					if err != nil {
						t.Fatal(err)
					}
					counter := newCountingInstallFS(w)
					result, _ := runInstallCommand(t, w, command, counter)
					found := false
					for _, s := range result.Sessions {
						if s.SessionID == f.Target && strings.Contains(s.Error, "synthetic preparation refusal") && strings.Contains(s.Error, "preserved") {
							found = true
						}
					}
					if !found || counter.counts.TargetRenames != 0 {
						t.Fatalf("preparation refusal not respected: %+v renames=%d", result, counter.counts.TargetRenames)
					}
					after := snapshotInstall(t, w, f.Target)
					if !reflect.DeepEqual(before, after) || !bytes.Equal(t0, readInstallFile(t, w.transcript)) || !bytes.Equal(m0, readInstallFile(t, w.metadata)) {
						t.Fatal("preparation refusal changed saved data")
					}
					continue
				}
				installChild(t, w, "harvest", c.Seam)
				tornT, tornM := readInstallFile(t, w.transcript), readInstallFile(t, w.metadata)
				if c.Seam == beforeTranscript {
					if !bytes.Equal(tornT, t0) || !bytes.Equal(tornM, m0) {
						t.Fatal("pre-rename changed installed pair")
					}
				} else {
					want := bytes.ReplaceAll(t0, []byte(f.OriginalText), []byte(f.ReplacementText))
					if !bytes.Equal(tornT, want) || bytes.Equal(tornT, t0) {
						t.Fatal("real rename did not install expected T1")
					}
					if c.Seam == afterTranscript {
						if !bytes.Equal(tornM, m0) {
							t.Fatal("metadata renamed before SIGKILL")
						}
						var old ingest.UnifiedMetadata
						if err := json.Unmarshal(m0, &old); err != nil {
							t.Fatal(err)
						}
						if old.ContentHash == schema.ComputeTranscriptHash(tornT) {
							t.Fatal("T1/M0 checksum must disagree")
						}
					} else if _, err := ingest.NewManagedArtifact(tornM, tornT); err != nil {
						t.Fatal("new complete pair invalid", err)
					}
				}
				crashed := snapshotInstall(t, w, f.Target)
				if crashed.State.IndexedInputHash != nil {
					t.Fatalf("after confirmed real rename and SIGKILL indexed_input_hash is still non-NULL: %s", *crashed.State.IndexedInputHash)
				}
				if !slices.Contains(crashed.Repair, f.Target) {
					t.Fatal("interrupted target absent from repair selection")
				}
				if !reflect.DeepEqual(crashed.State.ArtifactHash, before.State.ArtifactHash) || !reflect.DeepEqual(crashed.Entries, before.Entries) {
					t.Fatal("crash changed prior database artifact or entries")
				}
				// Reset BOTH native fingerprint and clock, not the saved pair or SQL.
				if c.RemoveSource {
					if err := os.Remove(w.native); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(w.native, w.nativeBytes, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(w.native, w.nativeClock, w.nativeClock); err != nil {
						t.Fatal(err)
					}
				}
				var result jsonPipelineResult
				var diagnostics string
				if c.CountPeer {
					counter := newCountingInstallFS(w)
					result, diagnostics = runInstallCommand(t, w, command, counter)
					requireNoExtraPeerIO(t, baselineFS.counts, counter.counts, w)
				} else {
					result, diagnostics = installChild(t, w, command, "")
				}
				if c.RemoveSource {
					wantDiagnostic := fmt.Sprintf("session %s has no usable saved copy in peasant-sync/ and its original source %q is unavailable, so it could not be re-ingested", f.Target, w.native)
					if !strings.Contains(diagnostics, wantDiagnostic) {
						t.Fatalf("missing actionable pair repair report: %s", diagnostics)
					}
					after := snapshotInstall(t, w, f.Target)
					if !reflect.DeepEqual(crashed, after) || !bytes.Equal(tornT, readInstallFile(t, w.transcript)) || !bytes.Equal(tornM, readInstallFile(t, w.metadata)) {
						t.Fatal("unavailable repair changed saved pair or stored entries")
					}
				} else {
					for _, s := range result.Sessions {
						if s.Error != "" {
							t.Fatal("recovery session error", s.Error)
						}
					}
					after := requireInstallSettled(t, w, f.Target, w.transcript, w.metadata)
					if !bytes.Equal(t0, readInstallFile(t, w.transcript)) || !reflect.DeepEqual(before.Entries, after.Entries) {
						t.Fatal("recovery did not restore authoritative native T0 and entries")
					}
					if c.Seam == beforeTranscript && !bytes.Equal(m0, readInstallFile(t, w.metadata)) {
						t.Fatal("usable old pair was rewritten")
					}
					quietFS := newCountingInstallFS(w)
					quiet, _ := runInstallCommand(t, w, command, quietFS)
					if quietFS.counts.TargetRenames != 0 {
						t.Fatal("settled target was rewritten")
					}
					for _, s := range quiet.Sessions {
						if s.Error != "" {
							t.Fatal(s.Error)
						}
					}
					if c.CountPeer {
						requireNoExtraPeerIO(t, baselineFS.counts, quietFS.counts, w)
					}
				}
				peerAfter := snapshotInstall(t, w, f.Peer)
				if !reflect.DeepEqual(peer.State, peerAfter.State) || !reflect.DeepEqual(peer.Entries, peerAfter.Entries) || !bytes.Equal(pt, readInstallFile(t, w.peerTranscript)) || !bytes.Equal(pm, readInstallFile(t, w.peerMetadata)) {
					t.Fatal("unaffected peer changed")
				}
			}
		})
	}
}
