package settings

import (
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/schema"
)

// SelectionForest projects a kit tree into the neutral forest.
func SelectionForest(roots []*kit.TreeNode) onboarding.Forest {
	return selectionForest(roots)
}

// CheckedSelectionIDs returns the neutral ids of checked rows, in tree order.
func CheckedSelectionIDs(roots []*kit.TreeNode) []string {
	return checkedForestIDs(roots)
}

// selectionForest projects a kit tree into the neutral forest. Session ids stay
// the session's own id. Other ids are path-qualified so two branches that share
// a display id remain distinct. Candidates carry the same identity the kit
// round-trip reads from node meta.
func selectionForest(roots []*kit.TreeNode) onboarding.Forest {
	projectFirst := isProjectFirstForest(roots)
	var nodes []onboarding.Node
	var walk func(node *kit.TreeNode, parentID string, parentKind onboarding.NodeKind, inherited onboarding.Node)
	walk = func(node *kit.TreeNode, parentID string, parentKind onboarding.NodeKind, inherited onboarding.Node) {
		if node == nil {
			return
		}
		kind := forestKind(node, parentKind, projectFirst)
		id := forestNodeID(parentID, node.ID, kind)
		built := onboarding.Node{
			ID:              id,
			ParentID:        parentID,
			Kind:            kind,
			Label:           node.Label,
			State:           forestState(node.State),
			Locked:          node.State == kit.Conflict,
			ExplicitBranch:  metaOf(node, metaExplicitBranchSelection) == "true",
			ExplicitSession: metaOf(node, metaExplicitSessionSelection) == "true",
			Identity:        projectIdentityOf(node),
			Candidate:       forestCandidate(node, kind, inherited),
		}
		if harness := harnessOf(node); harness != "" {
			built.Harness = defaults.Harness(harness)
			built.Candidate.Harness = ingest.Harness(harness)
		} else if inherited.Candidate.Harness != "" {
			built.Harness = defaults.Harness(inherited.Candidate.Harness)
			built.Candidate.Harness = inherited.Candidate.Harness
		}
		if kind == onboarding.NodeKindSession {
			built.SessionID = node.ID
			built.Candidate.SessionID = ingest.SessionID(node.ID)
		}
		nodes = append(nodes, built)
		for _, child := range node.Children {
			walk(child, id, kind, built)
		}
	}
	for _, root := range roots {
		walk(root, "", "", onboarding.Node{})
	}
	return onboarding.Forest{Nodes: nodes, Total: len(nodes)}
}

func forestKind(node *kit.TreeNode, parent onboarding.NodeKind, projectFirst bool) onboarding.NodeKind {
	if harnessOf(node) != "" {
		return onboarding.NodeKindSession
	}
	if projectFirst {
		switch parent {
		case "":
			return onboarding.NodeKindProject
		case onboarding.NodeKindProject:
			return onboarding.NodeKindBranch
		default:
			return onboarding.NodeKindSession
		}
	}
	switch parent {
	case "":
		return onboarding.NodeKindGroup
	case onboarding.NodeKindGroup:
		return onboarding.NodeKindProject
	case onboarding.NodeKindProject:
		return onboarding.NodeKindBranch
	default:
		return onboarding.NodeKindSession
	}
}

func forestNodeID(parentID, kitID string, kind onboarding.NodeKind) string {
	if kind == onboarding.NodeKindSession || parentID == "" {
		return kitID
	}
	return parentID + "/" + kitID
}

func forestState(state kit.TriState) onboarding.NodeState {
	switch state {
	case kit.Checked:
		return onboarding.NodeStateAll
	case kit.Partial:
		return onboarding.NodeStateSome
	default:
		return onboarding.NodeStateNone
	}
}

