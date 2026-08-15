package sentiment

import (
	"strings"
	"testing"
)

// Shape of the shipped table, from ADR-0028's "More Information" section. These
// are pinned so a re-normalization that silently drops or duplicates rows fails
// here rather than quietly shifting every score downstream.
const (
	wantAssignments = 3805
	wantUniqueItems = 1961
	wantLabels      = 246
	wantInstruments = 36
)

func TestEmbeddedTableShape(t *testing.T) {
	items, err := Items()
	if err != nil {
		t.Fatalf("loading embedded IPIP table: %v", err)
	}
	if got := len(items); got != wantAssignments {
		t.Errorf("assignments = %d, want %d", got, wantAssignments)
	}

	texts, labels, instruments := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, it := range items {
		texts[it.Text] = true
		labels[it.Label] = true
		instruments[it.Instrument] = true
	}
	if got := len(texts); got != wantUniqueItems {
		t.Errorf("unique items = %d, want %d", got, wantUniqueItems)
	}
	if got := len(labels); got != wantLabels {
		t.Errorf("labels = %d, want %d", got, wantLabels)
	}
	if got := len(instruments); got != wantInstruments {
		t.Errorf("instruments = %d, want %d", got, wantInstruments)
	}
}

func TestEmbeddedTableCarriesAttribution(t *testing.T) {
	// The table is public domain but IPIP asks to be credited, and ADR-0028 §5
	// makes shipping the attribution part of the decision. Assert it survives
	// any future re-normalization of the data file.
	for _, want := range []string{"International Personality Item Pool", "Oregon Research Institute", "ipip.ori.org", "Public domain"} {
		if !strings.Contains(ipipCSV, want) {
			t.Errorf("embedded table header is missing %q", want)
		}
	}
}

func TestEveryRowIsWellFormed(t *testing.T) {
	items, err := Items()
	if err != nil {
		t.Fatalf("loading embedded IPIP table: %v", err)
	}
	for _, it := range items {
		if strings.TrimSpace(it.Text) == "" {
			t.Errorf("item with empty text (label %q)", it.Label)
		}
		if strings.TrimSpace(it.Label) == "" {
			t.Errorf("item %q has no label", it.Text)
		}
		if it.Key != 1 && it.Key != -1 && it.Key != 0 {
			t.Errorf("item %q: key = %d, want +1/-1/0", it.Text, it.Key)
		}
		if it.Alpha < 0 || it.Alpha > 1 {
			t.Errorf("item %q: alpha = %v, want within [0,1]", it.Text, it.Alpha)
		}
	}
}

// TestSourceDataQuirks pins the four shapes of the source table that items.go
// handles specially. If a future refresh of the .xlsx removes them the handling
// can be simplified; if it adds more rows of these kinds, that is fine — these
// assert the quirks still exist and stay confined to the instruments they came
// from, so the special-casing keeps earning its place.
func TestSourceDataQuirks(t *testing.T) {
	items, err := Items()
	if err != nil {
		t.Fatalf("loading embedded IPIP table: %v", err)
	}

	noAlphaBy := map[string]int{}
	var undetermined int
	for _, it := range items {
		if it.Key == 0 {
			undetermined++
			if it.Instrument != "VIA" {
				t.Errorf("undetermined keying outside VIA: %q (%s)", it.Text, it.Instrument)
			}
		}
		if it.Alpha == 0 {
			noAlphaBy[it.Instrument]++
		}
	}
	if undetermined == 0 {
		t.Error("expected some rows with undetermined keying (key = 0)")
	}
	// "." (IPIP-IPC) plus the out-of-range slips: "1994" (BIS_BAS) and "12" (MPQ).
	for _, inst := range []string{"IPIP-IPC", "BIS_BAS", "MPQ"} {
		if noAlphaBy[inst] == 0 {
			t.Errorf("expected rows with no usable alpha from %s", inst)
		}
		delete(noAlphaBy, inst)
	}
	if len(noAlphaBy) != 0 {
		t.Errorf("unreported alpha appeared in unexpected instruments: %v", noAlphaBy)
	}
}

// TestNoAnchorCandidateHasImpossibleAlpha is the regression guard for the
// source's out-of-range alphas. Before parseAlpha rejected them, "1994" sorted
// above every genuine reliability, so six BIS_BAS rows captured the Anxiety
// construct's anchor slots purely on a citation year.
func TestNoAnchorCandidateHasImpossibleAlpha(t *testing.T) {
	items, err := Items()
	if err != nil {
		t.Fatalf("loading embedded IPIP table: %v", err)
	}
	for _, it := range items {
		if it.Alpha > 1 {
			t.Errorf("item %q (%s/%s) has alpha %v — an out-of-range value reached anchor selection",
				it.Text, it.Instrument, it.Label, it.Alpha)
		}
	}
}

func TestParseAlpha(t *testing.T) {
	tests := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{in: "0.78", want: 0.78},
		{in: "0.59,0.63", want: 0.63}, // multi-scale: highest wins
		{in: "0.80,0.64", want: 0.80}, // order does not matter
		{in: ".", want: 0},            // not reported
		{in: "", want: 0},             //
		{in: "not-a-number", wantErr: true},
	}
	for _, tc := range tests {
		got, err := parseAlpha(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseAlpha(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAlpha(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseAlpha(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseItemsRejectsBadHeader(t *testing.T) {
	_, err := parseItems(strings.NewReader("instrument,alpha,key,text\nA,0.8,1,B\n"))
	if err == nil {
		t.Fatal("parseItems accepted a 4-column header, want error")
	}
}
