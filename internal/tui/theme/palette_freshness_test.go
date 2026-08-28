package theme_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/peasant/internal/tui/theme/gen"
)

// paletteGoRelPath and tokensSnapshotRelPath mirror the constants in
// cmd/gen-terminal-palette/main.go; they are re-declared here (not imported -
// that command is a package main) so this test names its own committed
// artifacts explicitly.
const (
	paletteGoRelPath       = "internal/tui/theme/palette_gen.go"
	tokensSnapshotRelPath  = "internal/tui/theme/testdata/tokens.json"
	tokensInstalledRelPath = "web/node_modules/@peasant-labs/fairtrade/dist/lib/tokens.json"
)

// TestPaletteFreshness_HermeticRegenMatchesCommitted is the ALWAYS-runs half
// of the two-part freshness gate: it regenerates palette_gen.go purely from
// the committed testdata/tokens.json snapshot - no network, no
// web/node_modules - and asserts the result is byte-identical to what is
// committed. It fires in both directions: a hand-edit of palette_gen.go and
// a snapshot bump without regeneration both turn it red.
func TestPaletteFreshness_HermeticRegenMatchesCommitted(t *testing.T) {
	t.Parallel()
	root := testutil.ModuleRoot(t)

	snapshotPath := filepath.Join(root, filepath.FromSlash(tokensSnapshotRelPath))
	snapshot, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read committed tokens.json snapshot at %s: %v", snapshotPath, err)
	}

	tokens, err := gen.ParseColorTokens(snapshot)
	if err != nil {
		t.Fatalf("parse committed tokens.json snapshot: %v", err)
	}
	want, err := gen.GeneratePaletteGo(tokens)
	if err != nil {
		t.Fatalf("generate palette from committed snapshot: %v", err)
	}

	paletteGoPath := filepath.Join(root, filepath.FromSlash(paletteGoRelPath))
	got, err := os.ReadFile(paletteGoPath)
	if err != nil {
		t.Fatalf("read committed %s: %v", paletteGoPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf(
			"STALE committed %s: it does not match a regeneration from the committed testdata/tokens.json snapshot.\n"+
				"what: internal/tui/theme/palette_gen.go was hand-edited, or the snapshot changed without regeneration.\n"+
				"why:  bytes.Equal(committed, regenerated-from-snapshot) is false.\n"+
				"where: internal/tui/theme (palette_gen.go, testdata/tokens.json).\n"+
				"when: this hermetic half of the freshness gate always runs, with no dependency on web/node_modules.\n"+
				"means: the terminal palette in the tree no longer matches the fairtrade tokens it claims to be generated from.\n"+
				"fix: run `go run ./cmd/gen-terminal-palette` and commit both files together; never hand-edit palette_gen.go.",
			paletteGoRelPath)
	}
}

// TestPaletteFreshness_SnapshotMatchesInstalledTokens is the CURRENCY half:
// when web/node_modules/@peasant-labs/fairtrade is installed, it asserts the
// committed testdata/tokens.json snapshot is byte-identical to what is
// actually installed - catching a fairtrade pin bump in web/package.json
// whose tokens.json was never re-snapshotted. It skips (with an actionable
// message, not a silent pass) when node_modules is absent, which is expected
// in a hermetic CI job or a fresh worktree that has not run `pnpm install`.
func TestPaletteFreshness_SnapshotMatchesInstalledTokens(t *testing.T) {
	t.Parallel()
	root := testutil.ModuleRoot(t)

	installedPath := filepath.Join(root, filepath.FromSlash(tokensInstalledRelPath))
	installed, err := os.ReadFile(installedPath)
	if err != nil {
		t.Skipf(
			"skipping the node_modules-currency half of the palette freshness gate: %s is absent (%v).\n"+
				"This is expected when web/node_modules has not been installed (a hermetic CI job, or a fresh "+
				"worktree before `pnpm install` in web/). The hermetic half "+
				"(TestPaletteFreshness_HermeticRegenMatchesCommitted) still ran and does not depend on this file. "+
				"To exercise this half, run `pnpm install` in web/ and re-run this test.",
			installedPath, err)
	}

	snapshotPath := filepath.Join(root, filepath.FromSlash(tokensSnapshotRelPath))
	snapshot, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read committed tokens.json snapshot at %s: %v", snapshotPath, err)
	}

	if !bytes.Equal(snapshot, installed) {
		t.Errorf(
			"STALE tokens.json snapshot: %s no longer matches the installed %s.\n"+
				"what: the committed snapshot the hermetic freshness half regenerates from differs from what is installed.\n"+
				"why:  the fairtrade pin in web/package.json was bumped (or node_modules was reinstalled at a different "+
				"version) without re-running the generator.\n"+
				"where: internal/tui/theme/testdata/tokens.json vs %s.\n"+
				"when: this currency half runs only when web/node_modules is present.\n"+
				"means: internal/tui/theme/palette_gen.go may be generating stale colors relative to the pinned fairtrade version.\n"+
				"fix: run `go run ./cmd/gen-terminal-palette` (with web/node_modules installed) and commit the "+
				"updated snapshot and palette_gen.go together.",
			tokensSnapshotRelPath, tokensInstalledRelPath, tokensInstalledRelPath)
	}
}

// TestGeneratedPalette_HasAllColorTokens_BothModesPopulated asserts the
// SHAPE of the committed palette: 34 fields (all ColorPair), and every
// field's Dark and Light color both render a non-empty terminal
// representation. It reads the tokens from the committed snapshot rather
// than hardcoding 34, so it moves WITH the fairtrade token set rather than
// pinning today's count as a second, driftable copy of it.
func TestGeneratedPalette_HasAllColorTokens_BothModesPopulated(t *testing.T) {
	t.Parallel()
	root := testutil.ModuleRoot(t)
	snapshotPath := filepath.Join(root, filepath.FromSlash(tokensSnapshotRelPath))
	snapshot, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read committed tokens.json snapshot: %v", err)
	}
	tokens, err := gen.ParseColorTokens(snapshot)
	if err != nil {
		t.Fatalf("parse committed tokens.json snapshot: %v", err)
	}
	if len(tokens) == 0 {
		t.Fatal("the committed tokens.json snapshot carries zero color tokens")
	}

	for _, tok := range tokens {
		field := gen.FieldName(tok.Name)
		pair, ok := paletteField(t, field)
		if !ok {
			t.Errorf("theme.Palette has no field %q for fairtrade token %q; regenerate with `go run ./cmd/gen-terminal-palette`", field, tok.Name)
			continue
		}
		if pair.For(theme.ModeDark) == nil {
			t.Errorf("Palette.%s.For(ModeDark) is nil; every token must have a dark color", field)
		}
		if pair.For(theme.ModeLight) == nil {
			t.Errorf("Palette.%s.For(ModeLight) is nil; every token must have a light color", field)
		}
	}
}

// paletteField reads field by name from theme.GeneratedPalette via
// reflection, so this test's coverage tracks the actual generated struct
// shape rather than a second hand-maintained field list.
func paletteField(t *testing.T, field string) (theme.ColorPair, bool) {
	t.Helper()
	v := reflect.ValueOf(theme.GeneratedPalette)
	fv := v.FieldByName(field)
	if !fv.IsValid() {
		return theme.ColorPair{}, false
	}
	pair, ok := fv.Interface().(theme.ColorPair)
	if !ok {
		t.Fatalf("Palette field %q is not a theme.ColorPair", field)
	}
	return pair, true
}
