package sourcepreview_test

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/sourcepreview"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	sqlite "github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/capture.yaml
var captureFixtureData []byte

// captureFixturePath names the embedded case file in failure messages.
const captureFixturePath = "testdata/capture.yaml"

// originSpec names how a case's transcript is stored. Besides the listing
// origins, originNone is a session discovery recorded no location for.
type originSpec string

const originNone originSpec = "none"

// actionKind is the closed set of source mutations a case can apply.
type actionKind string

const (
	actionTruncate    actionKind = "truncate"
	actionReplace     actionKind = "replace"
	actionRewrite     actionKind = "rewrite"
	actionAppend      actionKind = "append"
	actionSQLiteTouch actionKind = "sqliteTouch"
	actionClose       actionKind = "close"
	actionCancel      actionKind = "cancel"
)

// fixtureSessionID is the one synthetic session identity every file
// transcript is written under.
const fixtureSessionID = "11111111-2222-4333-8444-555555555555"

type transcriptSpec struct {
	Name     string   `yaml:"name"`
	Producer string   `yaml:"harness"`
	Lines    []string `yaml:"lines"`
	Generate *struct {
		Pairs       int `yaml:"pairs"`
		AnswerBytes int `yaml:"answerBytes"`
	} `yaml:"generate"`
}

type captureOptionsSpec struct {
	FirstReadBytes int64 `yaml:"firstReadBytes"`
	SliceReadBytes int64 `yaml:"sliceReadBytes"`
	SourceCapBytes int64 `yaml:"sourceCapBytes"`
	TurnCap        int   `yaml:"turnCap"`
}

type actionSpec struct {
	AfterPage int        `yaml:"afterPage"`
	Do        actionKind `yaml:"do"`
	Bytes     int64      `yaml:"bytes"`
	// Fraction truncates to that share of the file's size instead of Bytes.
	Fraction float64  `yaml:"fraction"`
	From     string   `yaml:"from"`
	To       string   `yaml:"to"`
	Lines    []string `yaml:"lines"`
}

type captureExpect struct {
	Continuation                    string   `yaml:"continuation"`
	CutReason                       string   `yaml:"cutReason"`
	CloseReason                     string   `yaml:"closeReason"`
	EqualsWholeRead                 bool     `yaml:"equalsWholeRead"`
	EqualsWholeReadText             bool     `yaml:"equalsWholeReadText"`
	PrefixOfWholeRead               bool     `yaml:"prefixOfWholeRead"`
	FirstPageIsProperPrefix         bool     `yaml:"firstPageIsProperPrefix"`
	FirstPageHoldsNoPendingCall     bool     `yaml:"firstPageHoldsNoPendingCall"`
	FirstPageTurns                  int      `yaml:"firstPageTurns"`
	SomeTurnSentPending             bool     `yaml:"someTurnSentPending"`
	NoTurnSentPendingBeforeEnd      bool     `yaml:"noTurnSentPendingBeforeEnd"`
	UniqueAscendingTurns            bool     `yaml:"uniqueAscendingTurns"`
	MinPages                        int      `yaml:"minPages"`
	Pages                           int      `yaml:"pages"`
	Turns                           int      `yaml:"turns"`
	NoticeContains                  []string `yaml:"noticeContains"`
	ErrorContains                   []string `yaml:"errorContains"`
	ToolOutputContains              []string `yaml:"toolOutputContains"`
	ToolOutputAbsent                []string `yaml:"toolOutputAbsent"`
	PartialFromFirstPage            bool     `yaml:"partialFromFirstPage"`
	RetainedEqualsWholeRead         bool     `yaml:"retainedEqualsWholeRead"`
	MinRetained                     int      `yaml:"minRetained"`
	AttachedMetadataEqualsWholeRead bool     `yaml:"attachedMetadataEqualsWholeRead"`
	HeaderMetadataEqualsWholeRead   bool     `yaml:"headerMetadataEqualsWholeRead"`
	SameOnSecondRun                 bool     `yaml:"sameOnSecondRun"`
	RetainedAfterEnd                *int64   `yaml:"retainedAfterEnd"`
}

