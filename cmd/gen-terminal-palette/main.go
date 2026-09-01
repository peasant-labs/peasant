// Command gen-terminal-palette regenerates internal/tui/theme/palette_gen.go
// (the terminal Palette) from the fairtrade design system's tokens.json, and
// refreshes the committed testdata/tokens.json snapshot the freshness test
// regenerates from hermetically. Run it from the peasant module root after
// `pnpm install` in web/ (so web/node_modules/@peasant-labs/fairtrade is
// present) and after bumping the fairtrade pin in web/package.json:
//
//	go run ./cmd/gen-terminal-palette
package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/tui/theme/gen"
)

// tokensRelPath is where the pinned fairtrade package publishes its design
// tokens once installed, relative to the peasant module root. Note dist/lib/,
// not dist/ - verified against the installed 0.0.11 package.
const tokensRelPath = "web/node_modules/@peasant-labs/fairtrade/dist/lib/tokens.json"

// paletteGoRelPath is the committed generated palette, relative to the
// module root.
const paletteGoRelPath = "internal/tui/theme/palette_gen.go"

// tokensSnapshotRelPath is the committed tokens.json snapshot the freshness
// test's hermetic half regenerates from, relative to the module root.
const tokensSnapshotRelPath = "internal/tui/theme/testdata/tokens.json"

func main() {
	root, err := moduleRoot()
	if err != nil {
		log.Fatalf("gen-terminal-palette: locate module root: %v", err)
	}

	tokensPath := filepath.Join(root, tokensRelPath)
	tokensJSON, err := os.ReadFile(tokensPath)
	if err != nil {
		log.Fatalf(
			"gen-terminal-palette: read %s: %v\n"+
				"what: could not read the fairtrade tokens.json.\n"+
				"why: web/node_modules is likely not installed.\n"+
				"where: cmd/gen-terminal-palette/main.go, reading %s.\n"+
				"when: at generator startup, before any output was produced.\n"+
				"means: no palette can be generated without the token source.\n"+
				"fix: run `pnpm install` in web/ (this repo is pnpm), then re-run `go run ./cmd/gen-terminal-palette`.",
			tokensPath, err, tokensRelPath)
	}

	tokens, err := gen.ParseColorTokens(tokensJSON)
	if err != nil {
		log.Fatalf("gen-terminal-palette: parse %s: %v", tokensPath, err)
	}

	paletteGo, err := gen.GeneratePaletteGo(tokens)
	if err != nil {
		log.Fatalf("gen-terminal-palette: generate palette: %v", err)
	}

	paletteGoPath := filepath.Join(root, paletteGoRelPath)
	if err := os.WriteFile(paletteGoPath, paletteGo, 0644); err != nil {
		log.Fatalf("gen-terminal-palette: write %s: %v", paletteGoPath, err)
	}
	log.Printf("Generated %s (%d color tokens)", paletteGoPath, len(tokens))

	snapshotPath := filepath.Join(root, tokensSnapshotRelPath)
	if err := os.WriteFile(snapshotPath, tokensJSON, 0644); err != nil {
		log.Fatalf("gen-terminal-palette: write %s: %v", snapshotPath, err)
	}
	log.Printf("Snapshotted %s", snapshotPath)
}

// moduleRoot walks up from the working directory to the directory containing
// go.mod (the peasant module root), mirroring cmd/gen-redaction-policy.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
