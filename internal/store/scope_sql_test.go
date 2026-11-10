package store

import "testing"

// TestScopedWhereGroupsTheDisjunction pins the one composer every multi-arm
// scoped statement uses. Grouping is its whole purpose: an unparenthesised OR
// chain would bind the appended narrowed clause to the last arm only, so the
// composer must group even when the narrowed clause is empty.
func TestScopedWhereGroupsTheDisjunction(t *testing.T) {
	t.Parallel()
	grouped, err := scopedWhere([]string{"s.model_harness = 'codex'", "s.model_harness = 'opencode'"}, " AND s.session_id IN (?,?)")
	if err != nil {
		t.Fatal(err)
	}
	const wantGrouped = "(s.model_harness = 'codex' OR s.model_harness = 'opencode') AND s.session_id IN (?,?)"
	if grouped != wantGrouped {
		t.Fatalf("scopedWhere = %q, want %q", grouped, wantGrouped)
	}

	all, err := scopedWhere([]string{"a = 1", "b = 2"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if all != "(a = 1 OR b = 2)" {
		t.Fatalf("scopedWhere without a narrowed clause = %q, want the grouped arms", all)
	}

	if _, err := scopedWhere(nil, " AND s.session_id IN (?)"); err == nil {
		t.Fatal("scopedWhere accepted no selection arms; a pass with no arms must be refused")
	}
}