type captureCase struct {
	Name       string             `yaml:"name"`
	Transcript string             `yaml:"transcript"`
	SQLite     string             `yaml:"sqlite"`
	Origin     originSpec         `yaml:"origin"`
	Producer   string             `yaml:"harness"`
	NoRange    bool               `yaml:"noRange"`
	Options    captureOptionsSpec `yaml:"options"`
	SliceSweep []int64            `yaml:"sliceSweep"`
	Actions    []actionSpec       `yaml:"actions"`
	Expect     captureExpect      `yaml:"expect"`
}

type captureFixture struct {
	RequiredNames []string         `yaml:"requiredNames"`
	Transcripts   []transcriptSpec `yaml:"transcripts"`
	Cases         []captureCase    `yaml:"cases"`
}

func loadCaptureFixture(t *testing.T) (captureFixture, map[string]transcriptSpec) {
	t.Helper()
	var fixture captureFixture
	if err := testutil.DecodeFixtureYAML(captureFixtureData, &fixture); err != nil {
		t.Fatalf("decode %s: %v", captureFixturePath, err)
	}
	transcripts := make(map[string]transcriptSpec, len(fixture.Transcripts))
	for _, spec := range fixture.Transcripts {
		if spec.Name == "" || spec.Producer == "" {
			t.Fatalf("capture fixture has an incomplete transcript: %+v", spec)
		}
		transcripts[spec.Name] = spec
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatalf("capture fixture has an unnamed or duplicate case %q", c.Name)
		}
		seen[c.Name] = true
		if c.Transcript != "" {
			if _, ok := transcripts[c.Transcript]; !ok {
				t.Fatalf("capture case %q names transcript %q, which the fixture does not hold", c.Name, c.Transcript)
			}
		}
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("capture fixture is missing required case %q", name)
		}
	}
	return fixture, transcripts
}

// transcriptBytes renders a transcript spec as the file its harness writes.
func transcriptBytes(spec transcriptSpec) []byte {
	lines := append([]string(nil), spec.Lines...)
	if spec.Generate != nil {
		lines = append(lines, generatedClaudeLines(spec.Generate.Pairs, spec.Generate.AnswerBytes)...)
	}
	var out strings.Builder
	for _, line := range lines {
		out.WriteString(expandPad(line))
		out.WriteByte('\n')
	}
	return []byte(out.String())
}

func expandPad(line string) string {
	for {
		start := strings.Index(line, "{{PAD:")
		if start < 0 {
			return line
		}
		end := strings.Index(line[start:], "}}")
		n, err := strconv.Atoi(line[start+len("{{PAD:") : start+end])
		if err != nil {
			panic(fmt.Sprintf("bad PAD token in %q", line))
		}
		line = line[:start] + strings.Repeat("x", n) + line[start+end+2:]
	}
}

// generatedClaudeLines writes pairs of a question and a padded answer, each
// answer a child of its question, as a long Claude Code session.
func generatedClaudeLines(pairs, answerBytes int) []string {
	if pairs <= 0 {
		return nil
	}
	lines := make([]string, 0, 2*pairs)
	parent := ""
	for i := range pairs {
		user := fmt.Sprintf("u%d", i)
		answer := fmt.Sprintf("a%d", i)
		parentField := ""
		if parent != "" {
			parentField = fmt.Sprintf(`"parentUuid":%q,`, parent)
		}
		lines = append(lines,
			fmt.Sprintf(`{"type":"user","uuid":%q,%s"timestamp":"2026-01-01T00:%02d:%02d.000Z","message":{"role":"user","content":"question %d"}}`, user, parentField, (2*i)/60, (2*i)%60, i),
			fmt.Sprintf(`{"type":"assistant","uuid":%q,"parentUuid":%q,"timestamp":"2026-01-01T00:%02d:%02d.000Z","message":{"role":"assistant","content":[{"type":"text","text":"answer %d %s"}]}}`, answer, user, (2*i+1)/60, (2*i+1)%60, i, strings.Repeat("y", answerBytes)))
		parent = answer
	}
	return lines
}

