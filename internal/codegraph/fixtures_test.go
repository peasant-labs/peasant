//go:build cgo

// cgo-only: the shared fixture tree (fixtures_test.go) contains TypeScript
// sources, whose import extraction needs the tree-sitter C bindings. The
// build-mode availability gate that runs in BOTH legs is availability_test.go.

package codegraph_test

// Shared synthetic repo fixtures for the codegraph tests. The fixture is a
// small Go + TypeScript tree exercising: Go module-path resolution, TS
// relative/alias/index/js-suffix resolution, JSONC tsconfig parsing,
// language/loc rollups, and deterministic layering.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/codegraph"
)

// mapReader returns a FileReader backed by an in-memory file map.
func mapReader(files map[string]string) codegraph.FileReader {
	return func(p string) ([]byte, error) {
		content, ok := files[p]
		if !ok {
			return nil, fmt.Errorf("mapReader: no file %q", p)
		}
		return []byte(content), nil
	}
}

// pathsOf returns the sorted file paths of a fixture map.
func pathsOf(files map[string]string) []string {
	return slices.Sorted(maps.Keys(files))
}

// mustBuild builds a graph from a fixture map, failing the test on error.
func mustBuild(t *testing.T, files map[string]string) *codegraph.Graph {
	t.Helper()
	g, err := codegraph.NewGraphBuilder().Build(context.Background(), pathsOf(files), mapReader(files))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return g
}

// nodeByID finds a node in the graph, failing the test when absent.
func nodeByID(t *testing.T, g *codegraph.Graph, id string) codegraph.Node {
	t.Helper()
	for _, n := range g.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("node %q not found in graph (have %d nodes)", id, len(g.Nodes))
	return codegraph.Node{}
}

// fixtureFiles is the main synthetic Go + TS repository.
//
// Go import chain: cmd/app -> internal/svc -> internal/store -> internal/util
// (plus direct svc->util and cmd/app->util edges). TS: app page imports lib
// via the "@/*" alias (twice: import + re-export), components via relative
// and alias-index paths; components imports lib/util via a .js-suffixed
// specifier; lib imports lib/util relatively.
func fixtureFiles() map[string]string {
	return map[string]string{
		"go.mod": "module example.com/proj\n\ngo 1.25\n",
		"cmd/app/main.go": `package main

import (
	"fmt"

	"example.com/proj/internal/svc"
	"example.com/proj/internal/util"
)

func main() { fmt.Println(svc.Run(util.V)) }
`,
		"internal/svc/svc.go": `package svc

import (
	"example.com/proj/internal/store"
	"example.com/proj/internal/util"
)

func Run(v int) int { return store.Get() + util.V + v }
`,
		"internal/store/store.go": `package store

import "example.com/proj/internal/util"

func Get() int { return util.V }
`,
		"internal/util/util.go":       "package util\n\nconst V = 1\n",
		"internal/util/util_extra.go": "package util\n\nconst W = 2\n",
		"README.md":                   "# fixture\n",
		"docs/guide.md":               "guide\n",
		"web/tsconfig.json": `{
  // JSONC: comments and trailing commas must parse
  "compilerOptions": {
    "baseUrl": ".",
    /* block comment */
    "paths": {
      "@/*": ["./src/*"],
    },
  },
}
`,
		"web/src/app/page.tsx": `import { api } from "@/lib/api";
import Comp from "../components/Comp";
import { Widget } from "@/components";
import "./styles.css";
import React from "react";
export { api2 } from "@/lib/api";
`,
		"web/src/components/Comp.tsx": `import { helper } from "./helper";
import { fmt } from "../lib/util/format.js";
export default function Comp() { return null; }
`,
		"web/src/components/index.tsx": `import Comp from "./Comp";
export const Widget = Comp;
`,
		"web/src/components/helper.ts": "export const helper = 1;\n",
		"web/src/lib/api.ts": `import { fmt } from "./util/format";
export const api = fmt;
export const api2 = fmt;
`,
		"web/src/lib/util/format.ts": "export const fmt = (s: string) => s;\n",
	}
}

// fixtureEdges is the expected (sorted) package-level edge list for
// fixtureFiles.
func fixtureEdges() []codegraph.Edge {
	return []codegraph.Edge{
		{From: "cmd/app", To: "internal/svc", Count: 1},
		{From: "cmd/app", To: "internal/util", Count: 1},
		{From: "internal/store", To: "internal/util", Count: 1},
		{From: "internal/svc", To: "internal/store", Count: 1},
		{From: "internal/svc", To: "internal/util", Count: 1},
		{From: "web/src/app", To: "web/src/components", Count: 2},
		{From: "web/src/app", To: "web/src/lib", Count: 2},
		{From: "web/src/components", To: "web/src/lib/util", Count: 1},
		{From: "web/src/lib", To: "web/src/lib/util", Count: 1},
	}
}

// cycleFixture has internal/a <-> internal/b importing each other, with
// cmd/tool importing internal/a from above.
func cycleFixture() map[string]string {
	return map[string]string{
		"go.mod": "module example.com/cyc\n",
		"cmd/tool/main.go": `package main

import "example.com/cyc/internal/a"

func main() { _ = a.A }
`,
		"internal/a/a.go": `package a

import "example.com/cyc/internal/b"

const A = b.B
`,
		"internal/b/b.go": `package b

import "example.com/cyc/internal/a"

const B = a.A
`,
	}
}

// wrongWayFixture has internal/a importing cmd/app. The cmd/ pin forces
// cmd/app to layer 0, leaving the edge pointing sideways: a wrong-way
// violation.
func wrongWayFixture() map[string]string {
	return map[string]string{
		"go.mod":          "module example.com/ww\n",
		"cmd/app/main.go": "package main\n\nfunc main() {}\n",
		"internal/a/a.go": `package a

import "example.com/ww/cmd/app"

var _ = app.Thing
`,
	}
}
