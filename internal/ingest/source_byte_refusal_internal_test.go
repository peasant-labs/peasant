package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
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
	Name    string `yaml:"name"`
	Harness string `yaml:"harness"`
	Kind    string `yaml:"kind"`
	Settles bool   `yaml:"settles"`
	Message string `yaml:"message"`
	// MessageFrom names the production decode whose failure this case pins. The
	// sentence is then rebuilt from that decode at test time instead of being
	// written into the fixture, so a standard-library wording change fails only
	// where the user would see it, and the rest of the sentence still has to
	// match exactly. MessageTemplate is the literal wrapper that decode's
	// failure is printed inside, with one verb for the cause: the wrapper is
	// pinned here rather than copied from production, so a reworded capture
	// template fails even though the cause is rebuilt. A case that sets
	// MessageFrom sets MessageTemplate and must not also set Message.
	MessageFrom     string `yaml:"message_from"`
	MessageTemplate string `yaml:"message_template"`
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
		switch testCase.Kind {
		case "validation", "typed-decode":
			switch testCase.MessageFrom {
			case "":
				if strings.TrimSpace(testCase.Message) == "" {
					t.Fatalf("case %s does not pin the full refusal sentence", testCase.Name)
				}
				if strings.TrimSpace(testCase.MessageTemplate) != "" {
					t.Fatalf("case %s pins a template without naming the decode that fills it", testCase.Name)
				}
			case "pi-message-content", "strike-event-data":
				if strings.TrimSpace(testCase.Message) != "" {
					t.Fatalf("case %s pins both a rebuilt and a literal sentence", testCase.Name)
				}
				if strings.TrimSpace(testCase.MessageTemplate) == "" {
					t.Fatalf("case %s does not pin the refusal wrapper its rebuilt cause is printed inside", testCase.Name)
				}
				if strings.Count(testCase.MessageTemplate, "%") != 1 || (!strings.Contains(testCase.MessageTemplate, "%v") && !strings.Contains(testCase.MessageTemplate, "%s")) {
					t.Fatalf("case %s must carry exactly one %%v or %%s verb for the decoded cause in its message template", testCase.Name)
				}
			default:
				t.Fatalf("case %s names unknown message source %q", testCase.Name, testCase.MessageFrom)
			}
		case "cancelled":
		default:
			if strings.TrimSpace(testCase.MessageContains) == "" {
				t.Fatalf("case %s does not name the sentence the user already sees", testCase.Name)
			}
		}
	}
	requireNamedCases(t, "source-byte refusal fixture", fixture.Required, present)
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
			if sourceUnavailable(err) != testCase.Settles {
				t.Fatalf("sourceUnavailable = %v, want %v; error: %v", sourceUnavailable(err), testCase.Settles, err)
			}
			if testCase.Kind == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled capture returned %v, want context.Canceled", err)
				}
				return
			}
			switch testCase.Kind {
			case "validation", "typed-decode":
				want := testCase.Message
				if testCase.MessageFrom != "" {
					want = rebuiltTypedDecodeSentence(t, testCase.MessageFrom, testCase.MessageTemplate, session, data)
				}
				if err.Error() != want {
					t.Fatalf("message changed\n got %q\nwant %q", err.Error(), want)
				}
			case "unparseable":
				if !strings.Contains(err.Error(), testCase.MessageContains) {
					t.Fatalf("message changed\n got %q\nwant it to contain %q", err.Error(), testCase.MessageContains)
				}
				assertUnmarkedSentence(t, session, data, err)
			default:
				if !strings.Contains(err.Error(), testCase.MessageContains) {
					t.Fatalf("message changed\n got %q\nwant it to contain %q", err.Error(), testCase.MessageContains)
				}
			}
		})
	}
}

// refusalRecords returns the records the capture numbers, in order: it skips
// the empty lines the reader skips, so an index into the result is the record
// number the sentence names.
func refusalRecords(data []byte) [][]byte {
	var records [][]byte
	for rest := data; len(rest) > 0; {
		raw, next, _ := bytes.Cut(rest, []byte{'\n'})
		rest = next
		if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 {
			records = append(records, trimmed)
		}
	}
	return records
}

// rebuiltTypedDecodeSentence rebuilds a typed-decode refusal from the production
// decoder that produced it, so the sentence keeps its exact-equality check while
// the decoder's own wording (which a standard-library upgrade can change) is not
// copied into this fixture. The wrapper around that cause is NOT rebuilt: the
// fixture's template supplies it literally, with one verb for the cause, so a
// reworded capture template fails here instead of moving the expectation with
// it. The record number and the step are part of the pinned template.
func rebuiltTypedDecodeSentence(t *testing.T, from, template string, session DiscoveredSession, data []byte) string {
	t.Helper()
	for index, raw := range refusalRecords(data) {
		line := index + 1
		switch from {
		case "pi-message-content":
			entry, entryErr := decodePiEntry(raw, line)
			if entryErr != nil {
				continue
			}
			var message piMessagePayload
			if err := json.Unmarshal(entry.Message, &message); err != nil {
				continue
			}
			if _, err := decodePiContent(message.Content, message.Role == piRoleAssistant); err != nil {
				return fmt.Sprintf(template, err)
			}
		case "strike-event-data":
			var envelope strikeEnvelope
			if err := json.Unmarshal(raw, &envelope); err != nil {
				continue
			}
			if _, err := decodeStrikeEventData(envelope.Data); err != nil {
				return fmt.Sprintf(template, err)
			}
		default:
			t.Fatalf("unknown typed-decode source %q", from)
		}
	}
	t.Fatalf("%s fixture has no record the production decoder refuses", from)
	return ""
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
		if len(raw) == 0 || json.Valid(raw) {
			continue
		}
		return captureFailure(session, line, malformedSourceRecord{}).Error()
	}
	t.Fatal("Strike fixture has no unparseable record to compare")
	return ""
}