// wholeFileFS hides the range read, so the capture must read a file whole.
type wholeFileFS struct{ ingest.FileSystem }

// captureRun is everything one drive of a capture produced.
type captureRun struct {
	pages    []sourcepreview.Page
	err      error
	retained int64
}

func (r captureRun) turns() []ingest.Turn {
	var turns []ingest.Turn
	for _, page := range r.pages {
		turns = append(turns, page.Turns...)
	}
	return turns
}

func (r captureRun) last() sourcepreview.Page {
	if len(r.pages) == 0 {
		return sourcepreview.Page{}
	}
	return r.pages[len(r.pages)-1]
}

// source is one case's prepared transcript: where it is, the listing a scan
// would have recorded, and the filesystem the reader sees.
type source struct {
	path    string
	fs      ingest.FileSystem
	listing onboarding.SessionListing
	spec    transcriptSpec
}

func prepareSource(t *testing.T, c captureCase, transcripts map[string]transcriptSpec) source {
	t.Helper()
	switch {
	case c.Origin == originNone:
		return source{fs: &ingest.OSFileSystem{}, listing: onboarding.SessionListing{SessionID: fixtureSessionID, Harness: "claude-code"}}
	case c.SQLite != "":
		session := discoverSQLiteSession(t, c.SQLite, onboarding.SessionSourceOrigin(c.Origin))
		return source{
			path: string(session.SourcePath),
			fs:   &ingest.OSFileSystem{},
			listing: onboarding.SessionListing{SessionID: string(session.SessionID), Harness: string(session.Harness),
				Source: discovery.ListingSource(session)},
		}
	}
	spec := transcripts[c.Transcript]
	dir := t.TempDir()
	path := filepath.Join(dir, fixtureSessionID+".jsonl")
	if err := os.WriteFile(path, transcriptBytes(spec), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	harness := spec.Producer
	if c.Producer != "" {
		harness = c.Producer
	}
	var fs ingest.FileSystem = &ingest.OSFileSystem{}
	if c.NoRange {
		fs = wholeFileFS{fs}
	}
	return source{path: path, fs: fs, spec: spec, listing: onboarding.SessionListing{
		SessionID: fixtureSessionID, Harness: harness,
		Source: onboarding.SessionSource{Path: path, Root: dir, Origin: onboarding.SessionSourceOriginFile},
	}}
}

type noEnv struct{}

func (noEnv) LookupEnv(string) (string, bool) { return "", false }

// discoverSQLiteSession materializes the named synthetic OpenCode database and
// discovers one session of the wanted origin with the production adapter.
func discoverSQLiteSession(t *testing.T, fixtureName string, origin onboarding.SessionSourceOrigin) ingest.DiscoveredSession {
	t.Helper()
	materialized := testfixture.MaterializeByName(t, fixtureName)
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic SQLite root: %v", err)
	}
	adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{}, "latest",
		noEnv{}, &ingest.OSFileSystem{}, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("construct the OpenCode adapter: %v", err)
	}
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("discover the synthetic SQLite session: %v", err)
	}
	for _, session := range discovered {
		if discovery.ListingSource(session).Origin == origin {
			return session
		}
	}
	t.Fatalf("discovery found no %q session among %d", origin, len(discovered))
	return ingest.DiscoveredSession{}
}

func (s source) reader() *sourcepreview.Reader {
	return sourcepreview.NewReader(s.fs, []onboarding.SessionListing{s.listing}, sourcepreview.WithGitResolver(testutil.NoGitResolver()))
}

// drive opens a capture and reads it to its end, applying the case's actions.
func drive(t *testing.T, c captureCase, src source, opts sourcepreview.CaptureOptions) captureRun {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	capture, page, err := src.reader().Open(ctx, src.listing.SessionID, opts)
	if err != nil {
		return captureRun{err: err}
	}
	defer capture.Close()
	run := captureRun{pages: []sourcepreview.Page{page}}
	for {
		for _, action := range c.Actions {
			if action.AfterPage == len(run.pages) {
				applyAction(t, action, src, capture, cancel)
			}
		}
		if run.last().Continuation != sourcepreview.ContinuationMore {
			break
		}
		next, err := capture.Next(t.Context())
		if err != nil {
			run.err = err
			break
		}
		run.pages = append(run.pages, next)
		if len(run.pages) > 10_000 {
			t.Fatalf("the capture handed out more than 10000 pages without ending")
		}
	}
	run.retained = capture.RetainedBytes()
	return run
}

