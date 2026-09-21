// Command record-kinds-docgen rewrites the generated per-kind table in the
// record-kind registry's human view. It takes the document path as its only
// argument. The ingested package owns the table rendering; this command only
// splices it between the document's markers:
//
//	go run ./scripts/record-kinds-docgen docs/record-kinds.md
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
)

const (
	beginMarker = "<!-- BEGIN GENERATED RECORD KINDS: do not hand-edit; run go generate ./internal/ingest/ -->"
	endMarker   = "<!-- END GENERATED RECORD KINDS -->"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: record-kinds-docgen <docs/record-kinds.md>")
		os.Exit(2)
	}
	path := os.Args[1]
	registry, err := ingest.LoadRecordKindRegistry()
	if err != nil {
		fmt.Fprintln(os.Stderr, "load registry:", err)
		os.Exit(1)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read document:", err)
		os.Exit(1)
	}
	document := string(raw)
	begin := strings.Index(document, beginMarker)
	end := strings.Index(document, endMarker)
	if begin < 0 || end < 0 || end < begin {
		fmt.Fprintln(os.Stderr, "document lacks the generated-table markers")
		os.Exit(1)
	}
	updated := document[:begin+len(beginMarker)] + "\n\n" + registry.Markdown() + document[end:]
	if updated != document {
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write document:", err)
			os.Exit(1)
		}
		fmt.Println("record-kinds table regenerated")
		return
	}
	fmt.Println("record-kinds table already current")
}
