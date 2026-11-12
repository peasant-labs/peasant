package onboarding_test

import (
	_ "embed"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/selection_ids.yaml
var selectionIDsData []byte

type selectionIDsCase struct {
	Name           string   `yaml:"name"`
	Op             string   `yaml:"op"`
	Value          string   `yaml:"value"`
	IDs            []string `yaml:"ids"`
	Want           string   `yaml:"want"`
	WantIsIDAnswer bool     `yaml:"wantIsIDAnswer"`
	WantIDs        []string `yaml:"wantIDs"`
}

type selectionIDsDocument struct {
	RequiredNames []string           `yaml:"requiredNames"`
	Cases         []selectionIDsCase `yaml:"cases"`
}

// TestSelectionIDs runs the tree-answer encoding fixture through the module
// every presentation uses.
func TestSelectionIDs(t *testing.T) {
	t.Parallel()
	var document selectionIDsDocument
	decodeFixture(t, "selection_ids.yaml", selectionIDsData, &document)
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	requireNames(t, document.RequiredNames, names)

	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			switch row.Op {
			case "decode":
				ids, isIDAnswer := onboarding.SelectionIDs(schema.LocalSettingValue(row.Value))
				if isIDAnswer != row.WantIsIDAnswer {
					t.Fatalf("SelectionIDs(%q) id answer = %t, want %t", row.Value, isIDAnswer, row.WantIsIDAnswer)
				}
				if row.WantIsIDAnswer && !reflect.DeepEqual(ids, nonNilStrings(row.WantIDs)) {
					t.Fatalf("SelectionIDs(%q) = %#v, want %#v", row.Value, ids, nonNilStrings(row.WantIDs))
				}
			case "encode":
				got, err := onboarding.SelectionIDsValue(row.IDs)
				if err != nil {
					t.Fatalf("SelectionIDsValue(%v): %v", row.IDs, err)
				}
				if string(got) != row.Want {
					t.Fatalf("SelectionIDsValue(%v) = %s, want %s", row.IDs, got, row.Want)
				}
				// What is encoded decodes back to the same ids.
				ids, isIDAnswer := onboarding.SelectionIDs(got)
				if !isIDAnswer || !reflect.DeepEqual(ids, nonNilStrings(row.IDs)) {
					t.Fatalf("SelectionIDs(SelectionIDsValue(%v)) = %v (id answer %t)", row.IDs, ids, isIDAnswer)
				}
			case "list":
				got, err := onboarding.SelectionIDsListValue(row.IDs)
				if err != nil {
					t.Fatalf("SelectionIDsListValue(%v): %v", row.IDs, err)
				}
				if string(got) != row.Want {
					t.Fatalf("SelectionIDsListValue(%v) = %s, want %s", row.IDs, got, row.Want)
				}
			default:
				t.Fatalf("fixture case %q has op %q, want decode, encode, or list", row.Name, row.Op)
			}
		})
	}
}
