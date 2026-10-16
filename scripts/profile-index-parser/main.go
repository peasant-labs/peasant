// profile-index-parser measures retained parser exits without opening a store.
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
	"math"
	"os"
	"reflect"
	"runtime"
	"runtime/pprof"
	"slices"
	"sort"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// No embedded interface: an unexpected filesystem call must refuse, not panic
// or reach a native path mentioned by the retained metadata.
type deniedSources struct{}

func (deniedSources) ReadFile(string) ([]byte, error)             { return nil, os.ErrPermission }
func (deniedSources) WriteFile(string, []byte, os.FileMode) error { return os.ErrPermission }
func (deniedSources) MkdirAll(string, os.FileMode) error          { return os.ErrPermission }
func (deniedSources) Stat(string) (os.FileInfo, error)            { return nil, os.ErrPermission }
func (deniedSources) Lstat(string) (os.FileInfo, error)           { return nil, os.ErrPermission }
func (deniedSources) WalkDir(string, fs.WalkDirFunc) error        { return os.ErrPermission }
func (deniedSources) Rename(string, string) error                 { return os.ErrPermission }
func (deniedSources) ReadDir(string) ([]os.DirEntry, error)       { return nil, os.ErrPermission }
func (deniedSources) Remove(string) error                         { return os.ErrPermission }
func (deniedSources) RemoveAll(string) error                      { return os.ErrPermission }
func (deniedSources) CopyFile(string, string, os.FileMode) error  { return os.ErrPermission }

var _ ingest.FileSystem = deniedSources{}

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

type corpusItem struct {
	Metadata       string `json:"metadata"`
	Transcript     string `json:"transcript"`
	Snapshot       string `json:"snapshot,omitempty"`       // Actual captured OpenCodeHistorySnapshot, never synthesized.
	ContentOmitted bool   `json:"contentOmitted,omitempty"` // Actual composition flag, not inferred from transcript text.
	// OpenCode retained SQLite projections require the real recorded origin:
	// 1 = legacy SQLite, 2 = current SQLite. Directory reads are unsupported.
	Origin *int `json:"origin,omitempty"`
}

type input struct {
	meta     schema.UnifiedMetadata
	session  ingest.DiscoveredSession
	data     []byte
	digest   string
	snapshot *fixedSnapshot
}

type fixedSnapshot struct {
	value ingest.OpenCodeHistorySnapshot
}

func (s *fixedSnapshot) Snapshot(context.Context, string) (ingest.OpenCodeHistorySnapshot, error) {
	return s.value, nil
}

var _ ingest.OpenCodeSourceAccess = (*fixedSnapshot)(nil)

type caseResult struct {
	Case         int            `json:"case"`
	Harness      ingest.Harness `json:"harness"`
	InputDigest  string         `json:"inputDigest"`
	OutputDigest string         `json:"outputDigest"`
	Refused      bool           `json:"refused"`
}

type summary struct {
	Harness         ingest.Harness `json:"harness"`
	Cases           int            `json:"cases"`
	Success         int            `json:"success"`
	Refused         int            `json:"refused"`
	Bytes           int64          `json:"bytes"`
	SuccessfulBytes int64          `json:"successfulBytes"`
	BuildSeconds    float64        `json:"buildSeconds,omitempty"`
	AllocatedBytes  uint64         `json:"allocatedBytes,omitempty"`
	Allocations     uint64         `json:"allocations,omitempty"`
}

type report struct {
	Mode      string       `json:"mode"`
	Phase     string       `json:"phase"`
	Cases     []caseResult `json:"cases,omitempty"`
	Summaries []summary    `json:"summaries"`
}

func main() {
	manifest := flag.String("manifest", "", "explicit sandbox metadata/transcript JSON manifest")
	mode := flag.String("mode", "generation", "generation (Claude or captured OpenCode snapshot), capture (strict retained bytes), or entries (compatibility)")
	phase := flag.String("phase", "parity", "parity, timing, or profile; never combined")
	compare := flag.String("compare", "", "baseline parity report to compare exactly (parity only)")
	cpu := flag.String("cpuprofile", "", "scratch CPU profile destination (profile only)")
	profileBudget := flag.Int64("profile-max-bytes", 512<<20, "maximum preloaded profile input bytes; timing/parity load one case at a time")
	flag.Parse()
	if err := run(*manifest, *mode, *phase, *compare, *cpu, *profileBudget); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func loadManifest(manifest string) ([]corpusItem, error) {
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return nil, errors.New("manifest read failed; supply an explicit readable sandbox manifest")
	}
	var items []corpusItem
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return nil, errors.New("manifest decode failed or empty corpus; supply a non-empty JSON array")
	}
	return items, nil
}

