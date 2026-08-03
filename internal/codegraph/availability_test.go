package codegraph_test

// TestTSAnalysisAvailability is the build-mode gate for TypeScript code
// graphs. It is UNTAGGED on purpose (the redact.MaximumAvailable pattern,
// maximum_available_test.go): it compiles and runs in BOTH the cgo and !cgo
// test legs, branching on the build-tag-set codegraph.TSAnalysisAvailable
// constant rather than silently skipping.
//
//   - !cgo (TSAnalysisAvailable == false): Build over a tree containing
//     TypeScript MUST fail with an actionable error, and a Go-only tree MUST
//     still build (the extractor is constructed lazily).
//   - cgo  (TSAnalysisAvailable == true):  Build over the same TS tree MUST
//     succeed and extract the TS import edge.

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/codegraph"
)

func availRead(files map[string]string) (paths []string, reader codegraph.FileReader) {
	for p := range files {
		paths = append(paths, p)
	}
	return paths, func(p string) ([]byte, error) { return []byte(files[p]), nil }
}

var availGoOnly = map[string]string{
	"go.mod":              "module example.com/m\n",
	"cmd/app/main.go":     "package main\n\nimport \"example.com/m/internal/svc\"\n\nfunc main() { svc.Run() }\n",
	"internal/svc/svc.go": "package svc\n\nfunc Run() {}\n",
}

var availWithTS = map[string]string{
	"go.mod":              "module example.com/m\n",
	"cmd/app/main.go":     "package main\n\nfunc main() {}\n",
	"web/src/app/page.ts": "import { x } from \"../lib/util\";\n",
	"web/src/lib/util.ts": "export const x = 1;\n",
}

func TestTSAnalysisAvailability(t *testing.T) {
	ctx := context.Background()

	// A Go-only tree builds in EVERY build mode: the TS extractor is lazy.
	paths, read := availRead(availGoOnly)
	if _, err := codegraph.NewGraphBuilder().Build(ctx, paths, read); err != nil {
		t.Fatalf("Go-only Build must succeed in every build mode, got: %v", err)
	}

	paths, read = availRead(availWithTS)
	g, err := codegraph.NewGraphBuilder().Build(ctx, paths, read)

	if !codegraph.TSAnalysisAvailable {
		// !cgo: fail ACTIONABLY, never render a partial graph as complete.
		if err == nil {
			t.Fatal("Build over a TS tree must fail in a !cgo build (TSAnalysisAvailable == false), got nil error")
		}
		for _, want := range []string{"TypeScript analysis is unavailable", "CGO_ENABLED=0", "cgo-enabled peasant build"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("!cgo Build error must contain %q (actionable), got: %v", want, err)
			}
		}
		return
	}

	// cgo: the TS tree builds and the import edge is extracted.
	if err != nil {
		t.Fatalf("Build over a TS tree must succeed in a cgo build, got: %v", err)
	}
	found := false
	for _, e := range g.Edges {
		if e.From == "web/src/app" && e.To == "web/src/lib" {
			found = true
		}
	}
	if !found {
		t.Errorf("cgo Build must extract the web/src/app -> web/src/lib TS import edge; edges: %+v", g.Edges)
	}
}
