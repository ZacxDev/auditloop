package diff

import "sort"

// StringSetDelta returns the elements added (in cur, not prev) and removed (in
// prev, not cur). Inputs are de-duplicated; outputs are sorted for determinism.
// It is the shared primitive behind the page-set delta (by URL) and the a11y
// rule-set delta (by axe rule id).
func StringSetDelta(prev, cur []string) (added, removed []string) {
	prevSet := toSet(prev)
	curSet := toSet(cur)
	for v := range curSet {
		if !prevSet[v] {
			added = append(added, v)
		}
	}
	for v := range prevSet {
		if !curSet[v] {
			removed = append(removed, v)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		if s != "" {
			m[s] = true
		}
	}
	return m
}
