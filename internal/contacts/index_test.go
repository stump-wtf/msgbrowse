package contacts

import "testing"

// TestNameIndex covers the identifier→name lookup the settings render uses:
// exact matches per Kind, the KindPhone cross-shape widening (national ↔
// international, the phonesMatch rule), and the misses that must stay empty.
func TestNameIndex(t *testing.T) {
	people := []Person{
		{
			Key: "p1", DisplayName: "Jane Doe",
			Identifiers: []Identifier{
				{Kind: KindPhone, Value: "+15557770001"},
				{Kind: KindEmail, Value: "jane@example.com"},
			},
		},
		{
			Key: "p2", DisplayName: "Max Power",
			Identifiers: []Identifier{
				{Kind: KindPhone, Value: "5557770002"}, // national form in the book
				{Kind: KindHandle, Value: "max.power"},
			},
		},
	}
	ix := BuildNameIndex(people)

	tests := []struct {
		name string
		id   Identifier
		want string
	}{
		{"exact phone", Identifier{KindPhone, "+15557770001"}, "Jane Doe"},
		{"exact email", Identifier{KindEmail, "jane@example.com"}, "Jane Doe"},
		{"exact handle", Identifier{KindHandle, "max.power"}, "Max Power"},
		// The one documented cross-shape rule: a stored national number finds
		// the book's international form, and vice versa.
		{"national finds international", Identifier{KindPhone, "5557770001"}, "Jane Doe"},
		{"international finds national", Identifier{KindPhone, "+15557770002"}, "Max Power"},
		// Misses stay empty — including a value that only matches under a
		// different Kind, and the zero identifier.
		{"unknown phone", Identifier{KindPhone, "+15550000000"}, ""},
		{"kind mismatch", Identifier{KindHandle, "jane@example.com"}, ""},
		{"zero identifier", Identifier{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ix.Name(tt.id); got != tt.want {
				t.Errorf("Name(%+v) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

// TestNameIndexNormalizedLookup is the end-to-end shape the web layer uses:
// a raw stored identifier goes through Normalize and must land on the person —
// the zero-Kind, un-normalized lookup that used to be sent to Resolve never
// could (Kind "" matches nothing).
func TestNameIndexNormalizedLookup(t *testing.T) {
	ix := BuildNameIndex([]Person{{
		Key: "p1", DisplayName: "Jane Doe",
		Identifiers: []Identifier{{Kind: KindPhone, Value: "+15557770001"}},
	}})
	if got := ix.Name(Normalize("+1 (555) 777-0001")); got != "Jane Doe" {
		t.Errorf("normalized raw lookup = %q, want %q", got, "Jane Doe")
	}
	// The un-normalized zero-Kind identifier — the old bug — must miss.
	if got := ix.Name(Identifier{Value: "+1 (555) 777-0001"}); got != "" {
		t.Errorf("zero-Kind lookup = %q, want no match", got)
	}
}

// TestNameIndexNilAndFirstWins: a nil index answers "", and when two people
// share an identifier the first (provider order, Resolve's best-match-first)
// wins.
func TestNameIndexNilAndFirstWins(t *testing.T) {
	var nilIx *NameIndex
	if got := nilIx.Name(Identifier{KindPhone, "+15557770001"}); got != "" {
		t.Errorf("nil index = %q, want empty", got)
	}
	ix := BuildNameIndex([]Person{
		{Key: "a", DisplayName: "First", Identifiers: []Identifier{{KindEmail, "shared@example.com"}}},
		{Key: "b", DisplayName: "Second", Identifiers: []Identifier{{KindEmail, "shared@example.com"}}},
	})
	if got := ix.Name(Identifier{KindEmail, "shared@example.com"}); got != "First" {
		t.Errorf("shared identifier = %q, want %q (first person wins)", got, "First")
	}
}
