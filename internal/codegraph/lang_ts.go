// Pure-Go TypeScript import RESOLUTION: tsconfig discovery, path-alias
// patterns, and file-candidate resolution. Deliberately cgo-free — the
// tree-sitter EXTRACTION half lives in lang_ts_cgo.go (cgo builds only) with
// a !cgo stub in lang_ts_nocgo.go.

package codegraph

import (
	"encoding/json"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"
)

// tsConfigFileName is the TypeScript project file consulted for path-alias
// resolution. Its content arrives through the FileReader.
const tsConfigFileName = "tsconfig.json"

// ===== tsconfig path-alias resolution =====

// tsConfig is the subset of a tsconfig.json needed for import resolution:
// the directory it governs, the baseUrl, and the "paths" alias patterns.
type tsConfig struct {
	dir      string // repo-relative dir containing the tsconfig ("" = root)
	baseURL  string
	patterns []tsPathPattern
}

// tsPathPattern is one "paths" mapping, e.g. "@/*" -> ["./src/*"]. Patterns
// are ordered by descending literal-prefix length (TS longest-prefix-match
// semantics), tie-broken lexicographically for determinism.
type tsPathPattern struct {
	pattern string
	targets []string
}

// collectTSConfigs finds every tsconfig.json in the file list, keyed by the
// directory it governs. When no root tsconfig is listed, a direct
// read("tsconfig.json") is attempted.
func collectTSConfigs(paths []string, contents map[string][]byte, read FileReader) map[string]*tsConfig {
	cfgs := make(map[string]*tsConfig)
	for _, p := range paths {
		if path.Base(p) != tsConfigFileName {
			continue
		}
		data, ok := contents[p]
		if !ok {
			continue
		}
		if cfg := parseTSConfig(parentDir(p), data); cfg != nil {
			cfgs[cfg.dir] = cfg
		}
	}
	if _, ok := cfgs[""]; !ok {
		if data, err := read(tsConfigFileName); err == nil {
			if cfg := parseTSConfig("", data); cfg != nil {
				cfgs[""] = cfg
			}
		}
	}
	return cfgs
}

// parseTSConfig parses the compilerOptions subset of a tsconfig.json.
// tsconfig files are JSONC (comments + trailing commas allowed), so the
// content is sanitized before unmarshaling. Returns nil on parse failure.
func parseTSConfig(dir string, data []byte) *tsConfig {
	var doc struct {
		CompilerOptions struct {
			BaseURL string              `json:"baseUrl"`
			Paths   map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
	}
	if err := json.Unmarshal(stripTrailingCommas(stripJSONCComments(data)), &doc); err != nil {
		return nil
	}
	cfg := &tsConfig{dir: dir, baseURL: doc.CompilerOptions.BaseURL}
	for _, pattern := range slices.Sorted(maps.Keys(doc.CompilerOptions.Paths)) {
		cfg.patterns = append(cfg.patterns, tsPathPattern{
			pattern: pattern,
			targets: doc.CompilerOptions.Paths[pattern],
		})
	}
	sort.SliceStable(cfg.patterns, func(i, j int) bool {
		pi := patternPrefix(cfg.patterns[i].pattern)
		pj := patternPrefix(cfg.patterns[j].pattern)
		if len(pi) != len(pj) {
			return len(pi) > len(pj)
		}
		return cfg.patterns[i].pattern < cfg.patterns[j].pattern
	})
	return cfg
}

// patternPrefix returns the literal text before the first "*" (the whole
// pattern when there is no wildcard).
func patternPrefix(pattern string) string {
	if i := strings.Index(pattern, "*"); i >= 0 {
		return pattern[:i]
	}
	return pattern
}

// resolveTSImport resolves a module specifier from a file in fileDir to a
// repo-relative file path. Relative specifiers resolve against fileDir;
// non-relative specifiers are tried against the nearest governing tsconfig's
// path aliases. External and unresolvable specifiers return false.
func resolveTSImport(spec, fileDir string, cfgs map[string]*tsConfig, fileSet map[string]bool) (string, bool) {
	if spec == "" {
		return "", false
	}
	if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") || spec == "." || spec == ".." {
		return resolveTSFile(path.Join(fileDir, spec), fileSet)
	}
	// Nearest ancestor tsconfig with path aliases governs the file.
	for d := fileDir; ; d = parentDir(d) {
		if cfg, ok := cfgs[d]; ok && len(cfg.patterns) > 0 {
			return cfg.resolveAlias(spec, fileSet)
		}
		if d == "" {
			break
		}
	}
	return "", false
}

// resolveAlias matches spec against the config's path patterns (most specific
// first) and resolves the first target that names an existing file.
func (c *tsConfig) resolveAlias(spec string, fileSet map[string]bool) (string, bool) {
	for _, p := range c.patterns {
		star := strings.Index(p.pattern, "*")
		var matched string
		ok := false
		if star < 0 {
			if spec == p.pattern {
				ok = true
			}
		} else {
			prefix, suffix := p.pattern[:star], p.pattern[star+1:]
			if len(spec) >= len(prefix)+len(suffix) &&
				strings.HasPrefix(spec, prefix) && strings.HasSuffix(spec, suffix) {
				matched = spec[len(prefix) : len(spec)-len(suffix)]
				ok = true
			}
		}
		if !ok {
			continue
		}
		for _, target := range p.targets {
			base := path.Join(c.dir, c.baseURL, strings.Replace(target, "*", matched, 1))
			if resolved, found := resolveTSFile(base, fileSet); found {
				return resolved, true
			}
		}
	}
	return "", false
}

// tsResolveExts is the candidate extension order for extensionless
// specifiers; tsJSExts are explicit compiled-output extensions that map back
// to TS sources (import "./x.js" -> x.ts).
var (
	tsResolveExts = []string{tsExt, tsxExt}
	tsJSExts      = []string{".js", ".jsx", ".mjs", ".cjs"}
)

// resolveTSFile resolves a repo-relative base path to an existing file using
// the standard candidate order: exact path, compiled-output extension
// mapping, .ts/.tsx, then directory index files. Deterministic: first match
// in fixed order wins.
func resolveTSFile(base string, fileSet map[string]bool) (string, bool) {
	if base == "" || base == "." || strings.HasPrefix(base, "../") {
		return "", false
	}
	if fileSet[base] {
		return base, true
	}
	for _, jsExt := range tsJSExts {
		if trimmed, ok := strings.CutSuffix(base, jsExt); ok {
			for _, ext := range tsResolveExts {
				if fileSet[trimmed+ext] {
					return trimmed + ext, true
				}
			}
		}
	}
	for _, ext := range tsResolveExts {
		if fileSet[base+ext] {
			return base + ext, true
		}
	}
	for _, ext := range tsResolveExts {
		if candidate := base + "/index" + ext; fileSet[candidate] {
			return candidate, true
		}
	}
	return "", false
}
