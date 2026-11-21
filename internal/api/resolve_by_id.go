package api

// ResolveByID is the ONE implementation of the by-identifier resolution rule.
// It returns the elements of all named by ids, in the order ids names them. A
// repeated identifier yields its element once, at its first position. An
// identifier that names no element is OMITTED rather than reported, so one
// stale identifier cannot fail the resolution of the ones that exist. An empty
// request returns an empty, non-nil result.
//
// idOf reads an element's identifier. When all holds two elements with the
// same identifier, the last one wins.
//
// The helper documents the resolution rule only. Why a caller resolves outside
// discovery scope belongs to that caller.
func ResolveByID[T any](all []T, ids []string, idOf func(T) string) []T {
	byID := make(map[string]T, len(all))
	for _, element := range all {
		byID[idOf(element)] = element
	}
	resolved := make([]T, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		if element, ok := byID[id]; ok {
			resolved = append(resolved, element)
		}
	}
	return resolved
}
