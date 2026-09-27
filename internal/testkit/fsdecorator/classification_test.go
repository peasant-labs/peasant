package fsdecorator_test

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testkit/fsdecorator"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// decoratorClassificationYAML is the real classification of every `*FS` test
// decorator, distinct from classification_cases.yaml (the synthetic validator
// cases). It is committed before any migration so the owner and capability of
// each decorator are reviewable on their own.
//
//go:embed testdata/decorator_classification.yaml
var decoratorClassificationYAML []byte

// classificationDocument carries the fixture's required-name manifest beside the
// frozen Classification shape, so the inventory is checked in the same strict
// decode as the entries.
type classificationDocument struct {
	fsdecorator.Classification `yaml:",inline"`
	RequiredNames              []string `yaml:"required_names"`
}

func loadDecoratorClassification(t *testing.T) classificationDocument {
	t.Helper()
	var doc classificationDocument
	if err := testutil.DecodeFixtureYAML(decoratorClassificationYAML, &doc); err != nil {
		t.Fatalf("decode decorator classification: %v", err)
	}
	if err := fsdecorator.ValidateClassification(doc.Classification); err != nil {
		t.Fatalf("validate decorator classification: %v", err)
	}
	return doc
}

// TestDecoratorClassification_IsCompleteAndResolvable pins the audit: every
// decorator is classified exactly once, the required-name manifest equals the
// entries both ways, and every `where` names a file that really declares the
// type. A stale path or a renamed type fails here instead of silently leaving
// an unclassified decorator behind.
func TestDecoratorClassification_IsCompleteAndResolvable(t *testing.T) {
	doc := loadDecoratorClassification(t)

	const wantEntries = 28
	if got := len(doc.Entries); got != wantEntries {
		t.Fatalf("classification has %d entries, want %d", got, wantEntries)
	}

	byType := make(map[string]fsdecorator.ClassificationEntry, len(doc.Entries))
	for _, entry := range doc.Entries {
		byType[entry.Type] = entry
	}
	if len(doc.RequiredNames) != len(doc.Entries) {
		t.Errorf("required_names has %d names but there are %d entries", len(doc.RequiredNames), len(doc.Entries))
	}
	for _, name := range doc.RequiredNames {
		if _, ok := byType[name]; !ok {
			t.Errorf("required decorator %q has no classification entry", name)
		}
	}
	for _, entry := range doc.Entries {
		if !contains(doc.RequiredNames, entry.Type) {
			t.Errorf("classified decorator %q is missing from required_names", entry.Type)
		}
	}

	root := testutil.ModuleRoot(t)
	for _, entry := range doc.Entries {
		path := filepath.Join(root, entry.Where[:strings.IndexByte(entry.Where, ':')])
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: where file %s is unreadable: %v", entry.Type, path, err)
			continue
		}
		if !strings.Contains(string(data), "type "+entry.Type) {
			t.Errorf("%s: %s does not declare `type %s`", entry.Type, path, entry.Type)
		}
		if entry.Owner() == "" {
			t.Errorf("%s: no owner derives from white_box", entry.Type)
		}
	}
}

// TestDecoratorClassification_OwnerSplit pins the two-owner split: the ten
// white-box `package ingest` decorators route to OwnerIngestWhiteBox and the
// rest to OwnerTestutil.
func TestDecoratorClassification_OwnerSplit(t *testing.T) {
	doc := loadDecoratorClassification(t)

	var whiteBox, outside int
	owners := map[fsdecorator.Owner]int{}
	for _, entry := range doc.Entries {
		if entry.WhiteBox {
			whiteBox++
		} else {
			outside++
		}
		owners[entry.Owner()]++
	}
	const wantWhiteBox = 10
	if whiteBox != wantWhiteBox {
		t.Errorf("white-box decorators = %d, want %d", whiteBox, wantWhiteBox)
	}
	if outside != len(doc.Entries)-wantWhiteBox {
		t.Errorf("non-white-box decorators = %d, want %d", outside, len(doc.Entries)-wantWhiteBox)
	}
	if owners[fsdecorator.OwnerIngestWhiteBox] != wantWhiteBox {
		t.Errorf("OwnerIngestWhiteBox count = %d, want %d", owners[fsdecorator.OwnerIngestWhiteBox], wantWhiteBox)
	}
	if owners[fsdecorator.OwnerTestutil] == 0 {
		t.Error("OwnerTestutil has no classified decorators")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
