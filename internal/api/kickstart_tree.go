package api

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

// tree returns the whole selection forest of a session, each row under the
// session's answer. The server computes every row's state and lock; the page
// never recomputes either.
func (st *kickstartSessions) tree(id string) (schema.LocalKickstartTree, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sess, err := st.liveLocked(id)
	if err != nil {
		return schema.LocalKickstartTree{}, err
	}
	return kickstartTreeWire(sess.tree.Forest)
}

// kickstartTreeWire projects a forest onto the wire. Every kind and state goes
// through its contract constructor, so a row the contract does not know fails
// here rather than reaching the page as a token it cannot parse.
func kickstartTreeWire(forest onboarding.Forest) (schema.LocalKickstartTree, error) {
	tree := schema.LocalKickstartTree{Nodes: make([]schema.LocalKickstartNode, 0, len(forest.Nodes))}
	for _, node := range forest.Nodes {
		kind, err := schema.NewLocalKickstartNodeKind(node.Kind.String())
		if err != nil {
			return schema.LocalKickstartTree{}, fmt.Errorf("project forest row %q onto the wire: %w", node.ID, err)
		}
		state, err := schema.NewLocalKickstartNodeState(node.State.String())
		if err != nil {
			return schema.LocalKickstartTree{}, fmt.Errorf("project forest row %q onto the wire: %w", node.ID, err)
		}
		label := node.Label
		if label == "" {
			label = node.ID
		}
		tree.Nodes = append(tree.Nodes, schema.LocalKickstartNode{
			ID:         node.ID,
			ParentID:   node.ParentID,
			Kind:       kind,
			State:      state,
			Label:      label,
			SessionID:  node.SessionID,
			Harness:    string(node.Harness),
			ChildCount: node.ChildCount,
			Locked:     node.Locked,
		})
	}
	return tree, nil
}
