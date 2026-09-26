// Package coveragemap declares the schema for the consolidation coverage map
// and the inventory it closes (DoD-3).
//
// # The re-freeze rule
//
// The inventory is generated at the slice BRANCH POINT, not at plan time: the
// epic's slices branch from a fixed commit, and any name added to the default
// branch afterwards must not present as unmapped. The schema carries that rule
// in `frozen_from`: the commit the inventory was generated at. The loader
// requires it and rejects an inventory without a commit-shaped value, so a
// plan-time or hand-written inventory is not admissible.
//
// # The map
//
// Every inventory name appears in the map exactly once, with a destination from
// a closed set:
//
//	retained-in-place                       the name stays where it is
//	moved:<file>                            the name now lives in <file>
//	deleted:<rationale-ref>                 the name was removed; <rationale-ref>
//	                                        resolves to the recorded rationale
//	followup:<task-id>                      the name is tracked by a follow-up task
//
// A `moved` target must exist on disk; a `deleted` or `followup` entry must name
// its rationale or task. A map entry is written by the slice that performs the
// move, in the same commit, so the map is never written after the fact.
package coveragemap

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// DestinationKind is the closed set of inventory-name destinations.
type DestinationKind string

const (
	// DestinationRetained means the name stays where it is.
	DestinationRetained DestinationKind = "retained-in-place"
	// DestinationMoved means the name now lives in the ref's file.
	DestinationMoved DestinationKind = "moved"
	// DestinationDeleted means the name was removed; ref resolves to the rationale.
	DestinationDeleted DestinationKind = "deleted"
	// DestinationFollowup means a follow-up task, named by ref, tracks the name.
	DestinationFollowup DestinationKind = "followup"
)

// AllDestinationKinds is the closed set, in contract order.
var AllDestinationKinds = []DestinationKind{
	DestinationRetained, DestinationMoved, DestinationDeleted, DestinationFollowup,
}

// NewDestinationKind converts a name into a kind, refusing anything outside the
// closed set.
func NewDestinationKind(name string) (DestinationKind, bool) {
	for _, k := range AllDestinationKinds {
		if string(k) == name {
			return k, true
		}
	}
	return "", false
}

// Valid reports whether k is a member of the closed set.
func (k DestinationKind) Valid() bool {
	_, ok := NewDestinationKind(string(k))
	return ok
}

// Destination is one entry's destination. Retained carries no ref; every other
// kind requires one.
type Destination struct {
	Kind DestinationKind
	Ref  string
}

// ParseDestination parses the `kind` or `kind:ref` spelling used in the fixture.
func ParseDestination(s string) (Destination, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Destination{}, errors.New("destination is empty")
	}
	kind, ref, _ := strings.Cut(raw, ":")
	k, ok := NewDestinationKind(kind)
	if !ok {
		return Destination{}, fmt.Errorf("unknown destination kind %q (closed set: %s)", kind, strings.Join(destinationKindNames(), ", "))
	}
	ref = strings.TrimSpace(ref)
	if k == DestinationRetained {
		if ref != "" {
			return Destination{}, fmt.Errorf("destination %q takes no ref", string(DestinationRetained))
		}
		return Destination{Kind: k}, nil
	}
	if ref == "" {
		return Destination{}, fmt.Errorf("destination kind %q requires a ref", kind)
	}
	return Destination{Kind: k, Ref: ref}, nil
}

// String renders the destination in the fixture spelling.
func (d Destination) String() string {
	if d.Kind == DestinationRetained || d.Ref == "" {
		return string(d.Kind)
	}
	return string(d.Kind) + ":" + d.Ref
}

// UnmarshalYAML parses the fixture spelling and refuses anything outside the set.
func (d *Destination) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("destination must be a scalar, got YAML kind %d", node.Kind)
	}
	parsed, err := ParseDestination(node.Value)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// MarshalYAML writes the fixture spelling.
func (d Destination) MarshalYAML() (any, error) { return d.String(), nil }

// InventoryName is one name the inventory holds.
type InventoryName struct {
	// Name is the symbol being accounted for, e.g. a test, helper, or type name.
	Name string `yaml:"name"`
	// Kind is the inventory axis (count-guard, seed-helper, decorator, decoder, ...).
	Kind string `yaml:"kind"`
	// Where is the file:line it was observed at when the inventory was frozen.
	Where string `yaml:"where"`
}

// Inventory is the frozen set of names the coverage map must account for.
type Inventory struct {
	Version int `yaml:"version"`
	// FrozenFrom is the branch-point commit the inventory was generated at (R4).
	FrozenFrom string          `yaml:"frozen_from"`
	Names      []InventoryName `yaml:"names"`
}

// MapEntry is one inventory name's destination.
type MapEntry struct {
	Name        string      `yaml:"name"`
	Destination Destination `yaml:"destination"`
}

