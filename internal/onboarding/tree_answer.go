package onboarding

// TreeAnswer is a forest read under an answered set of node ids.
type TreeAnswer struct {
	// Forest is the input forest with every row's state computed from the
	// answer. Provenance and locked rows are kept as the input carried them.
	Forest Forest
	// SessionIDs are the session rows the answer selects, in forest order. A
	// parent id in the answer selects the sessions beneath it, and the result
	// names those sessions, never the parent. This is the set a picker reports
	// and receives, and the only form that survives a rescan without widening:
	// a session that appears later is not in the set.
	SessionIDs []string
}

// ApplyTreeAnswer reads forest under the answered node ids. It applies the same
// answer rule deriveSelection applies and rolls parents up from their children,
// so a presentation shows exactly the states the derived selection will use.
// ids may name sessions or rows that group them. An id that matches no row, or
// more than one row, selects nothing. A locked row is never selected. A nil ids
// is an empty answer: every row that is not locked reads as unselected. The
// input forest is not changed.
func ApplyTreeAnswer(forest Forest, ids []string) TreeAnswer {
	if ids == nil {
		ids = []string{}
	}
	roots := selectionTree(forest)
	applyAnswer(roots, ids)
	for _, root := range roots {
		rollupNode(root)
	}
	answered := writeForest(forest, roots)
	return TreeAnswer{Forest: answered, SessionIDs: SelectedSessionIDs(answered)}
}

// SelectedSessionIDs lists the session rows a forest already shows as selected,
// in forest order. A locked row is not selected. It is the picker's value for a
// forest whose states came from a saved selection rather than from an answer.
func SelectedSessionIDs(forest Forest) []string {
	selected := []string{}
	for _, node := range forest.Nodes {
		if node.Kind == NodeKindSession && node.State == NodeStateAll && !node.Locked {
			selected = append(selected, node.ID)
		}
	}
	return selected
}

// LockedRows counts the rows a forest marks locked.
func LockedRows(forest Forest) int {
	locked := 0
	for _, node := range forest.Nodes {
		if node.Locked {
			locked++
		}
	}
	return locked
}
