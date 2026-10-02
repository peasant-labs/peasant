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
	// ClassTable is the per-class wall/user/system decision table. The
	// wall − CPU gap is the work a core count cannot compress.
	ClassTable []ClassRow `json:"class_table"`
	// PreTestSteps carries one record per measured pre-test step when the
	// pre-test measurement was fed into this run; empty when only the aggregate
	// pre_test_wall_ms is known.
	PreTestSteps []ReportRecord `json:"pre_test_steps,omitempty"`
	// BudgetEnforcement records the budget mode the gate ran under
	// ("blocking" or "warn"); empty when no budget was committed.
	BudgetEnforcement BudgetEnforcement `json:"budget_enforcement,omitempty"`
	// BudgetWarn is true only when the suite missed its budget and the miss
	// was demoted to a warning under warn-only enforcement.
	BudgetWarn bool `json:"budget_warn,omitempty"`
}

// PassReport summarizes one pass, including its whole-pass child CPU.
type PassReport struct {
	Name     string `json:"name"`
	WallMS   int64  `json:"wall_ms"`
	Packages int    `json:"packages"`
	Tests    int    `json:"tests"`
	UserMS   int64  `json:"user_ms"`
	SystemMS int64  `json:"system_ms"`
	GapMS    int64  `json:"gap_ms"`
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

// WriteReport writes the report as indented JSON. Empty collections are
// normalized to `[]` so a consumer never has to special-case null.
func WriteReport(path string, report Report) error {
	if report.Records == nil {
		report.Records = []ReportRecord{}
	}
	if report.Findings == nil {
		report.Findings = []ReportFinding{}
	}
	if report.FailedTests == nil {
		report.FailedTests = []ReportTest{}
	}
	if report.InvocationErrors == nil {
		report.InvocationErrors = []string{}
	}
	if report.ClassTable == nil {
		report.ClassTable = []ClassRow{}
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
