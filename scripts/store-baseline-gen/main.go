// Command store-baseline-gen regenerates the committed baseline schema
// snapshot from the shipped migration chain. Run go generate ./internal/store.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/peasant-labs/peasant/internal/store"
)

func generate(args []string, output io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: store-baseline-gen <baseline_schema.sql>")
	}
	path := args[0]
	generated, err := store.GenerateSchemaBaselineSQL()
	if err != nil {
		return fmt.Errorf("generate baseline schema SQL: %w", err)
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read existing baseline schema %q: %w; supply its writable file path", path, err)
	}
	if bytes.Equal(existing, generated) {
		_, err = fmt.Fprintln(output, "baseline schema already current")
		return err
	}
	if err := os.WriteFile(path, generated, 0o644); err != nil {
		return fmt.Errorf("write baseline schema %q: %w", path, err)
	}
	_, err = fmt.Fprintln(output, "baseline schema regenerated")
	return err
}

func main() {
	if err := generate(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