func loadInput(item corpusItem, i int, mode string, budget *int64) (*input, error) {
	read := func(path string) ([]byte, error) {
		if budget == nil {
			return os.ReadFile(path)
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, *budget+1))
		if int64(len(data)) > *budget {
			return nil, errors.New("profile preload budget exceeded")
		}
		*budget -= int64(len(data))
		return data, err
	}
	metaBytes, err := read(item.Metadata)
	if err != nil {
		return nil, fmt.Errorf("case %d: metadata read failed; repair the sandbox manifest or raise -profile-max-bytes", i)
	}
	var in input
	if json.Unmarshal(metaBytes, &in.meta) != nil {
		return nil, fmt.Errorf("case %d: metadata decode failed; repair the sandbox input", i)
	}
	if !slices.Contains(schema.Harnesses(), in.meta.ModelHarness) || !in.meta.Source.Format.IsValid() {
		return nil, fmt.Errorf("case %d: invalid harness or source format enum; repair metadata", i)
	}
	id, err := ingest.NewSessionID(in.meta.SessionID.String())
	if err != nil {
		return nil, fmt.Errorf("case %d: invalid session identity; repair metadata", i)
	}
	path, err := ingest.NewResolvedPath(in.meta.Source.FilePath)
	if err != nil {
		return nil, fmt.Errorf("case %d: invalid recorded path; repair metadata", i)
	}
	in.session = ingest.DiscoveredSession{SessionID: id, Harness: in.meta.ModelHarness, SourcePath: path, SourceFormat: in.meta.Source.Format, ParentUUID: in.meta.ParentUUID}
	in.session.ContentOmitted = item.ContentOmitted
	capturedOpenCode := in.session.Harness == ingest.HarnessOpenCode && item.Snapshot != ""
	if mode == "generation" && in.session.Harness != ingest.HarnessClaudeCode && !capturedOpenCode {
		return nil, fmt.Errorf("case %d: native generation acquisition is unsupported; use retained capture mode", i)
	}
	if in.session.Harness == ingest.HarnessOpenCode && mode != "generation" {
		if item.Origin == nil || (*item.Origin != 1 && *item.Origin != 2) {
			return nil, fmt.Errorf("case %d: OpenCode requires recorded SQLite projection origin 1 or 2; directory acquisition is unsupported", i)
		}
		in.session.TranscriptOrigin, err = ingest.NewTranscriptOrigin(*item.Origin)
		if err != nil {
			return nil, fmt.Errorf("case %d: invalid origin; repair manifest", i)
		}
	} else if item.Origin != nil {
		return nil, fmt.Errorf("case %d: origin is only supported for OpenCode retained projections", i)
	}
	in.data, err = read(item.Transcript)
	if err != nil {
		return nil, fmt.Errorf("case %d: transcript read failed; repair the sandbox manifest or raise -profile-max-bytes", i)
	}
	h := sha256.New()
	// Length framing prevents ambiguity between metadata and transcript bytes.
	fmt.Fprintf(h, "%d:", len(metaBytes))
	h.Write(metaBytes)
	fmt.Fprintf(h, "%d:", len(in.data))
	h.Write(in.data)
	originBytes, _ := json.Marshal(item.Origin)
	h.Write(originBytes)
	fmt.Fprintf(h, ":%t:", item.ContentOmitted)
	if item.Snapshot != "" {
		if mode != "generation" || in.session.Harness != ingest.HarnessOpenCode {
			return nil, fmt.Errorf("case %d: snapshot requires OpenCode generation mode", i)
		}
		snapshotBytes, err := read(item.Snapshot)
		if err != nil {
			return nil, fmt.Errorf("case %d: snapshot read failed; supply actual captured source evidence or raise -profile-max-bytes", i)
		}
		in.snapshot = &fixedSnapshot{}
		if json.Unmarshal(snapshotBytes, &in.snapshot.value) != nil {
			return nil, fmt.Errorf("case %d: snapshot decode failed; supply actual captured source evidence", i)
		}
		fmt.Fprintf(h, "%d:", len(snapshotBytes))
		h.Write(snapshotBytes)
	}
	in.digest = fmt.Sprintf("%x", h.Sum(nil))
	return &in, nil
}

