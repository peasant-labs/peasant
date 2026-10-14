// Capture writes observed surfaces using only production code from its build.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/testkit/contentparity"
)

func main() {
	sources := flag.String("sources", "", "source YAML path")
	manifest := flag.String("manifest", "", "required-name manifest path")
	output := flag.String("out", "", "empty output directory")
	flag.Parse()
	if err := capture(*sources, *manifest, *output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func capture(sources, manifest, output string) error {
	data, err := os.ReadFile(sources)
	if err != nil {
		return err
	}
	names, err := os.ReadFile(manifest)
	if err != nil {
		return err
	}
	cases, err := contentparity.Load(data, names)
	if err != nil {
		return err
	}
	if output == "" {
		return fmt.Errorf("capture parity: supply --out naming a new sandbox output directory")
	}
	if err := os.Mkdir(output, 0755); err != nil {
		return err
	}
	for _, c := range cases {
		observed, err := contentparity.Run(context.Background(), c)
		if err != nil {
			return fmt.Errorf("capture %s: %w", c.Name, err)
		}
		raw, err := json.MarshalIndent(observed, "", "  ")
		if err != nil {
			return err
		}
		raw = append(raw, '\n')
		if err := os.WriteFile(filepath.Join(output, c.Name+".json"), raw, 0600); err != nil {
			return err
		}
		fmt.Println(c.Name)
	}
	return nil
}
