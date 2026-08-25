package ingest_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
)

//go:embed testdata/opencode_payload_size.yaml
var openCodePayloadSizeData []byte

// openCodePayloadSizeCase names one materialized source and one session in it.
type openCodePayloadSizeCase struct {
	Name          string `yaml:"name"`
	Origin        string `yaml:"origin"`
	Fixture       string `yaml:"fixture"`
	SessionID     string `yaml:"session_id"`
	ExpectPayload bool   `yaml:"expect_payload"`
}

type openCodePayloadSizeDoc struct {
	RequiredCases []string                  `yaml:"required_cases"`
	Cases         []openCodePayloadSizeCase `yaml:"cases"`
}

func loadOpenCodePayloadSizeDoc(t *testing.T) openCodePayloadSizeDoc {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(openCodePayloadSizeData))
	decoder.KnownFields(true)
	var doc openCodePayloadSizeDoc
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode payload size fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatal("payload size fixture must hold exactly one document")
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("payload size fixture declares no required cases")
	}
	present := make(map[string]struct{}, len(doc.Cases))
	for _, testCase := range doc.Cases {
		if testCase.Name == "" || testCase.Origin == "" || testCase.Fixture == "" || testCase.SessionID == "" {
			t.Fatalf("payload size fixture has an incomplete case: %+v", testCase)
		}
		if _, duplicate := present[testCase.Name]; duplicate {
			t.Fatalf("payload size fixture has a duplicate case name %q", testCase.Name)
		}
		present[testCase.Name] = struct{}{}
	}
	for _, name := range doc.RequiredCases {
		if _, ok := present[name]; !ok {
			t.Fatalf("payload size fixture is missing required case %q", name)
		}
	}
	return doc
}

// TestOpenCodeSessionPayloadSizeMatchesThePagedRows proves the probe reports
// exactly the rows and payload bytes a materialization of the same session
// would load. The probe drives the preview's decision to bound itself, so a
// probe that disagreed with the paged read would bound the wrong sessions.
func TestOpenCodeSessionPayloadSizeMatchesThePagedRows(t *testing.T) {
	doc := loadOpenCodePayloadSizeDoc(t)
	for _, testCase := range doc.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, testCase.Fixture)
			before := testfixture.SnapshotSource(t, materialized)
			source := openSyntheticSource(t, materialized, ingest.DefaultOpenCodeSQLiteSourceOptions())
			var probed ingest.OpenCodePayloadSize
			var pagedRows, pagedBytes int64
			switch testCase.Origin {
			case "legacy":
				probed, pagedRows, pagedBytes = legacyPayloadSizeAndPagedRows(t, source, testCase.SessionID)
			case "current":
				probed, pagedRows, pagedBytes = currentPayloadSizeAndPagedRows(t, source, testCase.SessionID)
			default:
				t.Fatalf("payload size case %q has an unsupported origin %q", testCase.Name, testCase.Origin)
			}
			if probed.Rows != pagedRows || probed.Bytes != pagedBytes {
				t.Errorf("probe reported %d rows / %d bytes, but the paged read loaded %d rows / %d bytes", probed.Rows, probed.Bytes, pagedRows, pagedBytes)
			}
			if testCase.ExpectPayload && (probed.Rows == 0 || probed.Bytes == 0) {
				t.Errorf("session %q was expected to hold payload rows, but the probe reported %+v", testCase.SessionID, probed)
			}
			if !testCase.ExpectPayload && probed != (ingest.OpenCodePayloadSize{}) {
				t.Errorf("session %q holds no rows in the source, but the probe reported %+v", testCase.SessionID, probed)
			}
			closeSyntheticSource(t, source)
			testfixture.AssertUnchanged(t, materialized, before)
		})
	}
}

func legacyPayloadSizeAndPagedRows(t *testing.T, source ingest.OpenCodeSQLiteSource, sessionID string) (ingest.OpenCodePayloadSize, int64, int64) {
	t.Helper()
	legacyID := mustLegacySessionID(t, sessionID)
	probed, err := source.LegacySessionPayloadSize(t.Context(), legacyID)
	if err != nil {
		t.Fatalf("probe legacy session payload size: %v", err)
	}
	// A legacy materialization reads the session's message rows as well as its
	// part rows, so the probe must account for both. Counting parts alone would
	// under-report a session whose payload sits in its message table.
	var rows, payloadBytes int64
	var messageCursor *ingest.OpenCodeLegacyMessageCursor
	for {
		page, pageErr := source.LegacyMessages(t.Context(), ingest.OpenCodeLegacyMessagePageRequest{SessionID: legacyID, PageSize: mustLegacyPageSize(t, 2), After: messageCursor})
		if pageErr != nil {
			t.Fatalf("page legacy session messages: %v", pageErr)
		}
		for _, message := range page.Messages {
			rows++
			payloadBytes += int64(len(message.Data))
		}
		if page.Next == nil {
			break
		}
		messageCursor = page.Next
	}
	var cursor *ingest.OpenCodeLegacyPartCursor
	for {
		page, pageErr := source.LegacySessionParts(t.Context(), ingest.OpenCodeLegacySessionPartPageRequest{SessionID: legacyID, PageSize: mustLegacyPageSize(t, 2), After: cursor})
		if pageErr != nil {
			t.Fatalf("page legacy session parts: %v", pageErr)
		}
		for _, part := range page.Parts {
			rows++
			payloadBytes += int64(len(part.Data))
		}
		// A row the reader could not decode still occupies its payload bytes in
		// the source, so a dropped row must not silently split the two counts.
		rows += int64(len(page.Dropped))
		if page.Next == nil {
			return probed, rows, payloadBytes
		}
		cursor = page.Next
	}
}

func currentPayloadSizeAndPagedRows(t *testing.T, source ingest.OpenCodeSQLiteSource, sessionID string) (ingest.OpenCodePayloadSize, int64, int64) {
	t.Helper()
	currentID := mustCurrentSessionID(t, sessionID)
	probed, err := source.CurrentSessionPayloadSize(t.Context(), currentID)
	if err != nil {
		t.Fatalf("probe current session payload size: %v", err)
	}
	var rows, payloadBytes int64
	var cursor *ingest.OpenCodeCurrentCursor
	for {
		page, pageErr := source.CurrentMessages(t.Context(), ingest.OpenCodeCurrentPageRequest{SessionID: currentID, PageSize: mustCurrentPageSize(t, 2), After: cursor})
		if pageErr != nil {
			t.Fatalf("page current session messages: %v", pageErr)
		}
		for _, message := range page.Messages {
			rows++
			payloadBytes += int64(len(message.Data))
		}
		if page.Next == nil {
			return probed, rows, payloadBytes
		}
		cursor = page.Next
	}
}
