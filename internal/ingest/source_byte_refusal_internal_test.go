package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/source_byte_refusal.yaml
var sourceByteRefusalYAML []byte

type sourceByteRefusalCase struct {
	Name            string `yaml:"name"`
	Harness         string `yaml:"harness"`
	Kind            string `yaml:"kind"`
	Settles         bool   `yaml:"settles"`
	MessageContains string `yaml:"message_contains"`
	Transcript      string `yaml:"transcript"`
}

type sourceByteRefusalFixture struct {
	Required []string                `yaml:"required_names"`
	Cases    []sourceByteRefusalCase `yaml:"cases"`
}

func loadSourceByteRefusalFixture(t *testing.T) sourceByteRefusalFixture {
	t.Helper()
	var fixture sourceByteRefusalFixture
	decoder := yaml.NewDecoder(bytes.NewReader(sourceByteRefusalYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode source-byte refusal fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("source-byte refusal fixture must hold exactly one document: %v", err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || present[testCase.Name] {
			t.Fatalf("empty or repeated source-byte refusal case %q", testCase.Name)
		}
		present[testCase.Name] = true
		switch testCase.Kind {
		case "unparseable", "validation", "typed-decode", "cancelled", "omitted":
		default:
			t.Fatalf("case %s names unknown kind %q", testCase.Name, testCase.Kind)
		}
		if strings.TrimSpace(testCase.Transcript) == "" {
			t.Fatalf("case %s has no transcript", testCase.Name)
		}
		if testCase.Kind != "cancelled" && strings.TrimSpace(testCase.MessageContains) == "" {
			t.Fatalf("case %s does not name the sentence the user already sees", testCase.Name)
		}
	}
	for _, name := range fixture.Required {
		if !present[name] {
			t.Fatalf("source-byte refusal fixture is missing required case %q", name)
		}
	}
	return fixture
}

// TestSourceByteRefusalKeepsTheSentenceAndLeavesCancellationUnmarked holds the
// capture boundary the settle reads. A refusal about the bytes is marked, and
// the sentence is the one the capture printed before the mark existed. A
// cancelled run and a stored omission flag are not those bytes.
func TestSourceByteRefusalKeepsTheSentenceAndLeavesCancellationUnmarked(t *testing.T) {
	fixture := loadSourceByteRefusalFixture(t)
	sessionID := SessionID("11111111-1111-4111-8111-111111111111")
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			ctx := context.Background()
			if testCase.Kind == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			session := DiscoveredSession{SessionID: sessionID, Harness: Harness(testCase.Harness)}
			if testCase.Kind == "omitted" {
				session.ContentOmitted = true
			}
			data := []byte(strings.ReplaceAll(testCase.Transcript, "SESSION_ID", sessionID.String()))
			var err error
			switch session.Harness {
			case HarnessPi:
				_, err = NewPiIndexer(nil).IndexTranscriptBytesForCapture(ctx, session, data)
			case HarnessStrike:
				_, err = NewStrikeIndexer(nil).IndexTranscriptBytesForCapture(ctx, session, data)
			default:
				t.Fatalf("case %s names a harness this capture check does not own: %s", testCase.Name, testCase.Harness)
			}
			if err == nil {
				t.Fatal("capture accepted bytes it must refuse")
			}
			if sourceRecordUnusable(err) != testCase.Settles {
				t.Fatalf("sourceRecordUnusable = %v, want %v; error: %v", sourceRecordUnusable(err), testCase.Settles, err)
			}
			if testCase.Kind == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled capture returned %v, want context.Canceled", err)
				}
				return
			}
			if !strings.Contains(err.Error(), testCase.MessageContains) {
				t.Fatalf("message changed\n got %q\nwant it to contain %q", err.Error(), testCase.MessageContains)
			}
			if testCase.Kind == "unparseable" {
				assertUnmarkedSentence(t, session, data, err)
			}
		})
	}
}

// assertUnmarkedSentence rebuilds the sentence the capture printed before the
// mark existed and requires the marked error to equal it. The mark must not
// add, drop, or reorder a byte of that sentence.
func assertUnmarkedSentence(t *testing.T, session DiscoveredSession, data []byte, got error) {
	t.Helper()
	switch session.Harness {
	case HarnessPi:
		want := unmarkedPiSentence(t, data)
		if got.Error() != want {
			t.Fatalf("Pi sentence changed\n got %q\nwant %q", got.Error(), want)
		}
	case HarnessStrike:
		want := unmarkedStrikeSentence(t, session, data)
		if got.Error() != want {
			t.Fatalf("Strike sentence changed\n got %q\nwant %q", got.Error(), want)
		}
	}
}

func unmarkedPiSentence(t *testing.T, data []byte) string {
	t.Helper()
	// The production parser reports the first record that is not JSON. Rebuild
	// that same sentence from the validator it calls, without the mark.
	line := 0
	for rest := data; len(rest) > 0; {
		raw, next, _ := bytes.Cut(rest, []byte{'\n'})
		rest = next
		line++
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		if err := schema.ScanRawJSONDocument(raw, schema.RawJSONPathPolicy{MaxDocumentBytes: defaults.MaxJSONLRecordBytes, MaxDocumentDepth: 128}); err != nil {
			return piSourceError("raw validation", line, err).Error()
		}
	}
	t.Fatal("Pi fixture has no unparseable record to compare")
	return ""
}

func unmarkedStrikeSentence(t *testing.T, session DiscoveredSession, data []byte) string {
	t.Helper()
	line := 0
	for rest := data; len(rest) > 0; {
		raw, next, _ := bytes.Cut(rest, []byte{'\n'})
		rest = next
		line++
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || jsonValid(raw) {
			continue
		}
		return captureFailure(session, line, malformedSourceRecord{}).Error()
	}
	t.Fatal("Strike fixture has no unparseable record to compare")
	return ""
}

func jsonValid(raw []byte) bool {
	return len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '{'
}