func applyAction(t *testing.T, action actionSpec, src source, capture *sourcepreview.Capture, cancel context.CancelFunc) {
	t.Helper()
	switch action.Do {
	case actionTruncate:
		size := action.Bytes
		if action.Fraction > 0 {
			info, err := os.Stat(src.path)
			if err != nil {
				t.Fatalf("measure the transcript: %v", err)
			}
			size = int64(float64(info.Size()) * action.Fraction)
		}
		if err := os.Truncate(src.path, size); err != nil {
			t.Fatalf("truncate the transcript: %v", err)
		}
	case actionReplace:
		data, err := os.ReadFile(src.path)
		if err != nil {
			t.Fatalf("read the transcript: %v", err)
		}
		next := src.path + ".next"
		if err := os.WriteFile(next, data, 0o600); err != nil {
			t.Fatalf("write the replacement: %v", err)
		}
		if err := os.Rename(next, src.path); err != nil {
			t.Fatalf("replace the transcript: %v", err)
		}
	case actionRewrite:
		data, err := os.ReadFile(src.path)
		if err != nil {
			t.Fatalf("read the transcript: %v", err)
		}
		if len(action.From) != len(action.To) || !strings.Contains(string(data), action.From) {
			t.Fatalf("rewrite %q to %q must keep the size and find the text", action.From, action.To)
		}
		rewritten := strings.Replace(string(data), action.From, action.To, 1)
		file, err := os.OpenFile(src.path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("open the transcript in place: %v", err)
		}
		if _, err := file.WriteAt([]byte(rewritten), 0); err != nil {
			t.Fatalf("rewrite the transcript in place: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close the transcript: %v", err)
		}
	case actionAppend:
		file, err := os.OpenFile(src.path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatalf("open the transcript for append: %v", err)
		}
		for _, line := range action.Lines {
			if _, err := file.WriteString(line + "\n"); err != nil {
				t.Fatalf("append to the transcript: %v", err)
			}
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close the transcript: %v", err)
		}
	case actionSQLiteTouch:
		conn, err := sqlite.OpenConn(src.path, sqlite.OpenReadWrite)
		if err != nil {
			t.Fatalf("open the synthetic database: %v", err)
		}
		defer conn.Close()
		if err := sqlitex.Execute(conn, `UPDATE session_message SET time_updated = time_updated + 1 WHERE session_id = ? AND seq = (SELECT MIN(seq) FROM session_message WHERE session_id = ?)`,
			&sqlitex.ExecOptions{Args: []any{src.listing.SessionID, src.listing.SessionID}}); err != nil {
			t.Fatalf("edit an already read row: %v", err)
		}
		if conn.Changes() != 1 {
			t.Fatalf("editing an already read row changed %d rows, want 1", conn.Changes())
		}
	case actionClose:
		capture.Close()
	case actionCancel:
		cancel()
	default:
		t.Fatalf("unknown action %q", action.Do)
	}
}

// wholeRead is the reference: what the production indexer and projection fold
// the whole transcript to, read at once, before any action changed it.
type wholeRead struct {
	projection transcript.Projection
}

