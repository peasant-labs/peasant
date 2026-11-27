package sourcepreview_test

import (
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/sourcepreview"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
)

//go:embed testdata/segmenter.yaml
var segmenterFixtureData []byte

// segmenterFixturePath names the embedded case file in failure messages.
const segmenterFixturePath = "testdata/segmenter.yaml"

type segmenterCase struct {
	Name     string `yaml:"name"`
	Generate struct {
		Pairs           int `yaml:"pairs"`
		AnswerBytes     int `yaml:"answerBytes"`
		HugeAnswerBytes int `yaml:"hugeAnswerBytes"`
	} `yaml:"generate"`
	Capture   captureOptionsSpec `yaml:"capture"`
	Segmenter struct {
		BudgetBytes        int   `yaml:"budgetBytes"`
		FrameCapBytes      int   `yaml:"frameCapBytes"`
		SerializedCapBytes int64 `yaml:"serializedCapBytes"`
	} `yaml:"segmenter"`
	Drive  string `yaml:"drive"`
	Expect struct {
		MinSegments                int      `yaml:"minSegments"`
		Segments                   int      `yaml:"segments"`
		EachWithinBudget           bool     `yaml:"eachWithinBudget"`
		EqualsContractCut          bool     `yaml:"equalsContractCut"`
		OversizedAloneAndTruncated bool     `yaml:"oversizedAloneAndTruncated"`
		FirstSegmentShort          bool     `yaml:"firstSegmentShort"`
		FirstSegmentHasNewer       bool     `yaml:"firstSegmentHasNewer"`
		RefsContiguous             bool     `yaml:"refsContiguous"`
		LastHasNoNewer             bool     `yaml:"lastHasNoNewer"`
		StillPendingAfterOne       bool     `yaml:"stillPendingAfterOne"`
		SerializedCut              bool     `yaml:"serializedCut"`
		NoticeContains             []string `yaml:"noticeContains"`
		WithinFrameCap             int      `yaml:"withinFrameCap"`
	} `yaml:"expect"`
}

type segmenterFixture struct {
	RequiredNames []string        `yaml:"requiredNames"`
	Cases         []segmenterCase `yaml:"cases"`
}

// segmentRun is what one drive of the segmenter produced.
type segmentRun struct {
	segments  []sourcepreview.Segment
	turns     []ingest.Turn
	seg       *sourcepreview.Segmenter
	cutErr    error
	stillMore bool
}

func writeGenerated(t testing.TB, pairs, answerBytes, hugeAnswerBytes int) onboarding.SessionListing {
	t.Helper()
	lines := generatedClaudeLines(pairs, answerBytes)
	if hugeAnswerBytes > 0 {
		huge := generatedClaudeLines(pairs, hugeAnswerBytes)
		middle := pairs / 2
		lines[2*middle+1] = huge[2*middle+1]
	}
	dir := t.TempDir()
	path := filepath.Join(dir, fixtureSessionID+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write the generated transcript: %v", err)
	}
	return onboarding.SessionListing{SessionID: fixtureSessionID, Harness: "claude-code",
		Source: onboarding.SessionSource{Path: path, Root: dir, Origin: onboarding.SessionSourceOriginFile}}
}

func driveSegments(t *testing.T, c segmenterCase) segmentRun {
	t.Helper()
	listing := writeGenerated(t, c.Generate.Pairs, c.Generate.AnswerBytes, c.Generate.HugeAnswerBytes)
	reader := sourcepreview.NewReader(&ingest.OSFileSystem{}, []onboarding.SessionListing{listing}, sourcepreview.WithGitResolver(testutil.NoGitResolver()))
	capture, page, err := reader.Open(t.Context(), fixtureSessionID, optionsFor(c.Capture))
	if err != nil {
		t.Fatalf("open the capture: %v", err)
	}
	defer capture.Close()
	seg, err := sourcepreview.NewSegmenter("1", sourcepreview.SegmenterOptions{BudgetBytes: c.Segmenter.BudgetBytes,
		FrameCapBytes: c.Segmenter.FrameCapBytes, SerializedCapBytes: c.Segmenter.SerializedCapBytes})
	if err != nil {
		t.Fatalf("build the segmenter: %v", err)
	}
	run := segmentRun{seg: seg}
	push := func(page sourcepreview.Page) {
		run.turns = append(run.turns, page.Turns...)
		seg.Push(page)
	}
	push(page)
	ended := page.Continuation != sourcepreview.ContinuationMore
	switch c.Drive {
	case "drain":
		for !ended {
			page, err = capture.Next(t.Context())
			if err != nil {
				t.Fatalf("read the capture: %v", err)
			}
			push(page)
			ended = page.Continuation != sourcepreview.ContinuationMore
		}
		for {
			segment, ok, err := seg.Next(true)
			if err != nil {
				t.Fatalf("cut a segment: %v", err)
			}
			if !ok {
				break
			}
			run.segments = append(run.segments, segment)
		}
	case "once":
		segment, ok, err := seg.Next(ended)
		if err != nil || !ok {
			t.Fatalf("cut the first segment: ok=%v err=%v", ok, err)
		}
		run.segments = append(run.segments, segment)
		run.stillMore = seg.Pending()
	case "perMore":
		for range 100_000 {
			for !seg.Pending() && !ended {
				page, err = capture.Next(t.Context())
				if err != nil {
					t.Fatalf("read the capture: %v", err)
				}
				push(page)
				ended = page.Continuation != sourcepreview.ContinuationMore
			}
			segment, ok, err := seg.Next(ended)
			if err != nil {
				run.cutErr = err
				break
			}
			if !ok {
				break
			}
			run.segments = append(run.segments, segment)
		}
	default:
		t.Fatalf("unknown drive %q", c.Drive)
	}
	return run
}

