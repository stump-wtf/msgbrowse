package contacts

// NameIndex is the prebuilt identifier→display-name lookup the Resolver doc
// tells multi-identifier callers to use: "Callers that need to match many
// identifiers should enumerate once with People() and index the result."
// Providers like macoscontacts re-enumerate the whole address book on every
// Resolve call, so a per-identifier Resolve loop is O(identifiers × book size);
// building this index once makes the whole pass a single enumeration.
//
// Matching follows the package's canonical-equality contract: byte equality
// per Kind, with the ONE documented KindPhone cross-shape exception
// (phonesMatch) — a national number still finds its international
// ("+"-prefixed) form and vice versa, exactly as the merge engine matches.
//
// Build it with BuildNameIndex; a nil index answers every lookup with "".
type NameIndex struct {
	exact map[Identifier]string
	// phones repeats the KindPhone entries in order, for the cross-shape
	// fallback a map lookup by exact value cannot serve.
	phones []phoneName
}

type phoneName struct {
	value string
	name  string
}

// BuildNameIndex indexes people by their canonical identifiers. When several
// people carry the same identifier the first (provider order) wins, matching
// Resolve's best-match-first contract. Zero identifiers are skipped.
func BuildNameIndex(people []Person) *NameIndex {
	ix := &NameIndex{exact: make(map[Identifier]string)}
	for _, p := range people {
		for _, id := range p.Identifiers {
			if id.IsZero() {
				continue
			}
			if _, ok := ix.exact[id]; !ok {
				ix.exact[id] = p.DisplayName
			}
			if id.Kind == KindPhone {
				ix.phones = append(ix.phones, phoneName{id.Value, p.DisplayName})
			}
		}
	}
	return ix
}

// Name returns the display name of the first person carrying the canonical
// identifier, or "" when the address book has no match (or the index is nil).
// KindPhone falls back to the cross-shape compare (phonesMatch) when the exact
// value misses, so a stored national number matches its international form.
func (ix *NameIndex) Name(id Identifier) string {
	if ix == nil || id.IsZero() {
		return ""
	}
	if name, ok := ix.exact[id]; ok {
		return name
	}
	if id.Kind == KindPhone {
		for _, p := range ix.phones {
			if phonesMatch(id.Value, p.value) {
				return p.name
			}
		}
	}
	return ""
}