func run(manifest, mode, phase, compare, cpu string, profileBudget int64) error {
	if mode != "generation" && mode != "entries" && mode != "capture" {
		return errors.New("use -mode=generation, capture, or entries")
	}
	if phase != "parity" && phase != "timing" && phase != "profile" {
		return errors.New("use -phase=parity, timing, or profile")
	}
	if (phase == "profile") != (cpu != "") || (compare != "" && phase != "parity") {
		return errors.New("use -cpuprofile only and always with profile; use -compare only with parity")
	}
	items, err := loadManifest(manifest)
	if err != nil {
		return err
	}
	indexers := ingest.NewIndexerRegistry(deniedSources{}, ingest.IndexerRegistryOptions{FullContent: true})
	// A single serial worker keeps allocation deltas attributable. Inputs are
	// preloaded only for profiling; timing/parity hold one case's source bytes.
	var preloaded []*input
	if phase == "profile" {
		if profileBudget <= 0 || profileBudget == math.MaxInt64 {
			return errors.New("set a positive, bounded -profile-max-bytes preload budget")
		}
		for i, item := range items {
			in, err := loadInput(item, i, mode, &profileBudget)
			if err != nil {
				return err
			}
			preloaded = append(preloaded, in)
		}
		f, err := os.OpenFile(cpu, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return errors.New("profile creation failed; choose a new writable scratch destination")
		}
		defer f.Close()
		if pprof.StartCPUProfile(f) != nil {
			return errors.New("CPU profile start failed; run in a separate process")
		}
		defer pprof.StopCPUProfile()
	}
	out := report{Mode: mode, Phase: phase}
	byHarness := make(map[ingest.Harness]*summary)
	for i, item := range items {
		var in *input
		if phase == "profile" {
			in = preloaded[i]
		} else {
			in, err = loadInput(item, i, mode, nil)
			if err != nil {
				return err
			}
		}
		idx := indexers[in.session.Harness]
		if idx == nil {
			return fmt.Errorf("case %d: unsupported harness; repair metadata", i)
		}
		builder, supportsGeneration := idx.(ingest.GenerationBuilder)
		if mode == "generation" && !supportsGeneration {
			return fmt.Errorf("case %d: generation builder unavailable; use capture mode", i)
		}
		buildInput := ingest.GenerationBuildInput{Session: in.session, GenerationID: "gen-parse-profile", RecordedMetadata: &in.meta, CapturedTranscript: in.data, Allocator: &refs{}}
		if in.snapshot != nil {
			buildInput.OpenCode = in.snapshot
		}
		var result any
		var buildErr error
		var before, after runtime.MemStats
		var start time.Time
		if phase == "timing" {
			runtime.ReadMemStats(&before)
			start = time.Now()
		}
		if mode == "generation" {
			result, buildErr = builder.BuildGeneration(context.Background(), buildInput)
		} else if mode == "capture" {
			result, buildErr = idx.IndexTranscriptBytesForCapture(context.Background(), in.session, in.data)
		} else {
			result, buildErr = idx.IndexTranscriptBytes(context.Background(), in.session, in.data)
		}
		var elapsed time.Duration
		if phase == "timing" {
			elapsed = time.Since(start)
			runtime.ReadMemStats(&after)
		}
		s := byHarness[in.session.Harness]
		if s == nil {
			s = &summary{Harness: in.session.Harness}
			byHarness[in.session.Harness] = s
		}
		s.Cases++
		s.Bytes += int64(len(in.data))
		if buildErr == nil {
			s.Success++
			s.SuccessfulBytes += int64(len(in.data))
		} else {
			s.Refused++
		}
		s.BuildSeconds += elapsed.Seconds()
		s.AllocatedBytes += after.TotalAlloc - before.TotalAlloc
		s.Allocations += after.Mallocs - before.Mallocs
		if phase == "parity" {
			h := sha256.New()
			// encoding/json sorts map keys; []byte blobs are encoded losslessly.
			// Hash the complete returned value, including partial values on refusal.
			if json.NewEncoder(h).Encode(result) != nil {
				return fmt.Errorf("case %d: output encoding failed; inspect parser return types", i)
			}
			out.Cases = append(out.Cases, caseResult{Case: i, Harness: in.session.Harness, InputDigest: in.digest, OutputDigest: fmt.Sprintf("%x", h.Sum(nil)), Refused: buildErr != nil})
		}
	}
	for _, s := range byHarness {
		out.Summaries = append(out.Summaries, *s)
	}
	sort.Slice(out.Summaries, func(i, j int) bool { return out.Summaries[i].Harness < out.Summaries[j].Harness })
	if compare != "" {
		raw, err := os.ReadFile(compare)
		if err != nil {
			return errors.New("baseline report read failed; supply a readable parity report")
		}
		var baseline report
		if json.Unmarshal(raw, &baseline) != nil || baseline.Phase != "parity" || baseline.Mode != mode {
			return errors.New("baseline report is incompatible; generate parity with the same mode and driver")
		}
		if !reflect.DeepEqual(baseline.Cases, out.Cases) {
			return errors.New("parity mismatch in corpus, complete output, or refusal status; inspect case ordinals in separate parity reports")
		}
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}
