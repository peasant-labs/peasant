package store

import (
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/migration_slots.yaml
var migrationSlotsYAML []byte

//go:embed testdata/migration_slots.manifest.yaml
var migrationSlotsManifestYAML []byte

type migrationSlotFixture struct {
	Name   string `yaml:"name"`
	Slot   int    `yaml:"slot"`
	Needle string `yaml:"needle"`
}

type migrationSlotGuardFixture struct {
	Name              string   `yaml:"name"`
	Operation         string   `yaml:"operation"`
	TargetSlot        int      `yaml:"targetSlot"`
	ReplacementNeedle string   `yaml:"replacementNeedle"`
	AppendedScript    string   `yaml:"appendedScript"`
	ExpectedFragments []string `yaml:"expectedFragments"`
	ExpectedSlots     []int    `yaml:"expectedSlots"`
}

type migrationSlotFixtures struct {
	Slots  []migrationSlotFixture      `yaml:"slots"`
	Guards []migrationSlotGuardFixture `yaml:"guards"`
}

type migrationSlotManifest struct {
	RequiredNames      []string `yaml:"requiredNames"`
	RequiredGuardNames []string `yaml:"requiredGuardNames"`
}

func LoadMigrationSlotFixtures() (migrationSlotFixtures, migrationSlotManifest, error) {
	var fixtures migrationSlotFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(migrationSlotsYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		return fixtures, migrationSlotManifest{}, fmt.Errorf("decode internal/store/testdata/migration_slots.yaml: %w", err)
	}
	var manifest migrationSlotManifest
	decoder = yaml.NewDecoder(strings.NewReader(string(migrationSlotsManifestYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return fixtures, manifest, fmt.Errorf("decode internal/store/testdata/migration_slots.manifest.yaml: %w", err)
	}
	return fixtures, manifest, nil
}

func validateMigrationNames(required, actual []string, label string) error {
	if len(required) == 0 {
		return fmt.Errorf("%s manifest is empty; add every named fixture to its required-name list", label)
	}
	seenRequired := make(map[string]struct{}, len(required))
	for _, name := range required {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%s manifest contains a blank required name; name every fixture identity", label)
		}
		if _, duplicate := seenRequired[name]; duplicate {
			return fmt.Errorf("%s manifest repeats required name %q; keep one declaration", label, name)
		}
		seenRequired[name] = struct{}{}
	}
	for _, name := range actual {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%s fixture contains a blank name; give every case a stable identity", label)
		}
	}
	return validateRecoveryRequiredNames(required, actual, label)
}

func validateMigrationSlots(migrations []string, rows []migrationSlotFixture, requiredNames []string) error {
	actualNames := make([]string, 0, len(rows))
	bySlot := make(map[int]migrationSlotFixture, len(rows))
	for _, row := range rows {
		actualNames = append(actualNames, row.Name)
		if strings.TrimSpace(row.Needle) == "" {
			return fmt.Errorf("internal/store/testdata/migration_slots.yaml: slot fixture %q has a blank needle; choose an unchanged substring unique to its migration script", row.Name)
		}
		if row.Slot < 1 || row.Slot > len(migrations) {
			return fmt.Errorf("internal/store/testdata/migration_slots.yaml: fixture %q names slot %d outside actual dbSchema.Migrations slots 1..%d; correct or remove the stale fixture", row.Name, row.Slot, len(migrations))
		}
		if prior, duplicate := bySlot[row.Slot]; duplicate {
			return fmt.Errorf("internal/store/testdata/migration_slots.yaml: slot %d is assigned to both %q and %q; keep one named identity per migration slot", row.Slot, prior.Name, row.Name)
		}
		bySlot[row.Slot] = row
	}
	if err := validateMigrationNames(requiredNames, actualNames, "migration slot identity"); err != nil {
		return err
	}
	for index := range migrations {
		slot := index + 1
		if _, found := bySlot[slot]; !found {
			return fmt.Errorf("internal/store/testdata/migration_slots.yaml: dbSchema.Migrations[%d], slot %d has no named identity fixture; add a slot row with a unique needle and add its name to requiredNames before shipping this migration", index, slot)
		}
	}
	for slot := 1; slot <= len(migrations); slot++ {
		row := bySlot[slot]
		if !strings.Contains(migrations[slot-1], row.Needle) {
			return fmt.Errorf("internal/store/migrations.go: dbSchema.Migrations[%d], slot %d (%s) does not contain needle %s during the in-memory identity check; migration order may have changed and upgrades are unsafe; restore the intended script at this slot, not the fixture to match a reordered list", slot-1, slot, row.Name, row.Needle)
		}
	}
	for slot := 1; slot <= len(migrations); slot++ {
		row := bySlot[slot]
		matches := make([]int, 0, 1)
		for index, migration := range migrations {
			if strings.Contains(migration, row.Needle) {
				matches = append(matches, index+1)
			}
		}
		if len(matches) != 1 || matches[0] != row.Slot {
			return fmt.Errorf("internal/store/testdata/migration_slots.yaml: identity %q expects slot %d needle %q to identify exactly that script, but it matches migration slots %v; the identity guard is ambiguous and cannot safely detect reorderings; choose a narrower unchanged-script substring", row.Name, row.Slot, row.Needle, matches)
		}
	}
	return nil
}

func TestMigrationSlotIdentity(t *testing.T) {
	fixtures, manifest, err := LoadMigrationSlotFixtures()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateMigrationSlots(dbSchema.Migrations, fixtures.Slots, manifest.RequiredNames); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationSlotAdjacentSwaps(t *testing.T) {
	fixtures, manifest, err := LoadMigrationSlotFixtures()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateMigrationSlots(dbSchema.Migrations, fixtures.Slots, manifest.RequiredNames); err != nil {
		t.Fatalf("baseline migration identities are invalid: %v", err)
	}
	rows := slices.Clone(fixtures.Slots)
	slices.SortFunc(rows, func(a, b migrationSlotFixture) int { return a.Slot - b.Slot })
	for _, row := range rows {
		if row.Slot == len(dbSchema.Migrations) {
			continue
		}
		t.Run(row.Name, func(t *testing.T) {
			migrations := slices.Clone(dbSchema.Migrations)
			migrations[row.Slot-1], migrations[row.Slot] = migrations[row.Slot], migrations[row.Slot-1]
			err := validateMigrationSlots(migrations, fixtures.Slots, manifest.RequiredNames)
			if err == nil {
				t.Fatalf("swapping slots %d and %d passed the identity check", row.Slot, row.Slot+1)
			}
			for _, want := range []string{fmt.Sprintf("slot %d", row.Slot), row.Needle} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("swap error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestMigrationSlotGuards(t *testing.T) {
	fixtures, manifest, err := LoadMigrationSlotFixtures()
	if err != nil {
		t.Fatal(err)
	}
	guardNames := make([]string, 0, len(fixtures.Guards))
	for _, guard := range fixtures.Guards {
		guardNames = append(guardNames, guard.Name)
	}
	if err := validateMigrationNames(manifest.RequiredGuardNames, guardNames, "migration slot guard"); err != nil {
		t.Fatal(err)
	}
	for _, guard := range fixtures.Guards {
		t.Run(guard.Name, func(t *testing.T) {
			migrations := slices.Clone(dbSchema.Migrations)
			rows := slices.Clone(fixtures.Slots)
			switch guard.Operation {
			case "delete-required-slot":
				rows = slices.DeleteFunc(rows, func(row migrationSlotFixture) bool { return row.Slot == guard.TargetSlot })
			case "replace-needle":
				found := false
				for index := range rows {
					if rows[index].Slot == guard.TargetSlot {
						rows[index].Needle = guard.ReplacementNeedle
						found = true
					}
				}
				if !found {
					t.Fatalf("guard target slot %d is absent", guard.TargetSlot)
				}
			case "append-uncovered-slot":
				migrations = append(migrations, guard.AppendedScript)
			default:
				t.Fatalf("unknown migration slot guard operation %q", guard.Operation)
			}
			err := validateMigrationSlots(migrations, rows, manifest.RequiredNames)
			if err == nil {
				t.Fatal(errors.New("guard mutation unexpectedly passed migration identity validation"))
			}
			wants := slices.Clone(guard.ExpectedFragments)
			if guard.Operation == "append-uncovered-slot" {
				wants = append(wants, fmt.Sprintf("slot %d", len(migrations)))
			}
			for _, slot := range guard.ExpectedSlots {
				wants = append(wants, fmt.Sprintf("%d", slot))
			}
			for _, want := range wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("guard error %q does not contain expected fragment %q", err, want)
				}
			}
		})
	}
}
