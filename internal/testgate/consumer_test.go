// Package testgate_test is an external consumer of the gate contract: it uses
// only exported names, proving a downstream slice can compile against the
// record and report shapes without reading the gate's implementation.
package testgate_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/testgate"
)

func TestConsumer_UsesExportedRecordAndReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	report := testgate.Report{
		SchemaVersion:  1,
		Module:         "github.com/peasant-labs/peasant",
		CombinedWallMS: 42,
		Records: []testgate.ReportRecord{
			{Unit: "internal/example", Class: string(testgate.ClassToolchain), Pass: testgate.ModeRace.String(), WallMS: 10},
		},
	}
	if err := testgate.WriteReport(path, report); err != nil {
		t.Fatalf("write report: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		SchemaVersion int `json:"schema_version"`
		Records       []struct {
			Unit   string `json:"unit"`
			Class  string `json:"class"`
			Pass   string `json:"pass"`
			WallMS int64  `json:"wall_ms"`
		} `json:"records"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if got.SchemaVersion != 1 || len(got.Records) != 1 || got.Records[0].Unit != "internal/example" {
		t.Fatalf("report did not carry the contract fields: %s", data)
	}
}
