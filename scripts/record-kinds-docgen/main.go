// Command record-kinds-docgen generates the checked-in registry and its human
// document from adapter vocabularies. Run go generate ./internal/ingest/.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/peasant-labs/peasant/internal/ingest"
)

func generate(args []string, output io.Writer) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: record-kinds-docgen <record_kinds.yaml> <docs/record-kinds.md>")
	}
	registryYAML, err := ingest.GenerateRecordKindRegistryYAML()
	if err != nil {
		return fmt.Errorf("generate record-kind registry YAML: %w", err)
	}
	registry, err := ingest.LoadRecordKindRegistry()
	if err != nil {
		return fmt.Errorf("load generated registry before generating document: %w", err)
	}
	document := []byte(registry.Document())
	yamlCurrent, err := generatedFileCurrent(args[0], registryYAML)
	if err != nil {
		return err
	}
	documentCurrent, err := generatedFileCurrent(args[1], document)
	if err != nil {
		return err
	}
	if !yamlCurrent {
		if err := os.WriteFile(args[0], registryYAML, 0o644); err != nil {
			return fmt.Errorf("write generated registry YAML %q: %w", args[0], err)
		}
	}
	if !documentCurrent {
		if err := os.WriteFile(args[1], document, 0o644); err != nil {
			return fmt.Errorf("write generated document %q: %w", args[1], err)
		}
	}
	if yamlCurrent && documentCurrent {
		_, err = fmt.Fprintln(output, "record-kind registry and document already current")
		return err
	}
	_, err = fmt.Fprintln(output, "record-kind registry and document regenerated")
	return err
}

func generatedFileCurrent(path string, generated []byte) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read existing generated file %q: %w; supply its writable file path", path, err)
	}
	return bytes.Equal(raw, generated), nil
}

func main() {
	if err := generate(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
