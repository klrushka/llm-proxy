// Package merge deterministically combines detection candidates into
// non-overlapping top-level entities with optional contained components. It
// holds no plaintext and makes no ownership decisions; it only resolves
// duplicates and overlaps.
package merge

import (
	"sort"

	"github.com/klrushka/llm-proxy/internal/detection"
)

// Entity is one merged top-level PII span. It embeds the winning candidate and
// carries any lower-priority candidates fully contained within it as
// Components. Components are metadata only and are not emitted as additional
// top-level entities; they are sorted in document order.
type Entity struct {
	detection.Candidate
	Components []detection.Candidate
}

// Merge combines candidates into deterministic, non-overlapping entities
// sorted in document order. Caller input and its Sources slices are never
// mutated. Invalid spans (Start < 0 or End <= Start) are ignored.
func Merge(candidates []detection.Candidate) []Entity {
	merged := mergeExactDuplicates(candidates)
	if len(merged) == 0 {
		return nil
	}

	sort.SliceStable(merged, func(i, j int) bool {
		return higherPriority(merged[i], merged[j])
	})

	var winners []Entity
	for _, c := range merged {
		var containing []int
		var partial []int
		for wi := range winners {
			w := winners[wi].Candidate
			if !overlap(c, w) {
				continue
			}
			if contains(w, c) {
				containing = append(containing, wi)
			} else {
				partial = append(partial, wi)
			}
		}
		switch {
		case len(containing) == 0 && len(partial) == 0:
			winners = append(winners, Entity{Candidate: c})
		case len(containing) == 1 && len(partial) == 0:
			wi := containing[0]
			winners[wi].Components = append(winners[wi].Components, c)
		}
	}

	for i := range winners {
		winners[i].Components = dedupeComponents(winners[i].Components)
	}

	sort.SliceStable(winners, func(i, j int) bool {
		return documentOrder(winners[i].Candidate, winners[j].Candidate)
	})
	return winners
}

// dupKey identifies an exact duplicate by Type plus span.
type dupKey struct {
	t     detection.Type
	start int
	end   int
}

// mergeExactDuplicates drops invalid spans and combines candidates that share
// Type+Start+End into one candidate with the maximum confidence and a
// deterministic de-duplicated union of known sources. Returned candidates own
// their Sources slices.
func mergeExactDuplicates(candidates []detection.Candidate) []detection.Candidate {
	groups := make(map[dupKey]*detection.Candidate)
	var order []dupKey
	for _, c := range candidates {
		if c.Start < 0 || c.End <= c.Start {
			continue
		}
		k := dupKey{c.Type, c.Start, c.End}
		g, ok := groups[k]
		if !ok {
			cp := c
			cp.Sources = append([]detection.Source(nil), c.Sources...)
			groups[k] = &cp
			order = append(order, k)
			continue
		}
		if c.Confidence > g.Confidence {
			g.Confidence = c.Confidence
		}
		g.Sources = unionSources(g.Sources, c.Sources)
	}
	out := make([]detection.Candidate, 0, len(order))
	for _, k := range order {
		out = append(out, *groups[k])
	}
	return out
}

// canonicalSourceOrder is the deterministic order used for source unions.
var canonicalSourceOrder = []detection.Source{
	detection.SourceRubert,
	detection.SourceGliner,
	detection.SourceRegex,
	detection.SourceValidator,
}

// unionSources returns the deterministic de-duplicated union of a and b,
// keeping only the known source values in canonical order.
func unionSources(a, b []detection.Source) []detection.Source {
	present := make(map[detection.Source]bool)
	for _, s := range a {
		present[s] = true
	}
	for _, s := range b {
		present[s] = true
	}
	var out []detection.Source
	for _, s := range canonicalSourceOrder {
		if present[s] {
			out = append(out, s)
		}
	}
	return out
}

// tier ranks provenance: validator beats regex beats model-only.
func tier(c detection.Candidate) int {
	hasValidator := false
	hasRegex := false
	for _, s := range c.Sources {
		switch s {
		case detection.SourceValidator:
			hasValidator = true
		case detection.SourceRegex:
			hasRegex = true
		}
	}
	if hasValidator {
		return 3
	}
	if hasRegex {
		return 2
	}
	return 1
}

// higherPriority reports whether a outranks b for overlap resolution.
func higherPriority(a, b detection.Candidate) bool {
	ta, tb := tier(a), tier(b)
	if ta != tb {
		return ta > tb
	}
	if a.Confidence != b.Confidence {
		return a.Confidence > b.Confidence
	}
	aw, bw := a.End-a.Start, b.End-b.Start
	if aw != bw {
		return aw > bw
	}
	if a.Start != b.Start {
		return a.Start < b.Start
	}
	if a.End != b.End {
		return a.End < b.End
	}
	return a.Type < b.Type
}

// overlap reports whether two spans share any byte.
func overlap(a, b detection.Candidate) bool {
	return a.Start < b.End && b.Start < a.End
}

// contains reports whether inner is fully contained within outer.
func contains(outer, inner detection.Candidate) bool {
	return outer.Start <= inner.Start && inner.End <= outer.End
}

// documentOrder sorts by Start, then End, then Type.
func documentOrder(a, b detection.Candidate) bool {
	if a.Start != b.Start {
		return a.Start < b.Start
	}
	if a.End != b.End {
		return a.End < b.End
	}
	return a.Type < b.Type
}

// dedupeComponents sorts components in document order and removes exact
// duplicates.
func dedupeComponents(in []detection.Candidate) []detection.Candidate {
	if len(in) == 0 {
		return nil
	}
	sort.SliceStable(in, func(i, j int) bool {
		return documentOrder(in[i], in[j])
	})
	out := make([]detection.Candidate, 0, len(in))
	for _, c := range in {
		if len(out) > 0 && sameCandidate(out[len(out)-1], c) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// sameCandidate reports whether two candidates are identical including sources.
func sameCandidate(a, b detection.Candidate) bool {
	if a.Type != b.Type || a.Start != b.Start || a.End != b.End || a.Confidence != b.Confidence {
		return false
	}
	if len(a.Sources) != len(b.Sources) {
		return false
	}
	for i := range a.Sources {
		if a.Sources[i] != b.Sources[i] {
			return false
		}
	}
	return true
}