func readWhole(t *testing.T, src source) wholeRead {
	t.Helper()
	session, err := sourcepreview.DiscoveredSessionFor(src.listing)
	if err != nil {
		t.Fatalf("map the listing: %v", err)
	}
	indexer := ingest.NewIndexerRegistry(&ingest.OSFileSystem{}, ingest.IndexerRegistryOptions{FullContent: true})[session.Harness]
	var entries []schema.SessionEntry
	if session.TranscriptOrigin == ingest.TranscriptOriginFile {
		var result ingest.TranscriptCaptureResult
		result, err = indexer.IndexTranscriptForCapture(t.Context(), session)
		entries = result.Entries
	} else {
		adapter := ingest.DefaultAdapterRegistry[session.Harness](&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
		captured, materializeErr := adapter.(ingest.TranscriptMaterializer).MaterializeTranscript(t.Context(), session)
		if materializeErr != nil {
			t.Fatalf("materialize the whole session: %v", materializeErr)
		}
		var result ingest.TranscriptCaptureResult
		result, err = indexer.IndexTranscriptBytesForCapture(t.Context(), session, captured.Data)
		entries = result.Entries
	}
	if err != nil {
		t.Fatalf("index the whole transcript: %v", err)
	}
	projection, err := transcript.EntriesToProjectionValidated(entries, transcript.ProjectionOptions{Harness: session.Harness, BoundedPreview: true})
	if err != nil {
		t.Fatalf("project the whole transcript: %v", err)
	}
	return wholeRead{projection: projection}
}

func digests(turns []ingest.Turn) []string {
	out := make([]string, len(turns))
	for i, turn := range turns {
		encoded, err := json.Marshal(turn)
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(encoded)
		out[i] = fmt.Sprintf("%d:%x", turn.Index, sum[:8])
	}
	return out
}

// texts is a turn signature that survives a change of index space: role,
// content and every tool call's result.
func texts(turns []ingest.Turn) []string {
	out := make([]string, len(turns))
	for i, turn := range turns {
		var b strings.Builder
		fmt.Fprintf(&b, "%s|%s", turn.Role, turn.Content)
		for _, call := range turn.ToolCalls {
			fmt.Fprintf(&b, "|%s=%s", call.ID, call.Result)
		}
		out[i] = b.String()
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func optionsFor(spec captureOptionsSpec) sourcepreview.CaptureOptions {
	return sourcepreview.CaptureOptions{FirstReadBytes: spec.FirstReadBytes, SliceReadBytes: spec.SliceReadBytes,
		SourceCapBytes: spec.SourceCapBytes, TurnCap: spec.TurnCap}
}

func TestCapture(t *testing.T) {
	t.Parallel()
	fixture, transcripts := loadCaptureFixture(t)
	for _, c := range fixture.Cases {
		sweep := c.SliceSweep
		if len(sweep) == 0 {
			sweep = []int64{c.Options.SliceReadBytes}
		}
		for _, slice := range sweep {
			t.Run(fmt.Sprintf("%s/slice-%d", c.Name, slice), func(t *testing.T) {
				t.Parallel()
				src := prepareSource(t, c, transcripts)
				var whole wholeRead
				needsWhole := slices.Contains([]bool{
					c.Expect.EqualsWholeRead,
					c.Expect.EqualsWholeReadText,
					c.Expect.PrefixOfWholeRead,
					c.Expect.FirstPageIsProperPrefix,
					c.Expect.RetainedEqualsWholeRead,
					c.Expect.AttachedMetadataEqualsWholeRead,
					c.Expect.HeaderMetadataEqualsWholeRead,
				}, true)
				if needsWhole {
					whole = readWhole(t, src)
				}
				opts := optionsFor(c.Options)
				opts.SliceReadBytes = slice
				run := drive(t, c, src, opts)
				checkCapture(t, c, src, opts, run, whole)
			})
		}
	}
}

func checkCapture(t *testing.T, c captureCase, src source, opts sourcepreview.CaptureOptions, run captureRun, whole wholeRead) {
	t.Helper()
	e := c.Expect
	if e.CloseReason != "" {
		if run.err == nil {
			t.Fatalf("the capture ended with %s after %d pages, want it closed with %s", run.last().Continuation, len(run.pages), e.CloseReason)
		}
		reason, ok := sourcepreview.ReasonOf(run.err)
		if !ok || reason.String() != e.CloseReason {
			t.Fatalf("the capture failed with %v, want close reason %s", run.err, e.CloseReason)
		}
		if strings.Contains(run.err.Error(), src.path) && src.path != "" {
			t.Fatalf("the capture error names the transcript path: %v", run.err)
		}
		for _, want := range e.ErrorContains {
			if !strings.Contains(fullChain(run.err), want) {
				t.Fatalf("the capture error %q does not say %q", run.err, want)
			}
		}
	} else if run.err != nil {
		t.Fatalf("the capture failed after %d pages: %v", len(run.pages), run.err)
	}
	last := run.last()
	if e.Continuation != "" && last.Continuation.String() != e.Continuation {
		t.Fatalf("the capture ended with %s (%s), want %s", last.Continuation, last.CutReason, e.Continuation)
	}
	if e.CutReason != "" && last.CutReason.String() != e.CutReason {
		t.Fatalf("the capture was cut for %q, want %q", last.CutReason, e.CutReason)
	}
	for _, want := range e.NoticeContains {
		if !strings.Contains(last.Notice, want) {
			t.Fatalf("the last notice %q does not say %q", last.Notice, want)
		}
	}
	if e.MinPages > 0 && len(run.pages) < e.MinPages {
		t.Fatalf("the capture handed out %d pages, want at least %d so the seams are exercised", len(run.pages), e.MinPages)
	}
	if e.Pages > 0 && len(run.pages) != e.Pages {
		t.Fatalf("the capture handed out %d pages, want %d", len(run.pages), e.Pages)
	}
	got := run.turns()
	if e.Turns > 0 && len(got) != e.Turns {
		t.Fatalf("the capture handed out %d turns, want %d", len(got), e.Turns)
	}
	if e.EqualsWholeRead {
		if want := digests(whole.projection.Turns); !equalStrings(digests(got), want) {
			t.Fatalf("the pages concatenate to\n%v\nbut the whole read folds to\n%v", digests(got), want)
		}
	}
	if e.EqualsWholeReadText {
		if want := texts(whole.projection.Turns); !equalStrings(texts(got), want) {
			t.Fatalf("the pages concatenate to\n%q\nbut the whole read folds to\n%q", texts(got), want)
		}
	}
	if e.PrefixOfWholeRead {
		want := digests(whole.projection.Turns)
		if len(got) == 0 || len(got) > len(want) || !equalStrings(digests(got), want[:len(got)]) {
			t.Fatalf("the pages are not a prefix of the whole read: got %v, whole %v", digests(got), want)
		}
	}
	if e.FirstPageIsProperPrefix {
		first := run.pages[0].Turns
		want := digests(whole.projection.Turns)
		if len(first) == 0 || len(first) >= len(want) || !equalStrings(digests(first), want[:len(first)]) {
			t.Fatalf("the first page (%d turns) is not a proper prefix of the whole read (%d turns)", len(first), len(want))
		}
	}
	if e.FirstPageHoldsNoPendingCall {
		for _, turn := range run.pages[0].Turns {
			for _, call := range turn.ToolCalls {
				if call.Result == "" {
					t.Fatalf("the first page handed out call %s before its result was read", call.ID)
				}
			}
		}
		if len(run.pages) < 2 {
			t.Fatalf("the case needs the result in a later read; the capture took one page")
		}
	}
	if e.FirstPageTurns > 0 && len(run.pages[0].Turns) != e.FirstPageTurns {
		t.Fatalf("the first page holds %d turns, want %d", len(run.pages[0].Turns), e.FirstPageTurns)
	}
	if e.SomeTurnSentPending {
		pending := false
		for _, turn := range got {
			for _, call := range turn.ToolCalls {
				if call.Result == "" {
					pending = true
				}
			}
		}
		if !pending {
			t.Fatalf("no turn was handed out with a pending call; the cut did not hand out the turns it held")
		}
	}
	if e.NoTurnSentPendingBeforeEnd {
		for i, page := range run.pages {
			if page.Continuation != sourcepreview.ContinuationMore {
				continue
			}
			for _, turn := range page.Turns {
				for _, call := range turn.ToolCalls {
					if call.Result == "" {
						t.Fatalf("page %d handed out call %s without its result while the capture could still read on", i+1, call.ID)
					}
				}
			}
		}
	}
	if e.UniqueAscendingTurns {
		previous := -1
		for _, turn := range got {
			if turn.Index <= previous {
				t.Fatalf("turn identity %d follows %d: the pages do not share one ascending index space", turn.Index, previous)
			}
			previous = turn.Index
		}
	}
	for _, want := range e.ToolOutputContains {
		if !strings.Contains(strings.Join(texts(got), "\n"), want) {
			t.Fatalf("the pages lost %q", want)
		}
	}
	for _, unwanted := range e.ToolOutputAbsent {
		if strings.Contains(strings.Join(texts(got), "\n"), unwanted) {
			t.Fatalf("the pages show %q, which lies past the extent fixed at open", unwanted)
		}
	}
	if e.PartialFromFirstPage {
		for i, page := range run.pages {
			if page.Session.Diagnostics == nil || !page.Session.Diagnostics.Partial {
				t.Fatalf("page %d is not partial; once a capture is partial every later header stays partial", i+1)
			}
		}
	}
	if e.RetainedEqualsWholeRead || e.MinRetained > 0 {
		var records []schema.RetainedUnknownRecord
		for _, page := range run.pages {
			for _, record := range page.RetainedUnknown {
				records = append(records, record.Record)
			}
		}
		if len(records) < e.MinRetained {
			t.Fatalf("the pages carry %d retained records, want at least %d", len(records), e.MinRetained)
		}
		if e.RetainedEqualsWholeRead && asJSON(records) != asJSON(whole.projection.RetainedUnknown) {
			t.Fatalf("the pages carry retained records\n%v\nbut the whole read projects\n%v", records, whole.projection.RetainedUnknown)
		}
	}
	if e.AttachedMetadataEqualsWholeRead {
		var attached, want []schema.NativeMetadataRecord
		for _, page := range run.pages {
			attached = append(attached, page.NativeMetadata...)
		}
		for _, record := range whole.projection.NativeMetadata {
			if record.Attachment != nil {
				want = append(want, record)
			}
		}
		if len(want) == 0 {
			t.Fatalf("the whole read attaches no metadata, so the case proves nothing")
		}
		if asJSON(attached) != asJSON(want) {
			t.Fatalf("the pages attach metadata\n%v\nbut the whole read attaches\n%v", attached, want)
		}
	}
	if e.HeaderMetadataEqualsWholeRead {
		var want []schema.NativeMetadataRecord
		for _, record := range whole.projection.NativeMetadata {
			if record.Attachment == nil {
				want = append(want, record)
			}
		}
		if len(want) == 0 {
			t.Fatalf("the whole read has no session-level metadata, so the case proves nothing")
		}
		if asJSON(last.Session.NativeMetadata) != asJSON(want) {
			t.Fatalf("the header carries metadata\n%v\nbut the whole read has session-level\n%v", last.Session.NativeMetadata, want)
		}
	}
	if e.SameOnSecondRun {
		again := drive(t, c, src, opts)
		if len(again.pages) != len(run.pages) {
			t.Fatalf("a second capture of the same source took %d pages, the first %d", len(again.pages), len(run.pages))
		}
		for i := range run.pages {
			if !equalStrings(digests(run.pages[i].Turns), digests(again.pages[i].Turns)) {
				t.Fatalf("page %d differs between two captures of the same source", i+1)
			}
		}
	}
	if e.RetainedAfterEnd != nil && run.retained != *e.RetainedAfterEnd {
		t.Fatalf("the capture still retains %d bytes after it ended, want %d", run.retained, *e.RetainedAfterEnd)
	}
}

func fullChain(err error) string {
	var b strings.Builder
	var walk func(error)
	walk = func(err error) {
		if err == nil {
			return
		}
		b.WriteString(err.Error())
		b.WriteByte('\n')
		switch unwrapped := err.(type) {
		case interface{ Unwrap() []error }:
			for _, inner := range unwrapped.Unwrap() {
				walk(inner)
			}
		case interface{ Unwrap() error }:
			walk(unwrapped.Unwrap())
		}
	}
	walk(err)
	return b.String()
}

func asJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
