package onboarding_test

import (
	_ "embed"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/onboarding"
)

//go:embed testdata/tree_answer.yaml
var treeAnswerData []byte

type treeAnswerNode struct {
	ID     string `yaml:"id"`
	Parent string `yaml:"parent"`
	Kind   string `yaml:"kind"`
	Label  string `yaml:"label"`
}

type treeAnswerCase struct {
	Name       string            `yaml:"name"`
	Locked     []string          `yaml:"locked"`
	IDs        *[]string         `yaml:"ids"`
	Selected   []string          `yaml:"selected"`
	States     map[string]string `yaml:"states"`
	LockedRows []string          `yaml:"lockedRows"`
}

type treeAnswerDocument struct {
	RequiredNames []string         `yaml:"requiredNames"`
	Forest        []treeAnswerNode `yaml:"forest"`
	Cases         []treeAnswerCase `yaml:"cases"`
}

func TestApplyTreeAnswer(t *testing.T) {
	t.Parallel()
	var document treeAnswerDocument
	decodeFixture(t, "tree_answer.yaml", treeAnswerData, &document)
	names := make([]string, len(document.Cases))
	for i, row := range document.Cases {
		names[i] = row.Name
	}
	requireNames(t, document.RequiredNames, names)

	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			forest := treeAnswerForest(t, document.Forest, row.Locked)
			before := append([]onboarding.Node(nil), forest.Nodes...)

			var ids []string
			if row.IDs != nil {
				ids = *row.IDs
			}
			got := onboarding.ApplyTreeAnswer(forest, ids)

			if want := nonNilStrings(row.Selected); !reflect.DeepEqual(got.SessionIDs, want) {
				t.Errorf("selected sessions = %v, want %v", got.SessionIDs, want)
			}
			if !reflect.DeepEqual(got.SessionIDs, onboarding.SelectedSessionIDs(got.Forest)) {
				t.Errorf("SessionIDs %v is not what SelectedSessionIDs reads from the answered forest", got.SessionIDs)
			}
			states := map[string]onboarding.NodeState{}
			for _, node := range got.Forest.Nodes {
				states[node.ID] = node.State
			}
			for id, want := range row.States {
				if string(states[id]) != want {
					t.Errorf("state of %s = %q, want %q", id, states[id], want)
				}
			}
			if !reflect.DeepEqual(forest.Nodes, before) {
				t.Errorf("ApplyTreeAnswer changed the forest it was given")
			}
			var locked []string
			for _, node := range got.Forest.Nodes {
				if node.Locked {
					locked = append(locked, node.ID)
				}
			}
			if !reflect.DeepEqual(locked, row.LockedRows) {
				t.Errorf("locked rows = %v, want %v", locked, row.LockedRows)
			}
			if count := onboarding.LockedRows(got.Forest); count != len(row.LockedRows) {
				t.Errorf("LockedRows = %d, want %d", count, len(row.LockedRows))
			}
		})
	}
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func treeAnswerForest(t *testing.T, rows []treeAnswerNode, locked []string) onboarding.Forest {
	t.Helper()
	lockedSet := map[string]bool{}
	for _, id := range locked {
		lockedSet[id] = true
	}
	nodes := make([]onboarding.Node, len(rows))
	for i, row := range rows {
		kind, err := onboarding.NewNodeKind(row.Kind)
		if err != nil {
			t.Fatalf("fixture node %s: %v", row.ID, err)
		}
		nodes[i] = onboarding.Node{
			ID: row.ID, ParentID: row.Parent, Kind: kind, Label: row.Label,
			State: onboarding.NodeStateNone, Locked: lockedSet[row.ID],
		}
		if kind == onboarding.NodeKindSession {
			nodes[i].SessionID = row.ID
		}
	}
	return onboarding.Forest{Nodes: nodes, Total: len(nodes)}
}