// CoverageMap maps every inventory name to exactly one destination.
type CoverageMap struct {
	Version int `yaml:"version"`
	// Inventory is the repo-relative path of the inventory this map closes.
	Inventory string     `yaml:"inventory"`
	Entries   []MapEntry `yaml:"entries"`
}

var frozenFromRE = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// LoadInventory reads and strictly decodes an inventory.
func LoadInventory(path string) (Inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Inventory{}, fmt.Errorf("read inventory %s: %w", path, err)
	}
	inv, err := DecodeInventory(data)
	if err != nil {
		return Inventory{}, fmt.Errorf("decode inventory %s: %w", path, err)
	}
	return inv, nil
}

// DecodeInventory strictly decodes exactly one YAML document.
func DecodeInventory(data []byte) (Inventory, error) {
	var inv Inventory
	if err := strictDecode(data, &inv); err != nil {
		return Inventory{}, err
	}
	return inv, nil
}

// LoadCoverageMap reads and strictly decodes a coverage map.
func LoadCoverageMap(path string) (CoverageMap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CoverageMap{}, fmt.Errorf("read coverage map %s: %w", path, err)
	}
	m, err := DecodeCoverageMap(data)
	if err != nil {
		return CoverageMap{}, fmt.Errorf("decode coverage map %s: %w", path, err)
	}
	return m, nil
}

// DecodeCoverageMap strictly decodes exactly one YAML document.
func DecodeCoverageMap(data []byte) (CoverageMap, error) {
	var m CoverageMap
	if err := strictDecode(data, &m); err != nil {
		return CoverageMap{}, err
	}
	return m, nil
}

func strictDecode(data []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("document must contain exactly one YAML document")
	}
	return nil
}

// ValidateInventory checks the re-freeze rule: version 1, a commit-shaped
// frozen_from, and a name set that is non-empty, unique, and located.
func ValidateInventory(inv Inventory) error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if inv.Version != 1 {
		add("inventory version = %d, want 1", inv.Version)
	}
	if !frozenFromRE.MatchString(strings.TrimSpace(inv.FrozenFrom)) {
		add("inventory frozen_from = %q is not a commit; the inventory is generated at the slice branch point, not at plan time", inv.FrozenFrom)
	}
	seen := map[string]bool{}
	for _, n := range inv.Names {
		if strings.TrimSpace(n.Name) == "" {
			add("inventory has an empty name")
			continue
		}
		if seen[n.Name] {
			add("inventory name %q appears more than once", n.Name)
		}
		seen[n.Name] = true
		if strings.TrimSpace(n.Kind) == "" {
			add("inventory name %q has no kind", n.Name)
		}
		if strings.TrimSpace(n.Where) == "" {
			add("inventory name %q has no where", n.Name)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("inventory validation failed:\n  - %s", strings.Join(problems, "\n  - "))
}

// ValidateCoverageMap checks the map against its inventory and the tree rooted
// at root: every inventory name appears exactly once with a closed-set
// destination; a moved target exists; a deleted or followup entry names its
// rationale or task.
func ValidateCoverageMap(root string, inv Inventory, m CoverageMap) error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if m.Version != 1 {
		add("coverage map version = %d, want 1", m.Version)
	}
	if strings.TrimSpace(m.Inventory) == "" {
		add("coverage map must name the inventory it closes")
	}

	inventoryNames := map[string]bool{}
	for _, n := range inv.Names {
		inventoryNames[n.Name] = true
	}

	mapped := map[string]bool{}
	for _, e := range m.Entries {
		if strings.TrimSpace(e.Name) == "" {
			add("coverage map has an entry with no name")
			continue
		}
		if mapped[e.Name] {
			add("coverage map name %q appears more than once", e.Name)
		}
		mapped[e.Name] = true
		if !inventoryNames[e.Name] {
			add("coverage map name %q is not in the inventory", e.Name)
		}
		if !e.Destination.Kind.Valid() {
			add("coverage map name %q has unknown destination %q", e.Name, string(e.Destination.Kind))
			continue
		}
		if e.Destination.Kind == DestinationMoved {
			target := filepath.Join(root, filepath.FromSlash(e.Destination.Ref))
			if _, err := os.Stat(target); err != nil {
				add("coverage map name %q moved to %q, which does not exist", e.Name, e.Destination.Ref)
			}
		}
	}

	for _, n := range inv.Names {
		if !mapped[n.Name] {
			add("inventory name %q has no destination in the coverage map", n.Name)
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("coverage map validation failed:\n  - %s", strings.Join(problems, "\n  - "))
}

func destinationKindNames() []string {
	out := make([]string, 0, len(AllDestinationKinds))
	for _, k := range AllDestinationKinds {
		out = append(out, string(k))
	}
	return out
}
