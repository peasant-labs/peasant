// Command record-kinds-docgen generates the entire human registry from the
// embedded YAML. Run go generate ./internal/ingest/ after registry changes.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/peasant-labs/peasant/internal/ingest"
)

func generate(args []string, output io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: record-kinds-docgen <docs/record-kinds.md>")
	}
	registry, err := ingest.LoadRecordKindRegistry()
	if err != nil {
		return fmt.Errorf("load registry before generating document: %w", err)
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return fmt.Errorf("read existing document %q: %w; supply its writable file path", args[0], err)
	}
	updated := registry.Document()
	if string(raw) == updated {
		_, err = fmt.Fprintln(output, "record-kinds document already current")
		return err
	}
	if err := os.WriteFile(args[0], []byte(updated), 0o644); err != nil {
		return fmt.Errorf("write generated document %q: %w", args[0], err)
	}
	_, err = fmt.Fprintln(output, "record-kinds document regenerated")
	return err
}

func main() {
	if err := generate(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
