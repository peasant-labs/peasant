package testgate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Cost is the observed cost pair for a registered unit, taken from a committed
// measurement. Wall is end-to-end; CPU is user+system, which is the work more
// cores cannot compress and the quantity the budget check divides.
type Cost struct {
	WallMS int64 `yaml:"wall_ms"`
	CPUMs  int64 `yaml:"cpu_ms"`
}

// BuildFlags is a subprocess entry's declared child build flags. A pointer
// distinguishes an explicit empty declaration from an absent field, so a
// subprocess entry that builds a no-flag child must still say `build_flags: []`.
type BuildFlags []string

// Entry is one registered test. Package is a repo-relative package directory;
// Test is a top-level test function name.
type Entry struct {
	Package         string      `yaml:"package"`
	Test            string      `yaml:"test"`
	Class           Class       `yaml:"class"`
	Evidence        string      `yaml:"evidence"`
	Justification   string      `yaml:"justification"`
	Cost            Cost        `yaml:"cost"`
	BuildFlags      *BuildFlags `yaml:"build_flags,omitempty"`
	ExecCommandSite string      `yaml:"exec_command_site,omitempty"`
}

// Registry is the no-race partition registry. Partition entries run in the
// no-race pass; Protected entries must stay in the race pass.
type Registry struct {
	Version   int     `yaml:"version"`
	Partition []Entry `yaml:"partition"`
	Protected []Entry `yaml:"protected"`
}

// LoadRegistry reads and strictly decodes the registry at path.
func LoadRegistry(path string) (Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Registry{}, fmt.Errorf("read registry %s: %w", path, err)
	}
	reg, err := DecodeRegistry(data)
	if err != nil {
		return Registry{}, fmt.Errorf("decode registry %s: %w", path, err)
	}
	return reg, nil
}

// DecodeRegistry strictly decodes a single YAML document into a Registry.
func DecodeRegistry(data []byte) (Registry, error) {
	var reg Registry
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&reg); err != nil {
		return Registry{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Registry{}, errors.New("registry must contain exactly one YAML document")
	}
	return reg, nil
}

// ValidateRegistry checks every admission criterion against the tree rooted at
// root. It returns one error naming every finding, so one run fixes them all.
//
// Admission criteria, per entry: a valid closed-set class; a non-empty
// justification; an evidence anchor that resolves to a test declared in the
// named test's file; an observed cost pair; and, for a subprocess entry, a
// build_flags declaration resolved against the referenced exec.Command site
// that proves the child carries no race detector.
func ValidateRegistry(root string, reg Registry) error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if reg.Version != 1 {
		add("registry version = %d, want 1", reg.Version)
	}

	seen := map[string]string{}
	validateEntry := func(list string, e Entry) {
		where := fmt.Sprintf("%s entry %s/%s", list, e.Package, e.Test)

		if e.Package == "" || e.Test == "" {
			add("%s: package and test are required", where)
			return
		}
		pkgAbs := filepath.Join(root, filepath.FromSlash(e.Package))
		if _, err := os.Stat(pkgAbs); err != nil {
			add("%s: package directory %s does not exist under the repo root", where, e.Package)
		}

		if !e.Class.IsRegistryClass() {
			add("%s: unknown class %q (closed set: %s)", where, e.Class, strings.Join(RegistryClassNames(), ", "))
		}
		if strings.TrimSpace(e.Justification) == "" {
			add("%s: justification is empty", where)
		}
		if e.Cost.WallMS <= 0 || e.Cost.CPUMs <= 0 {
			add("%s: cost must record a positive wall_ms and cpu_ms from a committed measurement", where)
		}

		key := e.Package + "|" + e.Test
		if prev, dup := seen[key]; dup {
			add("%s: duplicate registration (also in %s)", where, prev)
		} else {
			seen[key] = list
		}

		validateEvidence(root, pkgAbs, e, where, add)
		validateSubprocess(root, list, e, where, add)
	}

	for _, e := range reg.Partition {
		validateEntry("partition", e)
	}
	for _, e := range reg.Protected {
		validateEntry("protected", e)
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("registry validation failed:\n  - %s", strings.Join(problems, "\n  - "))
}

func validateEvidence(root, pkgAbs string, e Entry, where string, add func(string, ...any)) {
	anchor, err := ParseEvidenceAnchor(e.Evidence)
	if err != nil {
		add("%s: %v", where, err)
		return
	}
	declFile, err := testDeclFile(pkgAbs, e.Test)
	if err != nil {
		add("%s: %v", where, err)
		return
	}
	evidenceFile, err := testDeclFile(pkgAbs, anchor.Test)
	if err != nil {
		add("%s: evidence anchor #%s resolves to no test in package %s", where, anchor.Test, e.Package)
		return
	}
	declRel, err := filepath.Rel(root, declFile)
	if err != nil {
		declRel = declFile
	}
	evidenceRel, err := filepath.Rel(root, evidenceFile)
	if err != nil {
		evidenceRel = evidenceFile
	}
	if filepath.Clean(evidenceRel) != filepath.Clean(declRel) {
		add("%s: evidence anchor #%s resolves to %s, outside the named test's file %s", where, anchor.Test, filepath.ToSlash(evidenceRel), filepath.ToSlash(declRel))
	}
}

func validateSubprocess(root, list string, e Entry, where string, add func(string, ...any)) {
	if e.Class != ClassSubprocess {
		if e.ExecCommandSite != "" {
			add("%s: exec_command_site is set but class is %q, not subprocess", where, e.Class)
		}
		return
	}
	if e.ExecCommandSite == "" {
		add("%s: subprocess entry must name the exec_command_site it was resolved against", where)
		return
	}
	if e.BuildFlags == nil {
		add("%s: subprocess entry must declare build_flags (use `build_flags: []` for a no-flag child)", where)
		return
	}
	anchor, err := ParseExecAnchor(e.ExecCommandSite)
	if err != nil {
		add("%s: exec_command_site: %v", where, err)
		return
	}
	site, err := ResolveExecSite(root, anchor)
	if err != nil {
		add("%s: %v", where, err)
		return
	}
	if !site.FlagsKnown {
		add("%s: cannot prove the child at %s is built without a detector: unresolved argument %s", where, e.ExecCommandSite, site.Unresolved)
		return
	}
	for _, declared := range *e.BuildFlags {
		if !contains(site.Flags, declared) {
			add("%s: declared build_flags %q not found at %s (resolved: %v)", where, declared, e.ExecCommandSite, site.Flags)
		}
	}
	if list == "partition" {
		// A partition child must be built WITHOUT the detector, whether the
		// race-enabling flag is declared or only resolved at the site.
		if site.RaceEnabled || e.BuildFlags.HasRace() {
			add("%s: partition subprocess entry builds its child with the race detector enabled at %s; the child must be built without -race", where, e.ExecCommandSite)
		}
		return
	}
	if site.RaceEnabled && !e.BuildFlags.HasRace() {
		add("%s: build_flags must record the race-enabling flag resolved at %s", where, e.ExecCommandSite)
	}
}

// HasRace reports whether the declared flags enable the race detector.
func (b *BuildFlags) HasRace() bool {
	if b == nil {
		return false
	}
	for _, f := range *b {
		if enablesRace(f) {
			return true
		}
	}
	return false
}

func enablesRace(flag string) bool {
	return flag == "-race" || flag == "-race=true"
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
