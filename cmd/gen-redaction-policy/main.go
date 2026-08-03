// Command gen-redaction-policy regenerates the share wizard's view of the
// redaction policy (web/src/lib/share/redaction-policy.generated.ts) from
// internal/config. It is the canonical regeneration command referenced by the
// freshness gate. Run it from the peasant module root:
//
//	go run ./cmd/gen-redaction-policy
package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/redactionpolicygen"
)

func main() {
	root, err := moduleRoot()
	if err != nil {
		log.Fatalf("gen-redaction-policy: locate module root: %v", err)
	}

	data, err := redactionpolicygen.GenerateRedactionPolicyTS()
	if err != nil {
		log.Fatalf("gen-redaction-policy: generate: %v", err)
	}

	path := filepath.Join(root, redactionpolicygen.RedactionPolicyRelPath.String())
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Fatalf("gen-redaction-policy: write %s: %v", path, err)
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
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
