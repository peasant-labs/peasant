package testgate

import (
	"context"
	"time"

	"github.com/peasant-labs/peasant/internal/testkit/teststream"
)

// Compile-time shape pins for the gate contract.
//
// A rename, removal, or retype of any pinned field, constant, method, or free
// function breaks THIS FILE's build. contract_test.go adds the runtime freeze for
// struct tags and field order, which a keyed literal cannot observe. Together
// they make the contract fail loudly before a consumer sees it.
//
// These are compile-time assertions, not test cases: there is no table here and
// nothing to run.

// --- Named types: underlying kinds are part of the contract ---------------

var (
	_ string     = string(Class(""))
	_ int        = int(PassMode(0))
	_ int        = int(Severity(0))
	_ []string   = []string(BuildFlags{})
	_ BuildFlags = []string{}
)

// --- Closed-set constants -------------------------------------------------

var (
	_ Class = ClassSingleThreadedBytes
	_ Class = ClassSubprocess
	_ Class = ClassStaticAnalysis
	_ Class = ClassToolchain
	_ Class = ClassRace

	_ PreTestStep = StepFmt
	_ PreTestStep = StepLint
	_ PreTestStep = StepAstGrepScan
	_ PreTestStep = StepAstGrepGate
	_ PreTestStep = StepReleaseGuard

	_ Severity = SeverityReport
	_ Severity = SeverityFail

	_ BudgetEnforcement = EnforcementBlocking
	_ BudgetEnforcement = EnforcementWarn

	_ PassMode = ModeRace
	_ PassMode = ModeNoRace
)

// --- Frozen struct field sets ---------------------------------------------

var (
	_ = Record{Unit: "", Class: ClassRace, Pass: ModeRace, Wall: 0, User: 0, System: 0}

	_ = Invocation{ImportPath: "", Dir: "", Test: "", Pass: ModeRace, Class: ClassRace, Args: nil, OutputDir: "", StreamPath: "", ErrPath: ""}

	_ = Runner{Root: "", OutDir: "", GoBin: "", Concurrency: 0, Env: nil, SerialPassB: false}

	_ = RunResult{Records: nil, Streams: nil, Walls: nil, Errors: nil, User: 0, System: 0}

	_ = Report{
		SchemaVersion: 0, Module: "", Race: false, Concurrency: 0, GOMAXPROCS: 0,
		ListWallMS: 0, PassA: nil, PassB: nil, CombinedWallMS: 0, PreTestWallMS: 0,
		Calibration: Calibration{}, Records: nil, Findings: nil, FailedTests: nil, InvocationErrors: nil,
		ClassTable: nil, PreTestSteps: nil, BudgetEnforcement: EnforcementBlocking, BudgetWarn: false,
	}
	_ = PassReport{Name: "", WallMS: 0, Packages: 0, Tests: 0, UserMS: 0, SystemMS: 0, GapMS: 0}
	_ = Calibration{L: 0, ProbeMS: 0, ReferenceMS: 0, Inconclusive: false}
	_ = ReportRecord{Unit: "", Class: "", Pass: "", WallMS: 0, UserMS: 0, SystemMS: 0}
	_ = ReportTest{Package: "", Test: ""}
	_ = ReportFinding{Rule: "", Severity: "", What: "", Why: "", Where: "", When: "", Means: "", Fix: ""}
	_ = ClassRow{Class: "", Units: 0, WallMS: 0, UserMS: 0, SystemMS: 0, GapMS: 0, Basis: ""}
	_ = StepMeasurement{Step: StepFmt, Wall: 0, User: 0, System: 0, ExitCode: 0, Failed: false, LogPath: ""}
	_ = PreTestCommand{Step: StepFmt, Program: "", Args: nil, Target: "", Fragment: ""}
	_ = PassSummary{Pass: ModeRace, Wall: 0, User: 0, System: 0, Serialized: false, Units: 0}

	_ = Cost{WallMS: 0, CPUMs: 0}
	_ = Entry{Package: "", Test: "", Class: ClassRace, Evidence: "", Justification: "", Cost: Cost{}, BuildFlags: nil, ExecCommandSite: ""}
	_ = Registry{Version: 0, Partition: nil, Protected: nil}
	_ = Budget{Version: 0, Seconds: 0, Basis: "", Enforcement: EnforcementBlocking}

	_ = Finding{Rule: "", Severity: SeverityFail, What: "", Why: "", Where: "", When: "", Means: "", Fix: ""}
)

// --- Frozen function and method signatures --------------------------------

var (
	_ func(string) (Registry, error)       = LoadRegistry
	_ func([]byte) (Registry, error)       = DecodeRegistry
	_ func(string, Registry) error         = ValidateRegistry
	_ func(string) (ExecAnchor, error)     = ParseExecAnchor
	_ func(string) (EvidenceAnchor, error) = ParseEvidenceAnchor
	_ func(*BuildFlags) bool               = (*BuildFlags).HasRace
	_ func(string) (Budget, bool, error)   = LoadBudget

	_ func(Invocation) string              = Invocation.Unit
	_ func(*RunResult) []teststream.Record = (*RunResult).FailedTests

	_ func(string, string, *Plan, PassMode, bool) []Invocation                  = BuildInvocations
	_ func(*Runner, context.Context, *Plan, PassMode, bool) (*RunResult, error) = (*Runner).Run

	_ func(string) (string, error)                                       = ModulePath
	_ func(string, []string) ([]string, error)                           = ListPackages
	_ func(string, []string) (map[string][]string, time.Duration, error) = ListTests
	_ func(string, string, map[string][]string, Registry) (*Plan, error) = BuildPlan
	_ func(string, string) string                                        = PackageDir
	_ func(Registry, map[string]bool) Registry                           = ScopeRegistry
	_ func([]string) string                                              = RunRegex

	_ func(ScreenInput) []Finding     = Screen
	_ func([]Finding) bool            = Fails
	_ func() (float64, time.Duration) = Calibrate
	_ func(string, Report) error      = WriteReport

	_ func([]PassSummary, []Record) []ClassRow                                        = BuildClassTable
	_ func([]StepMeasurement) []ClassRow                                              = PreTestRows
	_ func(string, int, time.Duration, time.Duration, time.Duration, string) ClassRow = NewClassRow

	_ func([]byte) (PreTestFixture, error)        = LoadPreTestFixture
	_ func() ([]PreTestCommand, error)            = PreTestCommands
	_ func(string, PreTestDocument) error         = WritePreTestDocument
	_ func(string) (PreTestDocument, bool, error) = ReadPreTestDocument
	_ func([]StepMeasurement) []ReportRecord      = StepRecords

	_ func([]string, map[string]time.Duration, int) (BatchPlan, error)                             = PlanBatches
	_ func(context.Context, BatchProfileConfig) (*BatchProfileResult, error)                       = RunBatchProfile
	_ func(string, *BatchProfileResult) error                                                      = WriteProfileDocument
	_ func(context.Context, string, string, string) ([]string, error)                              = ListTestsForPackage
	_ func(context.Context, string, string, []PreTestCommand, []string) ([]StepMeasurement, error) = RunPreTestSteps

	_ func() []string         = RegistryClassNames
	_ func(PreTestStep) Class = PreTestClass
	_ func(Class) bool        = (Class).IsRegistryClass
	_ func(Class) bool        = (Class).IsRecordClass
	_ func(PassMode) string   = (PassMode).String
	_ func(Severity) string   = (Severity).String
	_ func(Finding) string    = (Finding).Render
)
