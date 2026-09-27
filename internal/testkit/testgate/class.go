// Package testgate implements the two-pass test gate: it partitions the suite
// into a race pass and a no-race pass, executes both, merges the streams, and
// applies the exactly-once screen.
//
// The registry (no-race-partition.yaml) is the admission record for the
// partition. Every entry carries auditable evidence, and an entry whose
// production code runs in a child must prove the child is built without the
// race detector.
package testgate

// Class is the cost class of a test unit. The registry admits only RegistryClasses
// (the four cost classes); a runtime record may additionally carry ClassRace for
// the race pass, or a pre-test step name. This is one closed set, not a second
// taxonomy: the registry simply restricts it to the classes it can admit.
type Class string

const (
	// Registry cost classes (the closed set a registry entry may use).
	ClassSingleThreadedBytes Class = "single-threaded-bytes"
	ClassSubprocess          Class = "subprocess"
	ClassStaticAnalysis      Class = "static-analysis"
	ClassToolchain           Class = "toolchain"

	// ClassRace is the race pass's default class. It is not a cost class and a
	// registry entry may not declare it.
	ClassRace Class = "race"
)

// RegistryClasses is the closed set of classes an entry may declare, in the
// order a report presents them.
var RegistryClasses = []Class{
	ClassSingleThreadedBytes,
	ClassSubprocess,
	ClassStaticAnalysis,
	ClassToolchain,
}

// PreTestStep names one of the steps `make check` runs before the test passes.
// The gate records each as its own unit so the pre-test wall stays separate
// from the test wall.
type PreTestStep string

const (
	StepFmt          PreTestStep = "fmt"
	StepLint         PreTestStep = "lint"
	StepAstGrepScan  PreTestStep = "ast-grep-scan"
	StepAstGrepGate  PreTestStep = "ast-grep-gate-test"
	StepReleaseGuard PreTestStep = "release-guard"
)

// PreTestSteps is the closed set of pre-test steps, in execution order.
var PreTestSteps = []PreTestStep{
	StepFmt,
	StepLint,
	StepAstGrepScan,
	StepAstGrepGate,
	StepReleaseGuard,
}

// PreTestClass returns the record class for a pre-test step.
func PreTestClass(s PreTestStep) Class { return Class("pre:" + string(s)) }

// IsRegistryClass reports whether c is one of the four registry cost classes.
func (c Class) IsRegistryClass() bool {
	for _, k := range RegistryClasses {
		if c == k {
			return true
		}
	}
	return false
}

// IsRecordClass reports whether c may appear on a runtime record.
func (c Class) IsRecordClass() bool {
	if c == ClassRace || c.IsRegistryClass() {
		return true
	}
	for _, s := range PreTestSteps {
		if c == PreTestClass(s) {
			return true
		}
	}
	return false
}

// RegistryClassNames returns the registry class names as strings, for messages
// and fixtures.
func RegistryClassNames() []string {
	out := make([]string, 0, len(RegistryClasses))
	for _, c := range RegistryClasses {
		out = append(out, string(c))
	}
	return out
}