func TestSegmenter(t *testing.T) {
	t.Parallel()
	var fixture segmenterFixture
	if err := testutil.DecodeFixtureYAML(segmenterFixtureData, &fixture); err != nil {
		t.Fatalf("decode %s: %v", segmenterFixturePath, err)
	}
	seen := map[string]bool{}
	for _, c := range fixture.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatalf("segmenter fixture has an unnamed or duplicate case %q", c.Name)
		}
		seen[c.Name] = true
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("segmenter fixture is missing required case %q", name)
		}
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			run := driveSegments(t, c)
			checkSegments(t, c, run)
		})
	}
}

func checkSegments(t *testing.T, c segmenterCase, run segmentRun) {
	t.Helper()
	e := c.Expect
	segments := run.segments
	if e.MinSegments > 0 && len(segments) < e.MinSegments {
		t.Fatalf("cut %d segments, want at least %d", len(segments), e.MinSegments)
	}
	if e.Segments > 0 && len(segments) != e.Segments {
		t.Fatalf("cut %d segments, want %d", len(segments), e.Segments)
	}
	for i, segment := range segments {
		if err := segment.Segment.Validate(); err != nil {
			t.Fatalf("segment %d does not satisfy the contract: %v", i, err)
		}
		if segment.Segment.Index != i || segment.Ref.Index != i || segment.Segment.HasOlder != (i > 0) {
			t.Fatalf("segment %d carries index %d, ref %d, hasOlder %v", i, segment.Segment.Index, segment.Ref.Index, segment.Segment.HasOlder)
		}
		if e.EachWithinBudget && len(segment.Segment.Turns) > 1 && segment.Segment.ByteLength > c.Segmenter.BudgetBytes {
			t.Fatalf("segment %d holds %d turns and %d bytes, over the %d-byte budget", i, len(segment.Segment.Turns), segment.Segment.ByteLength, c.Segmenter.BudgetBytes)
		}
		if e.WithinFrameCap > 0 && segment.EncodedBytes > e.WithinFrameCap {
			t.Fatalf("segment %d encodes to %d bytes, over the %d-byte frame cap", i, segment.EncodedBytes, e.WithinFrameCap)
		}
	}
	refs := run.seg.Refs()
	if len(refs) != len(segments) {
		t.Fatalf("the segmenter holds %d refs for %d segments", len(refs), len(segments))
	}
	if e.RefsContiguous {
		for i := range refs {
			if refs[i].Index != i || (i > 0 && refs[i].FirstTurn <= refs[i-1].LastTurn) {
				t.Fatalf("ref %d (%+v) does not follow ref %d", i, refs[i], i-1)
			}
			if refs[i].ConversationTurns == nil || (i > 0 && refs[i].ConversationTurns.Before != refs[i-1].ConversationTurns.Before+refs[i-1].ConversationTurns.Count) {
				t.Fatalf("ref %d carries a conversation span that does not continue the previous one", i)
			}
		}
	}
	if e.LastHasNoNewer {
		for i, segment := range segments {
			if want := i < len(segments)-1; segment.Segment.HasNewer != want {
				t.Fatalf("segment %d of %d has hasNewer %v, want %v", i, len(segments), segment.Segment.HasNewer, want)
			}
		}
	}
	if e.EqualsContractCut {
		want, _ := transcript.CutSegments(run.turns, c.Segmenter.BudgetBytes)
		if asJSON(refs) != asJSON(want) {
			t.Fatalf("the preview cut\n%s\ndiffers from the contract cut of the same turns\n%s", asJSON(refs), asJSON(want))
		}
	}
	if e.OversizedAloneAndTruncated {
		found := false
		for _, segment := range segments {
			if len(segment.Segment.Turns) == 1 && segment.Segment.ByteLength > c.Segmenter.BudgetBytes && segment.Segment.Turns[0].Truncated {
				found = true
			}
		}
		if !found {
			t.Fatalf("no segment holds the oversized turn alone with its text bounded")
		}
	}
	if e.FirstSegmentShort {
		first := segments[0].Segment
		if first.ByteLength >= c.Segmenter.BudgetBytes || run.seg.Pending() {
			t.Fatalf("the first segment holds %d bytes; it should be short of the %d-byte budget", first.ByteLength, c.Segmenter.BudgetBytes)
		}
	}
	if e.FirstSegmentHasNewer && !segments[0].Segment.HasNewer {
		t.Fatalf("the first segment says nothing newer follows, while the capture can still load more")
	}
	if e.StillPendingAfterOne && !run.stillMore {
		t.Fatalf("one request emptied the buffer; it should hand out exactly one segment and keep the rest")
	}
	if e.SerializedCut {
		if !errors.Is(run.cutErr, sourcepreview.ErrSerializedBudget) {
			t.Fatalf("the segmenter ended with %v, want the serialized cap", run.cutErr)
		}
		if run.seg.SerializedBytes() > c.Segmenter.SerializedCapBytes {
			t.Fatalf("the segmenter handed out %d encoded bytes, over the %d-byte cap", run.seg.SerializedBytes(), c.Segmenter.SerializedCapBytes)
		}
		for _, want := range e.NoticeContains {
			if !strings.Contains(run.seg.SerializedCutNotice(), want) {
				t.Fatalf("the serialized cut notice %q does not say %q", run.seg.SerializedCutNotice(), want)
			}
		}
	}
}
