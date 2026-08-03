// Command gen-mock-redactions regenerates the web session-detail redaction mock
// (web/src/lib/session-detail/mock-redactions.ts) from the schema contract
// fixture. It is the canonical regeneration command referenced by the peasant
// freshness gate. Run it from the peasant module root:
//
//	go run ./cmd/gen-mock-redactions
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/redactmock"
)

func main() {
	root, err := moduleRoot()
	if err != nil {
		log.Fatalf("gen-mock-redactions: locate module root: %v", err)
	}

	data, err := redactmock.GenerateMockRedactionsTS()
	if err != nil {
		log.Fatalf("gen-mock-redactions: generate: %v", err)
	}

	path := filepath.Join(root, redactmock.MockRedactionsRelPath)
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Fatalf("gen-mock-redactions: write %s: %v", path, err)
	}
	log.Printf("Generated %s", path)
}

// moduleRoot walks up from the working directory to the directory containing
// go.mod (the peasant module root).
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found from working directory upward")
		}
		dir = parent
	}
}
