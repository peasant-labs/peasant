// Package gen implements the fairtrade-tokens-to-Go-palette codegen that
// backs cmd/gen-terminal-palette. It is deliberately split from the command
// (which only resolves paths and writes files) so both the generator and its
// freshness test can call the same pure functions: the test regenerates from
// the committed testdata/tokens.json snapshot and asserts the result is
// byte-identical to the committed internal/tui/theme/palette_gen.go, the
// same way internal/redactionpolicygen backs its freshness test.
//
// Source of truth: the "color" token group of the fairtrade design system's
// published tokens.json (@peasant-labs/fairtrade, web/node_modules/
// @peasant-labs/fairtrade/dist/lib/tokens.json once installed). Every color
// token carries a dark value ($value) and a light value
// ($extensions["fairtrade.theme"].light); ParseColorTokens requires both and
// fails closed if either is missing, rather than silently emitting a
// one-sided palette. Non-color token groups (space, typography, motion,
// font, breakpoint, z-index, other) are not terminal-usable and are ignored.
package gen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"sort"
	"strings"
)

// Token is one color token as read from the fairtrade tokens.json "color"
// group, with both terminal-mode values resolved.
type Token struct {
	// Name is the token's kebab-case name in tokens.json, e.g. "amber-fill-ink".
	Name string
	// Dark is the token's dark-mode hex value ($value).
	Dark string
	// Light is the token's light-mode hex value
	// ($extensions["fairtrade.theme"].light).
	Light string
}

// FieldName converts a kebab-case fairtrade token name into the exported Go
// struct field name used for it in the generated Palette, e.g.
// "amber-fill-ink" -> "AmberFillInk", "surface-2" -> "Surface2".
func FieldName(tokenName string) string {
	parts := strings.Split(tokenName, "-")
	var b strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]))
		b.WriteString(part[1:])
	}
	return b.String()
}

// rawTokenSet is the shape of tokens.json this package reads: only the
// "color" group is decoded; every other top-level group (space, typography,
// motion, font, breakpoint, z-index, other) is intentionally left untyped
// and ignored, since none of it is terminal-usable.
type rawTokenSet struct {
	Color map[string]rawColorToken `json:"color"`
}

type rawColorToken struct {
	Type       string `json:"$type"`
	Value      string `json:"$value"`
	Extensions struct {
		FairtradeTheme struct {
			Light string `json:"light"`
		} `json:"fairtrade.theme"`
	} `json:"$extensions"`
}

// ParseColorTokens extracts the "color" token group from raw fairtrade
// tokens.json bytes into a deterministically-ordered (sorted by Name) slice
// of Token.
//
// It fails closed: a malformed top-level document, an empty color group, or
// any single token missing its dark value ($value) or its light value
// ($extensions["fairtrade.theme"].light) is an error, never a
// silently-substituted default. A one-sided or empty palette would make
// every terminal surface built on it render wrong or unreadable in the
// missing mode, so this stage refuses to produce one.
func ParseColorTokens(tokensJSON []byte) ([]Token, error) {
	var raw rawTokenSet
	if err := json.Unmarshal(tokensJSON, &raw); err != nil {
		return nil, fmt.Errorf(
			"gen.ParseColorTokens: malformed fairtrade tokens.json.\n"+
				"what: the top-level document did not decode as JSON with a \"color\" object.\n"+
				"why: %v\n"+
				"where: internal/tui/theme/gen.ParseColorTokens, decoding the tokens.json passed to it.\n"+
				"when: at palette codegen, before any Go source was written.\n"+
				"means: cmd/gen-terminal-palette cannot produce a palette from this input.\n"+
				"fix: verify the source is @peasant-labs/fairtrade's dist/lib/tokens.json (not a partial or hand-edited copy) and re-run `go run ./cmd/gen-terminal-palette`.",
			err)
	}
	if len(raw.Color) == 0 {
		return nil, fmt.Errorf(
			"gen.ParseColorTokens: the fairtrade tokens.json \"color\" group is empty.\n" +
				"what: no color tokens were found to generate a palette from.\n" +
				"why: the input either has no \"color\" key or it decoded to zero entries.\n" +
				"where: internal/tui/theme/gen.ParseColorTokens.\n" +
				"when: at palette codegen, before any Go source was written.\n" +
				"means: internal/tui/theme.Palette would have no fields, and every TUI surface built on it would have no colors.\n" +
				"fix: verify the tokens.json comes from an installed @peasant-labs/fairtrade package with a non-empty color group.")
	}

	names := make([]string, 0, len(raw.Color))
	for name := range raw.Color {
		names = append(names, name)
	}
	sort.Strings(names)

	tokens := make([]Token, 0, len(names))
	for _, name := range names {
		raw := raw.Color[name]
		if strings.TrimSpace(raw.Value) == "" {
			return nil, fmt.Errorf(
				"gen.ParseColorTokens: color token %q has no dark value.\n"+
					"what: the token's $value field is empty.\n"+
					"why: the fairtrade tokens.json entry for %q carries no $value.\n"+
					"where: internal/tui/theme/gen.ParseColorTokens, token %q.\n"+
					"when: at palette codegen, before any Go source was written.\n"+
					"means: this token could not be given a dark-mode color, so the generated Palette would be one-sided for it.\n"+
					"fix: this is a defect in the upstream fairtrade tokens.json; do not hand-patch the generated palette, fix the source token and regenerate.",
				name, name, name)
		}
		if strings.TrimSpace(raw.Extensions.FairtradeTheme.Light) == "" {
			return nil, fmt.Errorf(
				"gen.ParseColorTokens: color token %q has no light value.\n"+
					"what: the token's $extensions[\"fairtrade.theme\"].light field is empty.\n"+
					"why: the fairtrade tokens.json entry for %q carries a dark $value but no light-mode extension.\n"+
					"where: internal/tui/theme/gen.ParseColorTokens, token %q.\n"+
					"when: at palette codegen, before any Go source was written.\n"+
					"means: theme.ThemeLight would render this token using an unset color rather than failing loudly, so this "+
					"stage fails closed instead.\n"+
					"fix: this is a defect in the upstream fairtrade tokens.json; do not hand-patch the generated palette, fix the source token and regenerate.",
				name, name, name)
		}
		tokens = append(tokens, Token{
			Name:  name,
			Dark:  raw.Value,
			Light: raw.Extensions.FairtradeTheme.Light,
		})
	}
	return tokens, nil
}

