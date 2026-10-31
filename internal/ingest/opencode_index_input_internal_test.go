package ingest

import (
	"context"
	_ "embed"
	"errors"
	"strings"
	"testing"
)

//go:embed testdata/opencode_cancel_unmarked.yaml
var openCodeCancelUnmarkedYAML []byte

// cancelOnCall is live for the first after Err calls, then cancelled. The
// part case needs the message record to succeed and the part record to see
// the cancellation; a context that is already cancelled never reaches the part.
type cancelOnCall struct {
	context.Context
	calls int
	after int
}

func (c *cancelOnCall) Err() error {
	c.calls++
	if c.calls > c.after {
		return context.Canceled
	}
	return c.Context.Err()
}

// TestCancelledOpenCodeRecordStaysUnmarked holds the OpenCode read boundary
// the settle uses. A cancelled message and a cancelled part both stay
// unmarked: the bytes are not what the user has to repair.
func TestCancelledOpenCodeRecordStaysUnmarked(t *testing.T) {
	var fixtures struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name        string `yaml:"name"`
			Record      string `yaml:"record"`
			MessageName string `yaml:"message_name"`
			Message     string `yaml:"message"`
			PartName    string `yaml:"part_name"`
			Part        string `yaml:"part"`
		} `yaml:"cases"`
	}
	decodeOneFixture(t, openCodeCancelUnmarkedYAML, &fixtures, "OpenCode cancellation fixture")
	names := make(map[string]bool)
	for _, testCase := range fixtures.Cases {
		if testCase.Name == "" || names[testCase.Name] {
			t.Fatalf("empty or duplicate OpenCode cancellation case %q", testCase.Name)
		}
		names[testCase.Name] = true
		switch testCase.Record {
		case "message", "part":
		default:
			t.Fatalf("case %s names unknown record %q", testCase.Name, testCase.Record)
		}
		if strings.TrimSpace(testCase.MessageName) == "" || strings.TrimSpace(testCase.Message) == "" {
			t.Fatalf("case %s has no message", testCase.Name)
		}
		if testCase.Record == "part" && (strings.TrimSpace(testCase.PartName) == "" || strings.TrimSpace(testCase.Part) == "") {
			t.Fatalf("case %s has no part", testCase.Name)
		}
	}
	requireNamedCases(t, "OpenCode cancellation fixture", fixtures.Required, names)

	for _, testCase := range fixtures.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			after := 0
			if testCase.Record == "part" {
				after = 1
			}
			message := openCodeJSONMessage{File: openCodeJSONFile{Name: testCase.MessageName, Data: []byte(testCase.Message)}}
			if testCase.Record == "part" {
				message.Parts = []openCodeJSONFile{{Name: testCase.PartName, Data: []byte(testCase.Part)}}
			}
			_, err := parseOpenCodeJSONInput(&openCodeJSONInput{Messages: []openCodeJSONMessage{message}}, &indexCompletion{
				ctx:     &cancelOnCall{Context: context.Background(), after: after},
				session: DiscoveredSession{SessionID: SessionID("11111111-1111-4111-8111-111111111111"), Harness: HarnessOpenCode},
			})
			if err == nil {
				t.Fatal("cancelled read was accepted")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled %s returned %v, want context.Canceled", testCase.Record, err)
			}
			if sourceUnavailable(err) {
				t.Fatalf("cancelled %s was marked as a source refusal: %v", testCase.Record, err)
			}
			wantPrefix := "message " + testCase.MessageName + ":"
			if testCase.Record == "part" {
				wantPrefix = "part " + testCase.PartName + ":"
			}
			if !strings.HasPrefix(err.Error(), wantPrefix) {
				t.Fatalf("error %q does not come from the %s loop", err, testCase.Record)
			}
		})
	}
}