func forestCandidate(node *kit.TreeNode, kind onboarding.NodeKind, inherited onboarding.Node) ingest.DiscoveryCandidate {
	candidate := inherited.Candidate
	candidate.SessionID = ""
	if remote := gitRemoteOf(node); remote != "" {
		candidate.GitRemote = remote
	}
	if name := metaOf(node, MetaProjectName); name != "" {
		candidate.ProjectName = name
	} else if kind == onboarding.NodeKindProject && candidate.ProjectName == "" {
		candidate.ProjectName = projectNameOf(node)
	}
	if path := clonePathOf(node); path != "" {
		candidate.ClonePath = path
	}
	switch kind {
	case onboarding.NodeKindBranch:
		candidate.Branch = semanticBranchName(branchOf(node))
	case onboarding.NodeKindSession:
		if inherited.Kind == onboarding.NodeKindBranch {
			candidate.Branch = semanticBranchName(branchOfNode(inherited))
		} else {
			candidate.Branch = ""
		}
	default:
		candidate.Branch = ""
	}
	if multiplicity := multiplicityOf(node, MetaRemoteMultiplicity); multiplicity != ingest.DiscoveryIdentityUnproven {
		candidate.RemoteMultiplicity = multiplicity
	}
	if multiplicity := multiplicityOf(node, MetaNameMultiplicity); multiplicity != ingest.DiscoveryIdentityUnproven {
		candidate.NameMultiplicity = multiplicity
	}
	if harness := harnessOf(node); harness != "" {
		candidate.Harness = ingest.Harness(harness)
	}
	return candidate
}

func branchOfNode(node onboarding.Node) string {
	if node.Candidate.Branch != "" {
		return node.Candidate.Branch
	}
	return node.Label
}

func applySelectionForest(roots []*kit.TreeNode, forest onboarding.Forest) {
	flat := flattenKit(roots)
	if len(flat) != len(forest.Nodes) {
		return
	}
	for index, node := range flat {
		got := forest.Nodes[index]
		switch {
		case got.Locked:
			node.State = kit.Conflict
		case got.State == onboarding.NodeStateAll:
			node.State = kit.Checked
		case got.State == onboarding.NodeStateSome:
			node.State = kit.Partial
		default:
			node.State = kit.Unchecked
		}
		setProvenance(node, metaExplicitBranchSelection, got.ExplicitBranch)
		setProvenance(node, metaExplicitSessionSelection, got.ExplicitSession)
	}
}

func setProvenance(node *kit.TreeNode, key string, on bool) {
	if on {
		if node.Meta == nil {
			node.Meta = map[string]string{}
		}
		node.Meta[key] = "true"
		return
	}
	if node.Meta != nil {
		delete(node.Meta, key)
	}
}

func flattenKit(roots []*kit.TreeNode) []*kit.TreeNode {
	var flat []*kit.TreeNode
	var walk func(*kit.TreeNode)
	walk = func(node *kit.TreeNode) {
		if node == nil {
			return
		}
		flat = append(flat, node)
		for _, child := range node.Children {
			walk(child)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	return flat
}

func checkedForestIDs(roots []*kit.TreeNode) []string {
	forest := selectionForest(roots)
	flat := flattenKit(roots)
	var ids []string
	for index, node := range flat {
		if node.State != kit.Checked || index >= len(forest.Nodes) {
			continue
		}
		ids = append(ids, forest.Nodes[index].ID)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids
}

// deriveThroughChain writes the checked node ids through the selection field
// into a fresh base configuration and returns the derived value. That field is
// the only caller of the neutral derive.
func deriveThroughChain(roots []*kit.TreeNode, auto bool, residual onboarding.Residual) (TreeSelection, error) {
	cfg := config.BaseConfig()
	cfg.Selection.AutoIngestNewBranches = auto
	return writeSelectionIDs(cfg, roots, residual)
}

// writeSelectionIDs stores the checked node ids by deriving them through the
// selection field into cfg and returns the derived value.
func writeSelectionIDs(cfg *config.Config, roots []*kit.TreeNode, residual onboarding.Residual) (TreeSelection, error) {
	if cfg == nil {
		return TreeSelection{}, fmt.Errorf("write selection ids: configuration is nil")
	}
	forest := selectionForest(roots)
	raw, err := json.Marshal(map[string]any{"ids": checkedForestIDs(roots)})
	if err != nil {
		return TreeSelection{}, err
	}
	value := schema.LocalSettingValue(raw)
	answers := onboarding.Answers{onboarding.FieldSelection: value}
	model := onboarding.BuildModel(onboarding.Options{Scope: onboarding.ScopeSelection})
	if err := model.ApplyAnswers(cfg, answers, onboarding.WriteContext{
		Config:   cfg,
		Forest:   forest,
		Residual: residual,
		Answers:  answers,
	}); err != nil {
		return TreeSelection{}, err
	}
	return TreeSelection{Mode: cfg.Selection.Mode, Harnesses: cfg.Selection.Harnesses}, nil
}