// paletteHeader is the DO-NOT-EDIT banner shared by every generated
// palette_gen.go, mirroring the internal/redactionpolicygen / internal/redactmock
// convention.
const paletteHeader = `// Code generated by cmd/gen-terminal-palette. DO NOT EDIT.
//
// Source of truth: the "color" token group of the fairtrade design system's
// tokens.json (@peasant-labs/fairtrade), pinned via web/package.json and
// snapshotted at internal/tui/theme/testdata/tokens.json. Regenerate with
// ` + "`go run ./cmd/gen-terminal-palette`" + ` after bumping the fairtrade pin; a
// freshness test fails if this file drifts from either the committed
// snapshot or (when web/node_modules is installed) the currently-installed
// tokens.json.

package theme

import "charm.land/lipgloss/v2"

`

// GeneratePaletteGo renders the committed internal/tui/theme/palette_gen.go
// source from tokens: the closed-set Palette struct (one ColorPair field per
// token) and the GeneratedPalette instance populated from tokens' dark/light
// values. Field order is the same deterministic (sorted-by-name) order
// ParseColorTokens produces, so regeneration from an unchanged input is
// byte-for-byte stable.
func GeneratePaletteGo(tokens []Token) ([]byte, error) {
	if len(tokens) == 0 {
		return nil, fmt.Errorf(
			"gen.GeneratePaletteGo: called with zero tokens.\n" +
				"what: no color tokens were given to render.\n" +
				"why: the caller passed an empty token slice.\n" +
				"where: internal/tui/theme/gen.GeneratePaletteGo.\n" +
				"when: at palette codegen, before any Go source was written.\n" +
				"means: a Palette type with zero fields would compile but every consumer of it would have no colors to render.\n" +
				"fix: pass the tokens returned by a successful gen.ParseColorTokens call.")
	}

	seen := make(map[string]string, len(tokens))
	var buf bytes.Buffer
	buf.WriteString(paletteHeader)
	buf.WriteString("// Palette is the closed set of terminal colors sourced from the fairtrade\n")
	buf.WriteString("// design system's \"color\" token group. One field per token; both Dark and\n")
	buf.WriteString("// Light values are always populated (gen.ParseColorTokens fails closed on any\n")
	buf.WriteString("// token missing either side).\n")
	buf.WriteString("type Palette struct {\n")
	for _, tok := range tokens {
		field := FieldName(tok.Name)
		if prior, dup := seen[field]; dup {
			return nil, fmt.Errorf(
				"gen.GeneratePaletteGo: token names %q and %q both map to the Go field %q.\n"+
					"what: two distinct fairtrade tokens collide on the same generated struct field name.\n"+
					"why: gen.FieldName strips hyphens, so names differing only by hyphen placement or case collide.\n"+
					"where: internal/tui/theme/gen.GeneratePaletteGo.\n"+
					"when: at palette codegen, before any Go source was written.\n"+
					"means: the generated Palette struct would either fail to compile (duplicate field) or silently drop one token's colors.\n"+
					"fix: rename one of the colliding fairtrade tokens, or extend gen.FieldName to disambiguate them.",
				prior, tok.Name, field)
		}
		seen[field] = tok.Name
		fmt.Fprintf(&buf, "\t// %s is the fairtrade %q token.\n", field, tok.Name)
		fmt.Fprintf(&buf, "\t%s ColorPair\n", field)
	}
	buf.WriteString("}\n\n")

	buf.WriteString("// GeneratedPalette is the terminal palette generated from the pinned\n")
	buf.WriteString("// fairtrade tokens.json (internal/tui/theme/testdata/tokens.json snapshot).\n")
	buf.WriteString("var GeneratedPalette = Palette{\n")
	for _, tok := range tokens {
		field := FieldName(tok.Name)
		fmt.Fprintf(&buf, "\t%s: ColorPair{Dark: lipgloss.Color(%q), Light: lipgloss.Color(%q)},\n", field, tok.Dark, tok.Light)
	}
	buf.WriteString("}\n")

	return formatGoSource(buf.Bytes())
}

// formatGoSource runs gofmt over generated source so the committed
// palette_gen.go matches what `gofmt -l .` expects, and so byte-identity
// checks in the freshness test are not sensitive to this generator's own
// whitespace choices.
func formatGoSource(src []byte) ([]byte, error) {
	formatted, err := format.Source(src)
	if err != nil {
		return nil, fmt.Errorf(
			"gen.GeneratePaletteGo: generated source failed to gofmt.\n"+
				"what: format.Source rejected the rendered palette_gen.go.\n"+
				"why: %v\n"+
				"where: internal/tui/theme/gen.formatGoSource.\n"+
				"when: at palette codegen, after rendering but before writing the file.\n"+
				"means: this is a bug in the generator's templating, not in the fairtrade tokens.\n"+
				"fix: report the malformed generated snippet; do not hand-edit palette_gen.go to work around it.",
			err)
	}
	return formatted, nil
}
