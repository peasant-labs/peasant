package fsdecorator

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Capability names one of the three reusable decorator behaviors, plus the
// explicit disposition for a decorator that needs none (and is therefore
// deleted with a rationale rather than migrated).
type Capability string

const (
	// CapabilityCounting is the path-keyed fault/count behavior of CountingFS.
	CapabilityCounting Capability = "counting"
	// CapabilityGated is the blocking-gate behavior of GatedFS.
	CapabilityGated Capability = "gated"
	// CapabilityBounded is the bound/read-only behavior of BoundedFS.
	CapabilityBounded Capability = "bounded"
	// CapabilityNone is not a capability: it records that the decorator needs
	// none of the three and is deleted with its rationale.
	CapabilityNone Capability = "none"
)

// AllCapabilities lists the closed set, in contract order.
var AllCapabilities = []Capability{CapabilityCounting, CapabilityGated, CapabilityBounded, CapabilityNone}

// NewCapability converts a fixture name into a capability, refusing anything
// outside the closed set.
func NewCapability(name string) (Capability, bool) {
	for _, c := range AllCapabilities {
		if string(c) == name {
			return c, true
		}
	}
	return "", false
}

// Valid reports whether c is a member of the closed set.
func (c Capability) Valid() bool {
	_, ok := NewCapability(string(c))
	return ok
}

// Owner names who implements a classified decorator. The split is forced by the
// import cycle documented in the package comment.
type Owner string

const (
	// OwnerTestutil is internal/testutil, for every non-white-box decorator.
	OwnerTestutil Owner = "internal/testutil"
	// OwnerIngestWhiteBox is the white-box `package ingest` test file, for the
	// decorators that need the package's unexported internals.
	OwnerIngestWhiteBox Owner = "internal/ingest (package ingest)"
)

// ClassificationEntry is one decorator type's classification, recorded BEFORE
// any migration so the mapping is reviewable on its own.
type ClassificationEntry struct {
	// Type is the decorator type name, e.g. "gatedPairFS".
	Type string `yaml:"type"`
	// Package is the directory that declares it.
	Package string `yaml:"package"`
	// WhiteBox is true when the type is declared in a `package ingest` test file
	// and therefore routes to OwnerIngestWhiteBox.
	WhiteBox bool `yaml:"white_box"`
	// Capability is the closed-set behavior the type needs, or `none`.
	Capability Capability `yaml:"capability"`
	// Where is the file:line of the declaration.
	Where string `yaml:"where"`
	// Rationale is why that capability (or the deletion) is the right call.
	Rationale string `yaml:"rationale"`
}

// Owner returns the owner that implements this entry, derived from WhiteBox.
func (e ClassificationEntry) Owner() Owner {
	if e.WhiteBox {
		return OwnerIngestWhiteBox
	}
	return OwnerTestutil
}

// Classification is the decorator-classification document.
type Classification struct {
	Version int                   `yaml:"version"`
	Entries []ClassificationEntry `yaml:"entries"`
}

// LoadClassification reads and strictly decodes a classification document.
func LoadClassification(path string) (Classification, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Classification{}, fmt.Errorf("read classification %s: %w", path, err)
	}
	cls, err := DecodeClassification(data)
	if err != nil {
		return Classification{}, fmt.Errorf("decode classification %s: %w", path, err)
	}
	return cls, nil
}

// DecodeClassification strictly decodes exactly one YAML document.
func DecodeClassification(data []byte) (Classification, error) {
	var cls Classification
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cls); err != nil {
		return Classification{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Classification{}, errors.New("classification must contain exactly one YAML document")
	}
	return cls, nil
}

// ValidateClassification checks every entry: a unique type, non-empty package,
// where, and rationale, and a capability from the closed set. A `none` entry is
// admitted only with the rationale that justifies its deletion.
func ValidateClassification(cls Classification) error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	if cls.Version != 1 {
		add("classification version = %d, want 1", cls.Version)
	}
	seen := map[string]bool{}
	for _, e := range cls.Entries {
		where := fmt.Sprintf("entry %s/%s", e.Package, e.Type)
		if strings.TrimSpace(e.Type) == "" || strings.TrimSpace(e.Package) == "" {
			add("%s: type and package are required", where)
			continue
		}
		if seen[e.Type] {
			add("%s: duplicate classification for type %s", where, e.Type)
		}
		seen[e.Type] = true
		if !e.Capability.Valid() {
			add("%s: unknown capability %q (closed set: %s)", where, e.Capability, strings.Join(capabilityNames(), ", "))
		}
		if strings.TrimSpace(e.Where) == "" {
			add("%s: where (file:line) is required", where)
		}
		if strings.TrimSpace(e.Rationale) == "" {
			add("%s: rationale is required", where)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("classification validation failed:\n  - %s", strings.Join(problems, "\n  - "))
}

func capabilityNames() []string {
	out := make([]string, 0, len(AllCapabilities))
	for _, c := range AllCapabilities {
		out = append(out, string(c))
	}
	return out
}
