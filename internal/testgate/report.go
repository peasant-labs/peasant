package testgate

import (
	"encoding/json"
	"os"
)

// Report is the machine-readable gate report written to <out>/report.json.
// It is the document later slices read; the field names are the contract.
type Report struct {
	SchemaVersion    int             `json:"schema_version"`
	Module           string          `json:"module"`
	Race             bool            `json:"race"`
	Concurrency      int             `json:"concurrency"`
	GOMAXPROCS       int             `json:"gomaxprocs"`
	ListWallMS       int64           `json:"list_wall_ms"`
	PassA            *PassReport     `json:"pass_a,omitempty"`
	PassB            *PassReport     `json:"pass_b,omitempty"`
	CombinedWallMS   int64           `json:"combined_test_wall_ms"`
	PreTestWallMS    int64           `json:"pre_test_wall_ms,omitempty"`
	Calibration      Calibration     `json:"calibration"`
	Records          []ReportRecord  `json:"records"`
	Findings         []ReportFinding `json:"findings"`
	FailedTests      []ReportTest    `json:"failed_tests"`
	InvocationErrors []string        `json:"invocation_errors"`
}

// PassReport summarizes one pass.
type PassReport struct {
	Name     string `json:"name"`
	WallMS   int64  `json:"wall_ms"`
	Packages int    `json:"packages"`
	Tests    int    `json:"tests"`
}

// Calibration reports the load factor L and its inputs.
type Calibration struct {
	L            float64 `json:"l"`
	ProbeMS      int64   `json:"probe_ms"`
	ReferenceMS  int64   `json:"reference_ms"`
	Inconclusive bool    `json:"inconclusive"`
}

// ReportRecord is one per-invocation timing record.
type ReportRecord struct {
	Unit     string `json:"unit"`
	Class    string `json:"class"`
	Pass     string `json:"pass"`
	WallMS   int64  `json:"wall_ms"`
	UserMS   int64  `json:"user_ms"`
	SystemMS int64  `json:"system_ms"`
}

// ReportTest names a failed test.
type ReportTest struct {
	Package string `json:"package"`
	Test    string `json:"test"`
}

// ReportFinding is one screen finding.
type ReportFinding struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	What     string `json:"what"`
	Why      string `json:"why"`
	Where    string `json:"where"`
	When     string `json:"when"`
	Means    string `json:"means"`
	Fix      string `json:"fix"`
}

// WriteReport writes the report as indented JSON.
func WriteReport(path string, report Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
